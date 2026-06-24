package database

import (
	"path/filepath"
	"testing"
)

func TestInitAndDB(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "test.db")

	err := Init(Config{
		Driver:      "sqlite",
		SQLitePath:  dbPath,
		Silent:      true,
		AutoMigrate: true,
	})
	if err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	if !IsReady() {
		t.Errorf("IsReady() returned false")
	}

	d := DB()
	if d == nil {
		t.Errorf("DB() returned nil")
	}

	err = Close()
	if err != nil {
		t.Errorf("Close failed: %v", err)
	}

	if IsReady() {
		t.Errorf("IsReady() returned true after Close()")
	}

	// Test panic
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("DB() did not panic after Close()")
		}
	}()
	_ = DB()
}

func TestInitInvalidDriver(t *testing.T) {
	err := Init(Config{
		Driver: "invalid",
	})
	if err == nil {
		t.Errorf("expected error for invalid driver")
	}
}
