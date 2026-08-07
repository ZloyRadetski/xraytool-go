package support_chat

import (
	"fmt"
	"os"
	"path/filepath"

	"gorm.io/driver/postgres"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func openDB(cfg DBConfig) (*gorm.DB, error) {
	var dialector gorm.Dialector

	switch cfg.Driver {
	case "sqlite":
		if err := os.MkdirAll(filepath.Dir(cfg.SQLitePath), 0755); err != nil {
			return nil, fmt.Errorf("failed to create sqlite dir: %w", err)
		}
		// Enable foreign keys for sqlite
		dialector = sqlite.Open(cfg.SQLitePath + "?_fk=1")
	case "postgres":
		dialector = postgres.Open(cfg.DSN)
	default:
		return nil, fmt.Errorf("unsupported driver: %s", cfg.Driver)
	}

	db, err := gorm.Open(dialector, &gorm.Config{
		Logger: logger.Default.LogMode(logger.Warn),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// AutoMigrate tables
	if err := db.AutoMigrate(&Conversation{}, &Message{}); err != nil {
		return nil, fmt.Errorf("failed to migrate tables: %w", err)
	}

	return db, nil
}
