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
		if err := copyOwnership(fi, tmp); err != nil {
			// Non-fatal, just continue
		}
	}

	if err := osRename(tmp, path); err != nil {
		// Fallback for Docker bind mounts or situations where rename fails (e.g. device or resource busy)
		// This fallback is NOT atomic, but it's the only way to write to a bind-mounted file.
		outf, writeErr := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC|os.O_CREATE, perm)
		if writeErr != nil {
			os.Remove(tmp)
			return fmt.Errorf("rename failed (%v) and fallback open failed: %w", err, writeErr)
		}
		if _, writeErr := outf.Write(data); writeErr != nil {
			outf.Close()
			os.Remove(tmp)
			return fmt.Errorf("rename failed (%v) and fallback write failed: %w", err, writeErr)
		}
		if syncErr := outf.Sync(); syncErr != nil {
			outf.Close()
			os.Remove(tmp)
			return fmt.Errorf("rename failed (%v) and fallback sync failed: %w", err, syncErr)
		}
		outf.Close()
	}
	os.Remove(tmp)

	return nil
}
