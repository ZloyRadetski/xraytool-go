//go:build windows

package cmd

import "os"

func getWebUser() (uid, gid int, username string) {
	return 0, 0, "www-data"
}

func checkUnixAccess(info os.FileInfo, uid, gid int) (read, write bool) {
	// Always allow everything on Windows stub
	return true, true
}
