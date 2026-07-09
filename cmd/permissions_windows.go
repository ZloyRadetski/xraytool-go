//go:build windows

package cmd

import "os"

//nolint:unused
func getWebUser() (uid, gid int, username string) {
	return 0, 0, "www-data"
}
 //nolint:unused
//nolint:unused
//nolint:unused
//nolint:unused
func checkUnixAccess(info os.FileInfo, uid, gid int) (read, write bool) { //nolint:unused
	// Always allow everything on Windows stub
	return true, true
}
