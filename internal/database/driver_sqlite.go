package database

import (
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// sqliteDialector returns a GORM dialector for a SQLite database at the given path.
// Uses the pure-Go glebarez/sqlite driver — no CGO required.
func sqliteDialector(path string) gorm.Dialector {
	if path == "" {
		path = "/etc/xraytool/xraytool.db"
	}
	return sqlite.Open(path + "?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)")
}
