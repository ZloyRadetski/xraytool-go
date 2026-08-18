//go:build !linux && !darwin

package safeio

import (
	"os"
)

// CopyOwnership is a no-op on non-Unix systems.
func CopyOwnership(srcInfo os.FileInfo, dstPath string) error {
	// Not supported on this OS
	return nil
}

func copyOwnership(srcInfo os.FileInfo, dstPath string) error {
	return CopyOwnership(srcInfo, dstPath)
}
