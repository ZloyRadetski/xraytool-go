//go:build linux || darwin

package safeio

import (
	"os"
	"syscall"
)

func copyOwnership(srcInfo os.FileInfo, dstPath string) error {
	if stat, ok := srcInfo.Sys().(*syscall.Stat_t); ok {
		return os.Chown(dstPath, int(stat.Uid), int(stat.Gid))
	}
	return nil
}
