package worker

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"xraytool/internal/appconfig"
	"xraytool/internal/database"
)

// setupTestDB creates an SQLite database in a temp directory and runs AutoMigrate.
func setupTestDB(t *testing.T) *gorm.DB {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{})
	if err != nil {
		t.Fatalf("Failed to open test database: %v", err)
	}

	sqlDB, err := db.DB()
	if err == nil {
		t.Cleanup(func() { sqlDB.Close() })
	}

	err = db.AutoMigrate(&database.User{}, &database.Subscription{}, &database.SubscriptionNotification{}, &database.Device{})
	if err != nil {
		t.Fatalf("Failed to migrate test database: %v", err)
	}

	return db
}

func setupTestWorker(t *testing.T, db *gorm.DB, cfg *appconfig.Config) *ExpiryWorker {
	// Dummy logger
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Ensure dummy file paths for xrayconfig to not fail outright
	tmpDir := t.TempDir()
	if cfg.Paths.XrayConfig == "" {
		cfg.Paths.XrayConfig = filepath.Join(tmpDir, "config.json")
		os.WriteFile(cfg.Paths.XrayConfig, []byte(`{"inbounds":[]}`), 0644)
	}

	return NewExpiryWorker(db, cfg, nil, nil, logger)
}

func TestExpiryWorker_ProcessOnce_Expired(t *testing.T) {
	db := setupTestDB(t)

	// Create an expired subscription
	endsAt := time.Now().Add(-1 * time.Hour)
	sub := database.Subscription{
		ID:       "sub-1",
		UserID:   "user-1",
		XrayUUID: "uuid-sub-1",
		Email:    "test@example.com",
		Status: "active",
		EndsAt: &endsAt,
	}
	db.Create(&sub)

	cfg := &appconfig.Config{}
	worker := setupTestWorker(t, db, cfg)

	worker.ProcessOnce()

	var updatedSub database.Subscription
	db.First(&updatedSub, "id = ?", "sub-1")

	if updatedSub.Status != "expired" {
		t.Errorf("Expected status to be 'expired', got '%s'", updatedSub.Status)
	}
}

func TestExpiryWorker_ProcessOnce_Warnings(t *testing.T) {
	db := setupTestDB(t)

	// Create a subscription expiring in 12 hours
	endsAt := time.Now().Add(12 * time.Hour)
	sub := database.Subscription{
		ID:       "sub-2",
		UserID:   "user-1",
		XrayUUID: "uuid-sub-2",
		Email:    "test2@example.com",
		Status: "active",
		EndsAt: &endsAt,
	}
	db.Create(&sub)

	cfg := &appconfig.Config{
		Worker: appconfig.WorkerConf{
			ExpirationWarnings: []string{"24h", "6h"},
		},
	}
	worker := setupTestWorker(t, db, cfg)

	// First run should trigger the 24h warning, but not the 6h warning.
	worker.ProcessOnce()

	var notifs []database.SubscriptionNotification
	db.Find(&notifs)

	if len(notifs) != 1 {
		t.Fatalf("Expected exactly 1 notification, got %d", len(notifs))
	}
	if notifs[0].WarningLevel != "24h" {
		t.Errorf("Expected warning level '24h', got '%s'", notifs[0].WarningLevel)
	}

	// Second run should NOT trigger anything new because 24h is already recorded
	// and we are not yet within the 6h window.
	worker.ProcessOnce()

	db.Find(&notifs)
	if len(notifs) != 1 {
		t.Fatalf("Expected exactly 1 notification after second run, got %d", len(notifs))
	}

	// Now modify EndsAt to be within 5 hours, to trigger the 6h warning.
	endsAtNew := time.Now().Add(5 * time.Hour)
	db.Model(&sub).Update("ends_at", endsAtNew)

	worker.ProcessOnce()

	db.Find(&notifs)
	if len(notifs) != 2 {
		t.Fatalf("Expected exactly 2 notifications after third run, got %d", len(notifs))
	}

	found6h := false
	for _, n := range notifs {
		if n.WarningLevel == "6h" {
			found6h = true
		}
	}
	if !found6h {
		t.Errorf("Expected to find a '6h' warning, but didn't")
	}
}

