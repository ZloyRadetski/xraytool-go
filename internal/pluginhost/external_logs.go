package pluginhost

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const externalPluginLogTailBytes = 256 << 10 // 256 KiB per external plugin.

const externalPluginLogFileBytes = 2 << 20 // 2 MiB per external plugin.

// logTail is a bounded, concurrency-safe stderr tail. Keeping it in memory
// makes `plugin logs` useful without allowing a noisy subprocess to consume
// unbounded host memory. It intentionally stores raw plugin output; operators
// remain responsible for not logging secrets from their plugin implementation.
type logTail struct {
	mu    sync.RWMutex
	limit int
	data  []byte
}

func newLogTail(limit int) *logTail {
	if limit <= 0 {
		limit = externalPluginLogTailBytes
	}
	return &logTail{limit: limit}
}

func (t *logTail) Write(data []byte) (int, error) {
	if t == nil || len(data) == 0 {
		return len(data), nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.data = append(t.data, data...)
	if overflow := len(t.data) - t.limit; overflow > 0 {
		t.data = append([]byte(nil), t.data[overflow:]...)
		if lineEnd := bytes.IndexByte(t.data, '\n'); lineEnd >= 0 {
			t.data = t.data[lineEnd+1:]
		}
	}
	return len(data), nil
}

func (t *logTail) lines(maxLines int) []string {
	if t == nil {
		return nil
	}
	t.mu.RLock()
	copyData := append([]byte(nil), t.data...)
	t.mu.RUnlock()
	lines := strings.Split(strings.ReplaceAll(string(copyData), "\r\n", "\n"), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if maxLines > 0 && len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return lines
}

// externalLogSink keeps the in-process tail used by Host.ExternalLogs and a
// small on-disk tail used by the standalone `xraytool plugin logs` command.
// A failure to open the file must never make a third-party plugin fail to
// start, so persistent writes are intentionally best-effort.
type externalLogSink struct {
	tail *logTail
	path string

	mu   sync.Mutex
	file *os.File
}

func newExternalLogSink(pluginName, configuredPath string) *externalLogSink {
	path := strings.TrimSpace(configuredPath)
	if path == "" {
		path, _ = ExternalLogPath(pluginName)
	}
	return &externalLogSink{
		tail: newLogTail(externalPluginLogTailBytes),
		path: path,
	}
}

func (s *externalLogSink) Write(data []byte) (int, error) {
	if s == nil {
		return len(data), nil
	}
	_, _ = s.tail.Write(data)
	if len(data) == 0 || s.path == "" {
		return len(data), nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writeFileLocked(data); err != nil {
		// go-plugin treats failures of its stderr writer as process failures.
		// Disk logging is observability only, therefore retain the in-memory
		// tail and never propagate a filesystem error to the subprocess.
		return len(data), nil
	}
	return len(data), nil
}

func (s *externalLogSink) writeFileLocked(data []byte) error {
	if s.file == nil {
		if err := os.MkdirAll(filepath.Dir(s.path), 0750); err != nil {
			return err
		}
		file, err := os.OpenFile(s.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0640)
		if err != nil {
			return err
		}
		s.file = file
	}
	info, err := s.file.Stat()
	if err != nil {
		_ = s.file.Close()
		s.file = nil
		return err
	}
	if info.Size()+int64(len(data)) > externalPluginLogFileBytes {
		if err := s.file.Close(); err != nil {
			s.file = nil
			return err
		}
		file, err := os.OpenFile(s.path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0640)
		if err != nil {
			s.file = nil
			return err
		}
		s.file = file
	}
	_, err = s.file.Write(data)
	return err
}

func (s *externalLogSink) lines(maxLines int) []string {
	if s == nil {
		return nil
	}
	return s.tail.lines(maxLines)
}

func (s *externalLogSink) close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return nil
	}
	err := s.file.Close()
	s.file = nil
	return err
}

// ExternalLogPath returns the default persistent log path for an external
// plugin. Set XRAYTOOL_PLUGIN_LOG_DIR to place logs in a service-managed
// directory (for example /var/log/xraytool/plugins). Individual plugin
// entries may override this with log_path.
func ExternalLogPath(pluginName string) (string, error) {
	name := strings.TrimSpace(pluginName)
	if name == "" || filepath.Base(name) != name || strings.ContainsAny(name, `\\/:`) {
		return "", fmt.Errorf("invalid external plugin name %q for log path", pluginName)
	}
	dir := strings.TrimSpace(os.Getenv("XRAYTOOL_PLUGIN_LOG_DIR"))
	if dir == "" {
		cacheDir, err := os.UserCacheDir()
		if err != nil {
			cacheDir = os.TempDir()
		}
		dir = filepath.Join(cacheDir, "xraytool", "plugins")
	}
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("XRAYTOOL_PLUGIN_LOG_DIR must be an absolute path, got %q", dir)
	}
	return filepath.Join(dir, name+".log"), nil
}

var _ io.Writer = (*externalLogSink)(nil)
