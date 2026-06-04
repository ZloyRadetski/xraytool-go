package xrayconfig

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// processLock protects config reads/writes within a single process.
var processLock sync.RWMutex

// lockFilePath returns the path for the advisory lock file.
func lockFilePath(configPath string) string {
	return configPath + ".lock"
}

// Read reads and parses the xray config.json, acquiring a shared (read) lock.
func Read(path string) (RawConfig, error) {
	processLock.RLock()
	defer processLock.RUnlock()
	return readRaw(path)
}

// Modify reads the config, calls fn to transform it, then writes it back
// atomically. Acquires an exclusive lock for the entire operation.
func Modify(path string, fn func(RawConfig) error) error {
	processLock.Lock()
	defer processLock.Unlock()

	// Advisory file lock (cross-process safety on Linux).
	lf, lockErr := openLockFile(lockFilePath(path))
	if lockErr != nil {
		fmt.Fprintf(os.Stderr, "[WARN] xrayconfig: не удалось открыть lock-файл: %v\n", lockErr)
	} else {
		acquireFileLock(lf)
		defer func() {
			releaseFileLock(lf)
			lf.Close()
		}()
	}

	cfg, err := readRaw(path)
	if err != nil {
		return err
	}
	if err := fn(cfg); err != nil {
		return err
	}
	return writeRaw(path, cfg)
}

// Write atomically writes the config. Prefer Modify when possible.
func Write(path string, cfg RawConfig) error {
	processLock.Lock()
	defer processLock.Unlock()
	return writeRaw(path, cfg)
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

func readRaw(path string) (RawConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading xray config %q: %w", path, err)
	}
	var cfg RawConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing xray config %q: %w", path, err)
	}
	return cfg, nil
}

func writeRaw(path string, cfg RawConfig) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling xray config: %w", err)
	}

	// Preserve original file permissions (fallback to 0644)
	mode := os.FileMode(0644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode()
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "config.json.tmp.*")
	if err != nil {
		return fmt.Errorf("creating temp config: %w", err)
	}
	tmpPath := tmp.Name()

	// Set permissions of the temp file before renaming
	if err := os.Chmod(tmpPath, mode); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("setting temp config permissions: %w", err)
	}

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("writing temp config: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("syncing temp config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("closing temp config: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("replacing config (rename): %w", err)
	}
	return nil
}

func openLockFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		// Fallback to /tmp.
		return os.OpenFile("/tmp/xraytool_config.lock", os.O_CREATE|os.O_RDWR, 0o644)
	}
	return f, nil
}
