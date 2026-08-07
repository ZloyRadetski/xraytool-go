package database

import (
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// legacySubscriptionForUUIDMigration models the schema used before
// Subscription.UUID replaced the Xray-specific column name.
type legacySubscriptionForUUIDMigration struct {
	ID         string `gorm:"type:text;primaryKey"`
	UserID     string `gorm:"type:text;not null;index"`
	Email      string `gorm:"type:text;not null;uniqueIndex"`
	LegacyUUID string `gorm:"column:xray_uuid;type:text;not null;uniqueIndex"`
	Status     string `gorm:"type:text;not null;default:'inactive';index"`
	MaxDevices int    `gorm:"default:3;not null"`
}

func (legacySubscriptionForUUIDMigration) TableName() string {
	return subscriptionTable
}

func TestAutoMigrateRenamesLegacySubscriptionUUIDColumn(t *testing.T) {
	db := openLegacySubscriptionDB(t, false)

	if err := autoMigrate(db); err != nil {
		t.Fatalf("auto-migrate legacy database: %v", err)
	}
	assertMigratedSubscriptionUUID(t, db)
}

func TestAutoMigrateCompletesPartialSubscriptionUUIDMigration(t *testing.T) {
	db := openLegacySubscriptionDB(t, true)

	if err := autoMigrate(db); err != nil {
		t.Fatalf("auto-migrate partially migrated database: %v", err)
	}
	assertMigratedSubscriptionUUID(t, db)
}

func TestMigrateSubscriptionUUIDColumnRejectsConflictingPartialMigration(t *testing.T) {
	db := openLegacySubscriptionDB(t, true)
	if err := db.Exec("UPDATE subscriptions SET uuid = ?", "different-client-uuid").Error; err != nil {
		t.Fatalf("seed conflicting uuid column: %v", err)
	}

	err := migrateSubscriptionUUIDColumn(db)
	if err == nil {
		t.Fatal("expected conflicting UUID migration to fail")
	}

	if !db.Migrator().HasColumn(&Subscription{}, legacySubscriptionUUIDColumn) {
		t.Fatal("conflicting migration unexpectedly removed legacy column")
	}
}

func openLegacySubscriptionDB(t *testing.T, addUUIDColumn bool) *gorm.DB {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "legacy.db")
	db, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open legacy database: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("get legacy database handle: %v", err)
	}
	t.Cleanup(func() {
		_ = sqlDB.Close()
	})
	if err := db.AutoMigrate(&legacySubscriptionForUUIDMigration{}); err != nil {
		t.Fatalf("create legacy subscriptions table: %v", err)
	}
	if err := db.Create(&legacySubscriptionForUUIDMigration{
		ID:         "legacy-subscription",
		UserID:     "legacy-user",
		Email:      "legacy@example.test",
		LegacyUUID: "legacy-client-uuid",
		Status:     "active",
	}).Error; err != nil {
		t.Fatalf("insert legacy subscription: %v", err)
	}
	if addUUIDColumn {
		if err := db.Exec("ALTER TABLE subscriptions ADD COLUMN uuid text").Error; err != nil {
			t.Fatalf("add intermediate uuid column: %v", err)
		}
	}

	return db
}

func assertMigratedSubscriptionUUID(t *testing.T, db *gorm.DB) {
	t.Helper()

	if db.Migrator().HasColumn(&Subscription{}, legacySubscriptionUUIDColumn) {
		t.Fatalf("legacy %s column still exists", legacySubscriptionUUIDColumn)
	}
	if !db.Migrator().HasColumn(&Subscription{}, subscriptionUUIDColumn) {
		t.Fatalf("new %s column was not created", subscriptionUUIDColumn)
	}

	var sub Subscription
	if err := db.First(&sub, "id = ?", "legacy-subscription").Error; err != nil {
		t.Fatalf("read migrated subscription: %v", err)
	}
	if sub.UUID != "legacy-client-uuid" {
		t.Fatalf("migrated UUID = %q, want legacy value", sub.UUID)
	}

	duplicate := Subscription{
		ID:     "duplicate-subscription",
		UserID: "another-user",
		Email:  "another@example.test",
		UUID:   sub.UUID,
		Status: "active",
	}
	if err := db.Create(&duplicate).Error; err == nil {
		t.Fatal("expected migrated UUID unique constraint to reject a duplicate")
	}
}
