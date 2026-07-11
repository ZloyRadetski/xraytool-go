package logger

import (
	"bytes"
	json "github.com/goccy/go-json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"xraytool/internal/appconfig"
)

func TestLoggerInitializationAndLevels(t *testing.T) {
	tmpFile := filepath.Join(t.TempDir(), "test.log")
	cfg := &appconfig.Config{
		Logging: appconfig.LoggingConf{
			Level:    "warn",
			FilePath: tmpFile,
			Format:   "console",
		},
	}

	err := Init(cfg)
	if err != nil {
		t.Fatalf("Init() failed: %v", err)
	}

	if defaultLogger.level != LevelWarn {
		t.Errorf("expected level LevelWarn, got %v", defaultLogger.level)
	}

	if !LevelEnabled(LevelError) {
		t.Errorf("LevelError should be enabled")
	}
	if LevelEnabled(LevelInfo) {
		t.Errorf("LevelInfo should NOT be enabled")
	}

	// Write logs
	Debugf("debug message")
	Infof("info message")
	Warnf("warn message")
	Errorf("error message")

	// Flush and read
	if defaultLogger.file != nil {
		defaultLogger.file.Sync() //nolint:errcheck
	}
	Close()

	content, err := os.ReadFile(tmpFile)
	if err != nil {
		t.Fatalf("Failed to read log file: %v", err)
	}

	logStr := string(content)
	if strings.Contains(logStr, "debug message") {
		t.Errorf("log contains debug message but level is warn")
	}
	if strings.Contains(logStr, "info message") {
		t.Errorf("log contains info message but level is warn")
	}
	if !strings.Contains(logStr, "warn message") {
		t.Errorf("log missing warn message")
	}
	if !strings.Contains(logStr, "error message") {
		t.Errorf("log missing error message")
	}
}

func TestLoggerJSONFormat(t *testing.T) {
	var buf bytes.Buffer
	defaultLogger.mu.Lock()
	defaultLogger.level = LevelInfo
	defaultLogger.format = "json"
	defaultLogger.out = &buf
	defaultLogger.mu.Unlock()

	Infof("test json log %d", 42)

	output := buf.String()
	if !strings.HasPrefix(output, "{") {
		t.Errorf("Expected JSON output, got: %s", output)
	}

	var m map[string]string
	if err := json.Unmarshal([]byte(output), &m); err != nil {
		t.Fatalf("Failed to parse JSON: %v", err)
	}

	if m["level"] != "INFO" {
		t.Errorf("Expected level INFO, got %v", m["level"])
	}
	if m["message"] != "test json log 42" {
		t.Errorf("Unexpected message: %v", m["message"])
	}
}
