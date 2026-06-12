package xrayconfig

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"xraytool/internal/safeio"
)

// processLock protects config reads/writes within a single process.
var processLock sync.RWMutex

// lockFilePath returns the path for the advisory lock file.
func lockFilePath(configPath string) string {
	return configPath + ".lock"
}

func Read(path string) (RawConfig, error) {
	processLock.RLock()
	defer processLock.RUnlock()

	lf, lockErr := openLockFile(lockFilePath(path))
	if lockErr != nil {
		fmt.Fprintf(os.Stderr, "[WARN] xrayconfig Read: не удалось открыть lock-файл: %v\n", lockErr)
	} else {
		acquireFileLock(lf)
		defer func() {
			releaseFileLock(lf)
			lf.Close()
		}()
	}

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

func Write(path string, cfg RawConfig) error {
	processLock.Lock()
	defer processLock.Unlock()

	lf, lockErr := openLockFile(lockFilePath(path))
	if lockErr != nil {
		fmt.Fprintf(os.Stderr, "[WARN] xrayconfig Write: не удалось открыть lock-файл: %v\n", lockErr)
	} else {
		acquireFileLock(lf)
		defer func() {
			releaseFileLock(lf)
			lf.Close()
		}()
	}

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

	if err := safeio.WriteToFile(path, data, 0o644); err != nil {
		return fmt.Errorf("writing xray config: %w", err)
	}
	return nil
}

func openLockFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		// Fallback to os.TempDir(). Use a hash of the path to avoid cross-config lock contention
		h := sha256.Sum256([]byte(path))
		fallbackName := fmt.Sprintf("xraytool_config_%x.lock", h[:8])
		return os.OpenFile(filepath.Join(os.TempDir(), fallbackName), os.O_CREATE|os.O_RDWR, 0o644)
	}
	return f, nil
}
