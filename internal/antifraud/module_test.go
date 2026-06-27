package antifraud

import (
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"xraytool/internal/appconfig"
	"xraytool/internal/database"
)

func TestModule_IsBannedAndForceUnban(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		sqlDB, _ := db.DB()
		sqlDB.Close()
	}()

	cfg := &appconfig.Config{}
	log := slog.Default()
	m := New(cfg, db, log)

	// Create an active ban in the DB
	email := "test@example.com"
	db.Create(&database.AntifraudBan{
		Email:     email,
		Reason:    "test ban",
		BannedAt:  time.Now(),
		ExpiresAt: time.Now().Add(24 * time.Hour),
	})

	// Before loading, IsBanned should be false
	assert.False(t, m.IsBanned(email))

	// Load DB bans (similar to what Run() does on startup)
	m.recoverBansFromDB()

	// After loading, IsBanned should be true
	assert.True(t, m.IsBanned(email))

	// Now ForceUnban
	m.ForceUnban(email)

	// Should be unbanned in memory
	assert.False(t, m.IsBanned(email))

	// Should be deleted from DB
	var count int64
	db.Model(&database.AntifraudBan{}).Where("email = ?", email).Count(&count)
	assert.Equal(t, int64(0), count)
}

func TestBanStore(t *testing.T) {
	bs := newBanStore()
	email := "test@example.com"
	
	// Not banned initially
	assert.False(t, bs.isBanned(email))

	// Set ban in future
	future := time.Now().Add(1 * time.Hour)
	bs.setBan(email, future)
	assert.True(t, bs.isBanned(email))

	// Set ban in past (expired)
	past := time.Now().Add(-1 * time.Hour)
	bs.setBan(email, past)
	assert.False(t, bs.isBanned(email))

	// Clear ban
	bs.setBan(email, future)
	bs.clearBan(email)
	assert.False(t, bs.isBanned(email))
}
