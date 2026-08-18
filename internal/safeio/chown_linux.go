//go:build linux || darwin

package safeio

import (
	"os"
	"syscall"
)

// CopyOwnership copies the UID and GID from srcInfo to dstPath on Unix systems.
func CopyOwnership(srcInfo os.FileInfo, dstPath string) error {
	if stat, ok := srcInfo.Sys().(*syscall.Stat_t); ok {
		return os.Chown(dstPath, int(stat.Uid), int(stat.Gid))
	}
	return nil
}

func copyOwnership(srcInfo os.FileInfo, dstPath string) error {
	return CopyOwnership(srcInfo, dstPath)
}