func TestExpiryWorker_InvalidDuration(t *testing.T) {
	db := setupTestDB(t)

	endsAt := time.Now().Add(12 * time.Hour)
	sub := database.Subscription{
		ID:       "sub-invalid",
		UserID:   "user-1",
		XrayUUID: "uuid-sub-invalid",
		Email:    "test-inv@example.com",
		Status:   "active",
		EndsAt:   &endsAt,
	}
	db.Create(&sub)

	// "3 apples" is invalid
	cfg := &appconfig.Config{
		Worker: appconfig.WorkerConf{
			ExpirationWarnings: []string{"3 apples", "24h"},
		},
	}
	worker := setupTestWorker(t, db, cfg)

	worker.ProcessOnce()

	var notifs []database.SubscriptionNotification
	db.Find(&notifs)

	// It should skip "3 apples" and process "24h"
	if len(notifs) != 1 {
		t.Fatalf("Expected exactly 1 notification, got %d", len(notifs))
	}
	if notifs[0].WarningLevel != "24h" {
		t.Errorf("Expected warning level '24h', got '%s'", notifs[0].WarningLevel)
	}
}

func TestExpiryWorker_IgnoreUnlimitedAndBlocked(t *testing.T) {
	db := setupTestDB(t)

	// User 1: Active, but EndsAt is NULL (Unlimited)
	db.Create(&database.Subscription{
		ID:       "sub-unlim",
		UserID:   "user-unlim",
		XrayUUID: "uuid-sub-unlim",
		Email:    "unlim@example.com",
		Status:   "active",
		EndsAt:   nil,
	})

	// User 2: Blocked, EndsAt is in the past
	endsAtPast := time.Now().Add(-1 * time.Hour)
	db.Create(&database.Subscription{
		ID:       "sub-block",
		UserID:   "user-block",
		XrayUUID: "uuid-sub-block",
		Email:    "block@example.com",
		Status:   "blocked", // NOT active
		EndsAt:   &endsAtPast,
	})

	cfg := &appconfig.Config{
		Worker: appconfig.WorkerConf{
			ExpirationWarnings: []string{"24h"},
		},
	}
	worker := setupTestWorker(t, db, cfg)
	worker.ProcessOnce()

	var unlimSub database.Subscription
	db.First(&unlimSub, "id = ?", "sub-unlim")
	if unlimSub.Status != "active" {
		t.Errorf("Unlimited sub status changed to '%s'", unlimSub.Status)
	}

	var blockSub database.Subscription
	db.First(&blockSub, "id = ?", "sub-block")
	if blockSub.Status != "blocked" {
		t.Errorf("Blocked sub status changed to '%s'", blockSub.Status)
	}
}

func TestExpiryWorker_EmptyWarningsConfig(t *testing.T) {
	db := setupTestDB(t)

	endsAt := time.Now().Add(12 * time.Hour)
	db.Create(&database.Subscription{
		ID:       "sub-1",
		UserID:   "user-1",
		XrayUUID: "uuid-sub-1-b",
		Email:    "test@example.com",
		Status:   "active",
		EndsAt:   &endsAt,
	})

	// Empty array of warnings
	cfg := &appconfig.Config{
		Worker: appconfig.WorkerConf{
			ExpirationWarnings: []string{},
		},
	}
	worker := setupTestWorker(t, db, cfg)
	worker.ProcessOnce()

	var notifs []database.SubscriptionNotification
	db.Find(&notifs)
	if len(notifs) != 0 {
		t.Errorf("Expected 0 notifications with empty config, got %d", len(notifs))
	}
}

