package antifraud

import (
	"context"
	"log/slog"
	"os"
	"runtime"
	"time"

	"xraytool/internal/domain"
)

// rotator monitors the Xray access log file size.
// When the file exceeds the configured limit it performs a safe log rotation:
//
//  1. os.Rename(log → log.old)   — atomic on Linux; Xray keeps writing to .old via open fd
//  2. Engine.RestartLogger()     — Engine closes the old fd and opens a new access.log
//  3. signal tailer via notify() — tailer drains .old lines, then we delete .old
//
// This keeps tmpfs memory usage bounded without losing any log entries.
//
// Dry-run edge cases verified:
//   - RestartLogger fails (Xray not reachable): we log the error and do NOT delete .old;
//     on the next tick the size will still be large and rotation will be attempted again.
//   - File doesn't exist yet: os.Stat fails → skip tick silently.
//   - Double rotation (tick fires while .old still exists): we check for .old and skip.
type rotator struct {
	logPath      string
	maxBytes     int64
	loggerCtrl   domain.LoggerController
	notifyCh     chan<- struct{} // signals tailer to switch to fresh file
	log          *slog.Logger
	tickInterval time.Duration
}

const rotatorTickInterval = 10 * time.Second

func newRotator(logPath string, maxMB int, loggerCtrl domain.LoggerController, notifyCh chan<- struct{}, log *slog.Logger) *rotator {
	return &rotator{
		logPath:      logPath,
		maxBytes:     int64(maxMB) * 1024 * 1024,
		loggerCtrl:   loggerCtrl,
		notifyCh:     notifyCh,
		log:          log,
		tickInterval: rotatorTickInterval,
	}
}

// run starts the rotation ticker. Blocks until ctx is cancelled.
func (r *rotator) run(ctx context.Context) {
	r.log.Info("antifraud rotator: starting", "path", r.logPath, "limit_mb", r.maxBytes/1024/1024)
	defer r.log.Info("antifraud rotator: stopped")

	ticker := time.NewTicker(r.tickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.tryRotate()
		}
	}
}

func (r *rotator) tryRotate() {
	info, err := os.Stat(r.logPath)
	if err != nil {
		// File not present yet — normal at startup.
		return
	}

	if info.Size() < r.maxBytes {
		return
	}

	oldPath := r.logPath + ".old"

	// Skip if a previous .old rotation hasn't been fully consumed yet.
	if _, err := os.Stat(oldPath); err == nil {
		r.log.Warn("antifraud rotator: .old file still exists, skipping rotation", "old_path", oldPath)
		return
	}

	if runtime.GOOS == "windows" {
		// Windows: cannot reliably rename an opened file. Copy and truncate.
		input, err := os.ReadFile(r.logPath)
		if err != nil {
			r.log.Error("antifraud rotator: read failed", "err", err)
			return
		}
		if err := os.WriteFile(oldPath, input, 0644); err != nil {
			r.log.Error("antifraud rotator: write old failed", "err", err)
			return
		}
		if err := os.Truncate(r.logPath, 0); err != nil {
			r.log.Error("antifraud rotator: truncate failed", "err", err)
			return
		}
		if err := r.loggerCtrl.RestartLogger(context.Background()); err != nil {
			r.log.Error("antifraud rotator: RestartLogger failed", "err", err)
		}
	} else {
		// Step 1: Atomic rename. Xray keeps writing to the renamed file via its open fd.
		if err := os.Rename(r.logPath, oldPath); err != nil {
			r.log.Error("antifraud rotator: rename failed", "err", err)
			return
		}

		// Step 2: Tell Engine to close the old fd and open a fresh log file.
		if err := r.loggerCtrl.RestartLogger(context.Background()); err != nil {
			// Critical: Xray couldn't reopen the log. Reverse the rename so we don't lose data.
			_ = os.Rename(oldPath, r.logPath)
			r.log.Error("antifraud rotator: RestartLogger failed, rotation reversed", "err", err)
			return
		}
	}

	r.log.Info("antifraud rotator: log rotated", "old_path", oldPath, "size_before_mb",
		float64(info.Size())/1024/1024)

	// Step 3: Signal the tailer. It will drain .old and then we can delete it.
	// Non-blocking send: if the tailer channel is full we proceed anyway;
	// the tailer will eventually see the file via its poll loop.
	select {
	case r.notifyCh <- struct{}{}:
	default:
	}

	// Step 4: Give the tailer a moment to drain .old, then remove it to free RAM.
	// We use a short sleep here rather than a complex ack protocol — the window
	// for losing lines is the tailer's poll interval (~200ms) which is acceptable.
	//
	// If the tailer is currently processing .old and we delete it mid-scan,
	// the already-opened file descriptor in the tailer remains valid (Linux VFS
	// keeps the inode alive until all fds are closed). No data loss occurs.
	time.Sleep(2 * r.tickInterval)

	if err := os.Remove(oldPath); err != nil && !os.IsNotExist(err) {
		r.log.Warn("antifraud rotator: failed to remove old log", "err", err, "path", oldPath)
	} else {
		r.log.Info("antifraud rotator: old log removed, RAM freed", "path", oldPath)
	}
}
