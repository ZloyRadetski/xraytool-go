package database

import (
	"fmt"

	"log"
	"os"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)




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

// NewConnection opens a database connection and runs AutoMigrate for all models.
func NewConnection(cfg Config) (*gorm.DB, error) {
	var gormDB *gorm.DB
	var err error

	// Customize GORM logger to avoid spamming "record not found" and queries in production
	gormLogger := logger.New(
		log.New(os.Stdout, "\r\n", log.LstdFlags),
		logger.Config{
			SlowThreshold:             time.Second,
			LogLevel:                  logger.Warn,
			IgnoreRecordNotFoundError: true,
			Colorful:                  false,
		},
	)

	gormConfig := &gorm.Config{
		Logger: gormLogger,
	}
	if cfg.Silent {
		gormConfig.Logger = logger.Default.LogMode(logger.Silent)
	}

	if cfg.Driver == "postgres" {
		gormDB, err = gorm.Open(postgresDialector(cfg.DSN), gormConfig)
	} else {
		gormDB, err = gorm.Open(sqliteDialector(cfg.SQLitePath), gormConfig)
	}

	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	if cfg.AutoMigrate {
		if err := autoMigrate(gormDB); err != nil {
			return nil, fmt.Errorf("failed to auto migrate: %w", err)
		}
	}

	if cfg.Driver == "sqlite" {
		if sqlDB, err := gormDB.DB(); err == nil {
			sqlDB.SetMaxOpenConns(1)
		}
	}

	return gormDB, nil
}


// AutoMigrateAll performs GORM schema migrations and sets up indexes.




func autoMigrate(db *gorm.DB) error {
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
	}

	return nil
}