package logger

import (
	"fmt"
	json "github.com/goccy/go-json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"xraytool/internal/appconfig"
)

type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

func (l Level) String() string {
	switch l {
	case LevelDebug:
		return "DEBUG"
	case LevelInfo:
		return "INFO"
	case LevelWarn:
		return "WARN"
	case LevelError:
		return "ERROR"
	default:
		return "INFO"
	}
}

type Logger struct {
	mu     sync.Mutex
	level  Level
	format string
	out    io.Writer
	file   *os.File
}

var (
	defaultLogger = &Logger{
		level:  LevelInfo,
		format: "console",
		out:    os.Stdout,
	}
)

func Init(cfg *appconfig.Config) error {
	defaultLogger.mu.Lock()
	defer defaultLogger.mu.Unlock()

	// Parse level
	switch strings.ToLower(cfg.Logging.Level) {
	case "debug":
		defaultLogger.level = LevelDebug
	case "info":
		defaultLogger.level = LevelInfo
	case "warn", "warning":
		defaultLogger.level = LevelWarn
	case "error":
		defaultLogger.level = LevelError
	default:
		defaultLogger.level = LevelInfo
	}

	// Parse format
	if strings.ToLower(cfg.Logging.Format) == "json" {
		defaultLogger.format = "json"
	} else {
		defaultLogger.format = "console"
	}

	// Close previous file if any
	if defaultLogger.file != nil {
		defaultLogger.file.Close()
		defaultLogger.file = nil
	}

	// Setup output destination
	if cfg.Logging.FilePath != "" {
		dir := filepath.Dir(cfg.Logging.FilePath)
		if err := os.MkdirAll(dir, 0755); err != nil {
			defaultLogger.out = os.Stdout
			return fmt.Errorf("failed to create log directory: %w", err)
		}

		f, err := os.OpenFile(cfg.Logging.FilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			defaultLogger.out = os.Stdout
			return fmt.Errorf("failed to open log file: %w", err)
		}

		defaultLogger.file = f
		defaultLogger.out = f
	} else {
		defaultLogger.out = os.Stdout
	}

	return nil
}

func (l *Logger) log(level Level, format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if level < l.level {
		return
	}

	now := time.Now()
	msg := fmt.Sprintf(format, args...)

	if l.format == "json" {
		logEntry := map[string]string{
			"time":    now.UTC().Format(time.RFC3339),
			"level":   level.String(),
			"message": msg,
		}
		if data, err := json.Marshal(logEntry); err == nil {
			fmt.Fprintln(l.out, string(data))
		}
	} else {
		timeStr := now.Format("2006-01-02 15:04:05")
		fmt.Fprintf(l.out, "%s [%s] %s\n", timeStr, level.String(), msg)
	}
}

func Debugf(format string, args ...interface{}) {
	defaultLogger.log(LevelDebug, format, args...)
}

func Infof(format string, args ...interface{}) {
	defaultLogger.log(LevelInfo, format, args...)
}

func Warnf(format string, args ...interface{}) {
	defaultLogger.log(LevelWarn, format, args...)
}

func Errorf(format string, args ...interface{}) {
	defaultLogger.log(LevelError, format, args...)
}

func LevelEnabled(level Level) bool {
	defaultLogger.mu.Lock()
	defer defaultLogger.mu.Unlock()
	return level >= defaultLogger.level
}

func Close() {
	defaultLogger.mu.Lock()
	defer defaultLogger.mu.Unlock()
	if defaultLogger.file != nil {
		defaultLogger.file.Close()
		defaultLogger.file = nil
		defaultLogger.out = os.Stdout
	}
}
