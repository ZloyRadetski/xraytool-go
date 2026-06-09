package safeio

import (
	"fmt"
	"os"
	"path/filepath"
)

// WriteToFile atomically writes data to a file at path, preserving its permissions
// and ownership if it already exists. If it does not exist, it creates it with
// defaultPerm.
func WriteToFile(path string, data []byte, defaultPerm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}

	perm := defaultPerm
	var fi os.FileInfo
	fi, err := os.Stat(path)
	exists := err == nil
	if exists {
		perm = fi.Mode().Perm()
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return fmt.Errorf("write tmp: %w", err)
	}

	// Make sure permissions match exactly (os.WriteFile applies umask, so we must chmod)
	if err := os.Chmod(tmp, perm); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("chmod tmp: %w", err)
	}

	if exists {
		// Attempt to preserve ownership (only effective on Linux/Unix)
		if err := copyOwnership(fi, tmp); err != nil {
			// Non-fatal, just continue
		}
	}

	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename: %w", err)
	}

	return nil
}
