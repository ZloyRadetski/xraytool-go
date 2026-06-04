//go:build !windows

package cmd

import (
	"fmt"
	"os"
	"os/user"
	"syscall"
)

func getWebUser() (uid, gid int, username string) {
	// Try www-data
	u, err := user.Lookup("www-data")
	if err == nil {
		uidVal := 0
		gidVal := 0
		_, _ = fmt.Sscanf(u.Uid, "%d", &uidVal)
		_, _ = fmt.Sscanf(u.Gid, "%d", &gidVal)
		return uidVal, gidVal, u.Username
	}
	// Try nginx
	u, err = user.Lookup("nginx")
	if err == nil {
		uidVal := 0
		gidVal := 0
		_, _ = fmt.Sscanf(u.Uid, "%d", &uidVal)
		_, _ = fmt.Sscanf(u.Gid, "%d", &gidVal)
		return uidVal, gidVal, u.Username
	}
	// Try apache
	u, err = user.Lookup("apache")
	if err == nil {
		uidVal := 0
		gidVal := 0
		_, _ = fmt.Sscanf(u.Uid, "%d", &uidVal)
		_, _ = fmt.Sscanf(u.Gid, "%d", &gidVal)
		return uidVal, gidVal, u.Username
	}
	// Fallback to standard Debian/Ubuntu www-data UID/GID (33)
	return 33, 33, "www-data"
}

func checkUnixAccess(info os.FileInfo, uid, gid int) (read, write bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false, false
	}

	mode := info.Mode()

	// 1. Root bypass
	if uid == 0 {
		return true, true
	}

	// 2. Owner
	if int(stat.Uid) == uid {
		read = mode&0400 != 0
		write = mode&0200 != 0
		return
	}

	// 3. Group
	if int(stat.Gid) == gid {
		read = mode&0040 != 0
		write = mode&0020 != 0
		return
	}

	// 4. Others
	read = mode&0004 != 0
	write = mode&0002 != 0
	return
}
