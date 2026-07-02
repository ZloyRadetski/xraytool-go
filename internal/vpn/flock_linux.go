//go:build linux

package vpn

import (
	"os"
	"syscall"
)

func acquireFileLock(f *os.File) {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if err == nil {
			return
		}
		if err == syscall.EINTR {
			continue
		}
		// For other errors (NFS, ENOTSUP, etc.) log and return — advisory lock only.
		return
	}
}

func releaseFileLock(f *os.File) {
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck
}
