package safeio

import (
	"fmt"
	"os"
	"path/filepath"
)

var osRename = os.Rename

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

	f, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create tmp: %w", err)
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("sync tmp: %w", err)
	}
	f.Close()

	// Make sure permissions match exactly (os.WriteFile applies umask, so we must chmod)
	if err := os.Chmod(tmp, perm); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("chmod tmp: %w", err)
	}

	if exists {
		// Attempt to preserve ownership (only effective on Linux/Unix)
		if err := copyOwnership(fi, tmp); err != nil { //nolint:staticcheck //nolint:staticcheck
			// Non-fatal, just continue
		}
	}

	if err := osRename(tmp, path); err != nil {
		// The temp file is always created in the same directory as the target
		// (via os.CreateTemp(dir, ...)), so os.Rename never crosses filesystem
		// boundaries and must not fail with EXDEV. Any other rename failure is
		// a real I/O error — abort and leave the original file intact.
		os.Remove(tmp)
		return fmt.Errorf("atomic rename %q → %q: %w", tmp, path, err)
	}

	return nil
}
