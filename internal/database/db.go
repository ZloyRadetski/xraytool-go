package database

import (
	"fmt"
	"sync"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/clause"
)

var (
	dbMutex sync.RWMutex
	db      *gorm.DB
	initErr error
)

// DB returns the global GORM instance.
// Panics if Init has not been called successfully.
func DB() *gorm.DB {
	dbMutex.RLock()
	defer dbMutex.RUnlock()
	if db == nil {
		panic("database: DB() called before Init()")
	}
	return db
}

// IsReady returns true if the database is successfully connected.
func IsReady() bool {
	dbMutex.RLock()
	defer dbMutex.RUnlock()
	return db != nil
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
	// Silent disables GORM logging. Useful for CLI tools.
	Silent bool
	// AutoMigrate enables GORM AutoMigrate on initialization.
	AutoMigrate bool
}

// Init opens the database connection and runs AutoMigrate for all models.
// It is safe to call multiple times — only the first call takes effect.
func Init(cfg Config) error {
	dbMutex.Lock()
	defer dbMutex.Unlock()

	if db != nil {
		return nil // Already initialized successfully
	}

	var dialector gorm.Dialector

	switch cfg.Driver {
	case "sqlite", "":
		dialector = sqliteDialector(cfg.SQLitePath)
	case "postgres":
		dialector = postgresDialector(cfg.DSN)
	default:
		initErr = fmt.Errorf("database: unknown driver %q", cfg.Driver)
		return initErr
	}

	logMode := logger.Warn
	if cfg.Silent {
		logMode = logger.Silent
	}
	
	gormCfg := &gorm.Config{
		// Warn on slow queries; adjust to logger.Info for development verbosity.
		Logger: logger.Default.LogMode(logMode),
	}

	conn, err := gorm.Open(dialector, gormCfg)
	if err != nil {
		initErr = fmt.Errorf("database: connect failed: %w", err)
		return initErr
	}

	if cfg.Driver == "sqlite" || cfg.Driver == "" {
		sqlDB, err := conn.DB()
		if err == nil {
			sqlDB.SetMaxOpenConns(1)
		}
	}

	db = conn

	// AutoMigrate creates or updates tables to match the current model structs.
	// It is intentionally non-destructive: it never drops columns or indexes.
	if cfg.AutoMigrate {
		if err := autoMigrateAllUnsafe(); err != nil {
			initErr = err
			return initErr
		}
	}

	initErr = nil

	// Seed default plans atomically
	defaultPlans := []Plan{
		{Months: 1, BasePrice: 159},
		{Months: 3, BasePrice: 429},
		{Months: 6, BasePrice: 799},
		{Months: 12, BasePrice: 1399},
	}
	db.Clauses(clause.OnConflict{DoNothing: true}).Create(&defaultPlans)

	return nil
}

func Close() error {
	dbMutex.Lock()
	defer dbMutex.Unlock()
	if db == nil {
		return nil
	}
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	db = nil // Ensure we can re-init if needed
	return sqlDB.Close()
}

// AutoMigrateAll performs GORM schema migrations and sets up indexes.
func AutoMigrateAll() error {
	dbMutex.RLock()
	defer dbMutex.RUnlock()
	return autoMigrateAllUnsafe()
}

func autoMigrateAllUnsafe() error {
	if db == nil {
		return fmt.Errorf("database not initialized")
	}

	if err := db.AutoMigrate(
		&User{},
		&Subscription{},
		&Device{},
		&Payment{},
		&ReferralReward{},
		&SubscriptionNotification{},
		&Plan{},
		&PromoCode{},
		&AntifraudBan{},
	); err != nil {
		return fmt.Errorf("database: auto-migrate failed: %w", err)
	}

	if db.Dialector.Name() == "postgres" {
		db.Exec("CREATE INDEX IF NOT EXISTS idx_users_telegram_id ON users ((metadata->>'telegram_id'));")
		db.Exec("CREATE INDEX IF NOT EXISTS idx_users_telegram_username ON users ((metadata->>'telegram_username'));")
	} else if db.Dialector.Name() == "sqlite" {
		db.Exec("CREATE INDEX IF NOT EXISTS idx_users_telegram_id ON users (json_extract(metadata, '$.telegram_id'));")
	}
	return nil
}