func TestExpiryWorker_UnorderedConfigAndMultipleHits(t *testing.T) {
	db := setupTestDB(t)

	// Only 2 hours left
	endsAt := time.Now().Add(2 * time.Hour)
	sub := database.Subscription{
		ID:       "sub-1",
		UserID:   "user-1",
		XrayUUID: "uuid-sub-1-c",
		Email:    "unordered@example.com",
		Status:   "active",
		EndsAt:   &endsAt,
	}
	db.Create(&sub)

	// Admin made a mess in config: unordered
	cfg := &appconfig.Config{
		Worker: appconfig.WorkerConf{
			ExpirationWarnings: []string{"1h", "72h", "24h", "3h"},
		},
	}
	worker := setupTestWorker(t, db, cfg)

	// First run should trigger 72h, 24h, and 3h simultaneously (since 2h < all of them)
	// But it should NOT trigger 1h.
	worker.ProcessOnce()

	var notifs []database.SubscriptionNotification
	db.Find(&notifs)

	if len(notifs) != 3 {
		t.Fatalf("Expected exactly 3 notifications, got %d", len(notifs))
	}

	// Verify which ones got triggered
	triggered := map[string]bool{}
	for _, n := range notifs {
		triggered[n.WarningLevel] = true
	}

	if !triggered["72h"] || !triggered["24h"] || !triggered["3h"] {
		t.Errorf("Expected 72h, 24h, and 3h to be triggered, got: %v", triggered)
	}
	if triggered["1h"] {
		t.Errorf("1h should NOT have been triggered")
	}
}

func TestExpiryWorker_BatchMixedUsers(t *testing.T) {
	db := setupTestDB(t)

	now := time.Now()
	endsExpired := now.Add(-1 * time.Hour)
	endsSoon := now.Add(10 * time.Hour)
	endsLate := now.Add(100 * time.Hour)

	db.Create(&database.Subscription{ID: "sub-exp", UserID: "u1", XrayUUID: "uuid1", Email: "exp@x.com", Status: "active", EndsAt: &endsExpired})
	db.Create(&database.Subscription{ID: "sub-soon", UserID: "u2", XrayUUID: "uuid2", Email: "soon@x.com", Status: "active", EndsAt: &endsSoon})
	db.Create(&database.Subscription{ID: "sub-late", UserID: "u3", XrayUUID: "uuid3", Email: "late@x.com", Status: "active", EndsAt: &endsLate})

	cfg := &appconfig.Config{
		Worker: appconfig.WorkerConf{
			ExpirationWarnings: []string{"24h"},
		},
	}
	worker := setupTestWorker(t, db, cfg)
	worker.ProcessOnce()

	// 1. sub-exp should be expired
	var subExp database.Subscription
	db.First(&subExp, "id = ?", "sub-exp")
	if subExp.Status != "expired" {
		t.Errorf("sub-exp should be expired, got %s", subExp.Status)
	}

	// 2. sub-soon should be active AND have a 24h notification
	var subSoon database.Subscription
	db.First(&subSoon, "id = ?", "sub-soon")
	if subSoon.Status != "active" {
		t.Errorf("sub-soon should be active, got %s", subSoon.Status)
	}
	var notifs []database.SubscriptionNotification
	db.Where("subscription_id = ?", "sub-soon").Find(&notifs)
	if len(notifs) != 1 || notifs[0].WarningLevel != "24h" {
		t.Errorf("sub-soon should have 1 notification for '24h', got %v", notifs)
	}

	// 3. sub-late should be active AND have 0 notifications
	var subLate database.Subscription
	db.First(&subLate, "id = ?", "sub-late")
	if subLate.Status != "active" {
		t.Errorf("sub-late should be active, got %s", subLate.Status)
	}
	db.Where("subscription_id = ?", "sub-late").Find(&notifs)
	if len(notifs) != 0 {
		t.Errorf("sub-late should have 0 notifications, got %d", len(notifs))
	}
}
