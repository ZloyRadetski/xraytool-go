package antifraud

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"time"
)

// event represents a parsed connection event from the Xray access log.
// Both fields are guaranteed non-empty by the parser before sending.
type event struct {
	email string
	ip    string
}

// --- Parser constants ---
//
// Xray access log format (accepted line example):
//   2024/01/15 12:34:56 accepted tcp:1.2.3.4:12345 [inbound] email
//
// We look for lines containing " accepted " and extract:
//   - The source address field (index after "accepted") → strip port → IP
//   - The last whitespace-delimited token                → Email
//
// All operations work on []byte to avoid string allocations.

var (
	acceptedTag = []byte(" accepted ")
)

// parseLine extracts (ip, email) from a single Xray access log line.
// Returns ("", "") if the line is not an accepted-connection entry.
//
// Zero-allocation contract: no strings are created; we return sub-slices
// of the input buf. Callers must copy if they need the values to outlive buf.
//
// Xray actual log format:
// 2024/01/15 12:34:56 1.2.3.4:56789 accepted tcp:8.8.8.8:443 [inbound] email
func parseLine(line []byte) (ip, email []byte) {
	idx := bytes.Index(line, acceptedTag)
	if idx < 0 {
		return nil, nil
	}

	// 1. Extract Source IP (the token immediately BEFORE " accepted ")
	beforeAccepted := bytes.TrimSpace(line[:idx])
	lastSpaceIdx := bytes.LastIndexByte(beforeAccepted, ' ')
	if lastSpaceIdx < 0 {
		return nil, nil
	}
	srcAddrField := beforeAccepted[lastSpaceIdx+1:] // e.g. "1.2.3.4:56789"

	// Strip port to get raw IP
	rawIP := stripPort(srcAddrField)
	if len(rawIP) == 0 {
		return nil, nil
	}

	// 2. Extract Email (the LAST token on the entire line)
	// Example: "tcp:8.8.8.8:443 [inbound] email"
	rest := line[idx+len(acceptedTag):]
	
	// Xray logs always have at least "[tag] email" or "[tag]" after the destination address.
	// If there's no space in rest, it means it just ends with "tcp:8.8.8.8:443", which is not an email.
	if bytes.IndexByte(rest, ' ') < 0 {
		return nil, nil
	}

	emailField := lastToken(rest)
	if len(emailField) == 0 {
		return nil, nil
	}

	// Newer Xray versions prefix the email with "email: " (e.g., "... email: bot_client").
	// We handle this cleanly since `lastToken` already splits by space, but just in case
	// it appears as `email:bot_client` (without space), we trim it.
	emailField = bytes.TrimPrefix(emailField, []byte("email:"))

	return rawIP, emailField
}

// stripPort removes the port suffix from an address field.
// Handles both IPv4 (1.2.3.4:port) and IPv6 ([::1]:port or ::1).
func stripPort(addr []byte) []byte {
	if len(addr) == 0 {
		return nil
	}
	// IPv6 with brackets: [::1]:port
	if addr[0] == '[' {
		end := bytes.IndexByte(addr, ']')
		if end < 0 {
			return nil
		}
		return addr[1:end]
	}
	// IPv4 or bare IPv6: find last colon
	lastColon := bytes.LastIndexByte(addr, ':')
	if lastColon < 0 {
		// No colon → treat as bare address without port
		return addr
	}
	return addr[:lastColon]
}

// lastToken returns the last whitespace-separated token in b.
func lastToken(b []byte) []byte {
	b = bytes.TrimRight(b, " \t\r\n")
	idx := bytes.LastIndexByte(b, ' ')
	if idx < 0 {
		return b
	}
	return b[idx+1:]
}

// tailer reads an Xray access log file line by line in a tail-like fashion.
// It sends parsed events to the out channel until ctx is cancelled.
//
// Implementation notes:
//   - Uses bufio.Scanner for line-oriented reads.
//   - After EOF, sleeps tailPollInterval and retries (simulates tail -F).
//   - When the Rotator signals via rotateCh, the tailer re-opens the file
//     so it starts reading the fresh log (not the renamed .old file).
//   - goroutine leak check: all goroutines launched here are gated on ctx.Done.
type tailer struct {
	path           string
	out            chan<- event
	rotateCh       <-chan struct{}
	log            *slog.Logger
	pollInterval   time.Duration
}

const tailPollInterval = 200 * time.Millisecond

func newTailer(path string, out chan<- event, rotateCh <-chan struct{}, log *slog.Logger) *tailer {
	return &tailer{
		path:         path,
		out:          out,
		rotateCh:     rotateCh,
		log:          log,
		pollInterval: tailPollInterval,
	}
}

// run starts tailing. It blocks until ctx is cancelled.
// Dry-run edge cases:
//   - File does not exist yet: retries after pollInterval.
//   - File is renamed by Rotator: rotateCh triggers re-open.
//   - Very long lines (>64KB): Scanner returns ErrTooLong; line is skipped.
func (t *tailer) run(ctx context.Context) {
	t.log.Info("antifraud tailer: starting", "path", t.path)
	defer t.log.Info("antifraud tailer: stopped")

	var (
		f      *os.File
		reader *bufio.Scanner
		offset int64
	)

	openFile := func() {
		if f != nil {
			_ = f.Close()
		}
		var err error
		f, err = os.Open(t.path)
		if err != nil {
			f = nil
			reader = nil
			offset = 0
			return
		}
		// Seek to the last known offset so we don't re-process old lines.
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			offset = 0
		}
		reader = bufio.NewScanner(f)
		// Increase the buffer to handle very long log lines without crashing.
		reader.Buffer(make([]byte, 128*1024), 128*1024)
	}

	openFile()

	for {
		select {
		case <-ctx.Done():
			if f != nil {
				_ = f.Close()
			}
			return
		case <-t.rotateCh:
			// Rotator has renamed the old file and created a new one. Re-open.
			offset = 0
			openFile()
		default:
		}

		if reader == nil {
			// File not available yet — wait and retry.
			select {
			case <-ctx.Done():
				return
			case <-time.After(t.pollInterval):
			}
			openFile()
			continue
		}

		scanned := false
		for reader.Scan() {
			scanned = true
			line := reader.Bytes()
			rawIP, rawEmail := parseLine(line)
			if rawIP == nil || rawEmail == nil {
				continue
			}
			// We must copy the sub-slices because the Scanner reuses its buffer.
			e := event{
				email: string(rawEmail),
				ip:    string(rawIP),
			}
			select {
			case t.out <- e:
			case <-ctx.Done():
				if f != nil {
					_ = f.Close()
				}
				return
			}
		}

		if err := reader.Err(); err != nil {
			t.log.Warn("antifraud tailer: scanner error", "err", err)
			// Re-open the file to recover from read errors.
			if pos, err2 := f.Seek(0, io.SeekCurrent); err2 == nil {
				offset = pos
			}
			openFile()
			continue
		}

		if !scanned {
			// Reached EOF without reading any new lines — wait before polling.
			// Save the current offset to resume from here after rotateCh or sleep.
			if f != nil {
				if pos, err := f.Seek(0, io.SeekCurrent); err == nil {
					offset = pos
				}
			}
			select {
			case <-ctx.Done():
				if f != nil {
					_ = f.Close()
				}
				return
			case <-t.rotateCh:
				offset = 0
				openFile()
			case <-time.After(t.pollInterval):
				// Re-open the same file to get fresh data.
				if f != nil {
					if pos, err := f.Seek(0, io.SeekCurrent); err == nil {
						offset = pos
					}
					_ = f.Close()
				}
				openFile()
			}
		}
	}
}
