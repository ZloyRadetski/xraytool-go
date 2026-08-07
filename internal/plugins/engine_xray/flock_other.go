//go:build !linux

package engine_xray

import "os"

// On non-Linux platforms, file locking is a no-op.
// The tool is intended to run on Linux; this file exists only to allow
// cross-compilation from Windows/macOS development machines.
func acquireFileLock(_ *os.File) {}
func releaseFileLock(_ *os.File) {}
