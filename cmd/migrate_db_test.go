package cmd

import (
	"os"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"xraytool/internal/database"
)

func TestMigrateData_InMemory(t *testing.T) {
	// 1. Create source in-memory DB
	srcDB, _ := gorm.Open(sqlite.Open("file:mem1?mode=memory&cache=shared"), &gorm.Config{})
	srcDB.AutoMigrate(&legacyUser{}, &legacySubscription{}, &legacyServer{}, &legacyPayment{})

	// Add some data
	srcDB.Create(&legacyUser{TgID: 100, Name: "A", Username: "a", Balance: 50, MaxDevices: 2})
	srcDB.Create(&legacySubscription{TgID: 100, Status: "active"})

	// 2. Create target in-memory DB. Must be a different DB so schema doesn't conflict!
	dstDB, _ := gorm.Open(sqlite.Open("file:mem2?mode=memory&cache=shared"), &gorm.Config{})
	dstDB.AutoMigrate(&database.User{}, &database.Subscription{})

	// 3. Migrate
	if err := migrateData(srcDB, dstDB); err != nil {
		t.Fatalf("migration failed: %v", err)
	}

	// 4. Verify target DB
	var u database.User
	if err := dstDB.First(&u).Error; err != nil {
		t.Fatalf("user not migrated: %v", err)
	}
	if u.Username != "A" || u.Balance != 50 {
		t.Errorf("wrong user data: %v", u)
	}

	var s database.Subscription
	if err := dstDB.First(&s, "user_id = ?", u.ID).Error; err != nil {
		t.Fatalf("subscription not migrated: %v", err)
	}
	if s.Status != "active" || s.MaxDevices != 2 {
		t.Errorf("wrong subscription data: %v", s)
	}
}

func TestMigrateData_RealBotDB(t *testing.T) {
	sourcePath := `C:\Dev\SERVER\bot.db`
	if _, err := os.Stat(sourcePath); os.IsNotExist(err) {
		t.Skip("Real bot.db not found, skipping integration test")
	}

	srcDB, err := gorm.Open(sqlite.Open(sourcePath), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("failed to open real bot.db: %v", err)
	}

	dstDB, _ := gorm.Open(sqlite.Open("file:mem3?mode=memory&cache=shared"), &gorm.Config{})
	dstDB.AutoMigrate(&database.User{}, &database.Subscription{})

	if err := migrateData(srcDB, dstDB); err != nil {
		t.Fatalf("real migration failed: %v", err)
	}

	var count int64
	dstDB.Model(&database.User{}).Count(&count)
	if count == 0 {
		t.Errorf("expected >0 users migrated")
	}
	t.Logf("Successfully migrated %d users from real bot.db", count)
}
