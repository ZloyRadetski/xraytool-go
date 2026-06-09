//go:build !linux && !darwin

package safeio

import (
	"os"
)

func copyOwnership(srcInfo os.FileInfo, dstPath string) error {
	// Not supported on this OS
	return nil
}
