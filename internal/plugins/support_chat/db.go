package support_chat

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func openDB(cfg DBConfig) (*gorm.DB, error) {
	var dialector gorm.Dialector

	switch cfg.Driver {
	case "sqlite":
		localFile := cfg.SQLitePath != ":memory:" && !strings.HasPrefix(cfg.SQLitePath, "file:")
		if localFile {
			if err := os.MkdirAll(filepath.Dir(cfg.SQLitePath), 0700); err != nil {
				return nil, fmt.Errorf("failed to create sqlite dir: %w", err)
			}
			if err := os.Chmod(filepath.Dir(cfg.SQLitePath), 0700); err != nil && !os.IsNotExist(err) {
				return nil, fmt.Errorf("protect sqlite dir: %w", err)
			}
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
	if err := db.AutoMigrate(&Conversation{}, &Message{}, &Attachment{}); err != nil {
		return nil, fmt.Errorf("failed to auto migrate: %w", err)
	}
	if cfg.Driver == "sqlite" && cfg.SQLitePath != ":memory:" && !strings.HasPrefix(cfg.SQLitePath, "file:") {
		if err := os.Chmod(cfg.SQLitePath, 0600); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("protect sqlite database: %w", err)
		}
	}

	return db, nil
}
