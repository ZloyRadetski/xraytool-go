package database

import (
	"strings"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// sqliteDialector returns a GORM dialector for a SQLite database at the given path.
// Uses the pure-Go glebarez/sqlite driver — no CGO required.
func sqliteDialector(path string) gorm.Dialector {
	if path == "" {
		path = "/etc/xraytool/xraytool.db"
	}
	if !strings.Contains(path, "?") {
		path += "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	}
	return sqlite.Open(path)
}
