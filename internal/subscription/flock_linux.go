//go:build linux

package subscription

import (
	"os"
	"syscall"
)

func acquireFileLock(f *os.File) {
	syscall.Flock(int(f.Fd()), syscall.LOCK_EX) //nolint:errcheck
}

func releaseFileLock(f *os.File) {
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck
}
