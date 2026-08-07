package database

import (
	"fmt"

	"log"
	"os"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const (
	subscriptionTable            = "subscriptions"
	legacySubscriptionUUIDColumn = "xray_uuid"
	subscriptionUUIDColumn       = "uuid"
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
	// Legacy commands still request one monolithic migration pass. Keep that
	// compatibility path, but delegate to the same owner-specific schema
	// functions used by PluginDBHandle so the table ownership cannot drift.
	for _, pluginName := range []string{"core", "antifraud", "cluster_sync"} {
		if err := applyBuiltinPluginSchema(db, pluginName, 1); err != nil {
			return fmt.Errorf("database: auto-migrate %s failed: %w", pluginName, err)
		}
	}
	return nil
}

// migrateSubscriptionUUIDColumn moves the historical xray_uuid column to the
// engine-agnostic uuid name before GORM inspects the current Subscription
// schema. A direct rename preserves every value and its constraints on both
// SQLite and PostgreSQL.
//
// A database left in an intermediate state with both columns is repaired only
// when the two non-empty values agree. Refusing conflicting values is safer
// than silently overwriting an identifier used by an existing VPN client.
func migrateSubscriptionUUIDColumn(db *gorm.DB) error {
	if !db.Migrator().HasTable(&Subscription{}) {
		return nil
	}

	return db.Transaction(func(tx *gorm.DB) error {
		hasLegacy := tx.Migrator().HasColumn(&Subscription{}, legacySubscriptionUUIDColumn)
		if !hasLegacy {
			return nil
		}

		hasUUID := tx.Migrator().HasColumn(&Subscription{}, subscriptionUUIDColumn)
		if !hasUUID {
			if err := tx.Migrator().RenameColumn(&Subscription{}, legacySubscriptionUUIDColumn, subscriptionUUIDColumn); err != nil {
				return fmt.Errorf("database: rename subscriptions.%s to %s: %w", legacySubscriptionUUIDColumn, subscriptionUUIDColumn, err)
			}
			return nil
		}

		var conflicts int64
		if err := tx.Table(subscriptionTable).
			Where("xray_uuid IS NOT NULL AND xray_uuid <> '' AND uuid IS NOT NULL AND uuid <> '' AND uuid <> xray_uuid").
			Count(&conflicts).Error; err != nil {
			return fmt.Errorf("database: check subscriptions UUID migration: %w", err)
		}
		if conflicts > 0 {
			return fmt.Errorf("database: cannot migrate subscriptions UUID column: %d conflicting rows", conflicts)
		}

		if err := tx.Exec("UPDATE subscriptions SET uuid = xray_uuid WHERE (uuid IS NULL OR uuid = '') AND xray_uuid IS NOT NULL").Error; err != nil {
			return fmt.Errorf("database: copy subscriptions.%s to %s: %w", legacySubscriptionUUIDColumn, subscriptionUUIDColumn, err)
		}
		if err := tx.Migrator().DropColumn(&Subscription{}, legacySubscriptionUUIDColumn); err != nil {
			return fmt.Errorf("database: drop legacy subscriptions.%s: %w", legacySubscriptionUUIDColumn, err)
		}
		return nil
	})
}
