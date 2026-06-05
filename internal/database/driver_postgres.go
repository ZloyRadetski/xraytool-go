package database

import (
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// postgresDialector returns a GORM dialector for a Postgres database using the given DSN.
func postgresDialector(dsn string) gorm.Dialector {
	return postgres.Open(dsn)
}
