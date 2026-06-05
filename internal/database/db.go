package database

import (
	"fmt"
	"sync"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var (
	once sync.Once
	db   *gorm.DB
)

// DB returns the global GORM instance.
// Panics if Init has not been called successfully.
func DB() *gorm.DB {
	if db == nil {
		panic("database: DB() called before Init()")
	}
	return db
}

// Config holds the parameters needed to open a database connection.
type Config struct {
	// Driver selects the backend: "postgres" or "sqlite".
	Driver string
	// DSN is the Postgres connection string (used when Driver == "postgres").
	DSN string
	// SQLitePath is the file path (used when Driver == "sqlite").
	// Defaults to /etc/xraytool/xraytool.db if empty.
	SQLitePath string
}

// Init opens the database connection and runs AutoMigrate for all models.
// It is safe to call multiple times — only the first call takes effect.
func Init(cfg Config) error {
	var initErr error
	once.Do(func() {
		var dialector gorm.Dialector

		switch cfg.Driver {
		case "sqlite", "":
			dialector = sqliteDialector(cfg.SQLitePath)
		case "postgres":
			dialector = postgresDialector(cfg.DSN)
		default:
			initErr = fmt.Errorf("database: unknown driver %q", cfg.Driver)
			return
		}

		gormCfg := &gorm.Config{
			// Warn on slow queries; adjust to logger.Info for development verbosity.
			Logger: logger.Default.LogMode(logger.Warn),
		}

		conn, err := gorm.Open(dialector, gormCfg)
		if err != nil {
			initErr = fmt.Errorf("database: connect failed: %w", err)
			return
		}

		// AutoMigrate creates or updates tables to match the current model structs.
		// It is intentionally non-destructive: it never drops columns or indexes.
		if err := conn.AutoMigrate(
			&User{},
			&Subscription{},
			&Device{},
			&Payment{},
			&ReferralReward{},
		); err != nil {
			initErr = fmt.Errorf("database: auto-migrate failed: %w", err)
			return
		}

		db = conn
	})
	return initErr
}

// Close closes the underlying *sql.DB connection pool.
// Safe to call even if Init was never called.
func Close() error {
	if db == nil {
		return nil
	}
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}
