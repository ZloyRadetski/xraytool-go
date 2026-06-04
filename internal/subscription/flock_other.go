//go:build !linux

package subscription

import "os"

func acquireFileLock(_ *os.File) {}
func releaseFileLock(_ *os.File) {}
