package antifraud

import (
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"xraytool/internal/appconfig"
	"xraytool/internal/database"
)

// setupTestDB creates an in-memory SQLite database for testing.
func setupTestDB(t *testing.T) *gorm.DB {
	db, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{
		Logger: nil, // silent
	})
	require.NoError(t, err)

	err = db.AutoMigrate(&database.AntifraudBan{}, &database.Subscription{}, &database.User{})
	require.NoError(t, err)

	return db
}

func TestAnalyzer_Enforce(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		sqlDB, _ := db.DB()
		sqlDB.Close()
	}()

	cfg := &appconfig.Config{
		AntiFraud: appconfig.AntiFraudConf{
			BanDuration: "1h",
		},
	}

	state := newState()
	bs := newBanStore()
	log := slog.Default()

	an := newAnalyzer(cfg, state, bs, nil, 5*time.Minute, 3, db, log)

	email := "badguy@example.com"
	an.enforce(email, "test reason")

	// 1. Verify in-memory ban store updated
	assert.True(t, bs.isBanned(email), "user should be banned in memory")

	// 2. Verify database record created
	var ban database.AntifraudBan
	err := db.Where("email = ?", email).First(&ban).Error
	require.NoError(t, err, "ban record should exist in DB")
	assert.Equal(t, email, ban.Email)
	assert.Equal(t, "test reason", ban.Reason)
	assert.True(t, ban.ExpiresAt.After(time.Now()), "expires_at should be in the future")
}

func TestAnalyzer_HandleEvent(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		sqlDB, _ := db.DB()
		sqlDB.Close()
	}()

	cfg := &appconfig.Config{
		AntiFraud: appconfig.AntiFraudConf{
			BanDuration: "1h",
		},
	}

	state := newState()
	bs := newBanStore()
	log := slog.Default()

	// maxIPs = 2
	an := newAnalyzer(cfg, state, bs, nil, 5*time.Minute, 2, db, log)

	email := "user@x.com"

	// 1st IP
	an.handleEvent(event{email: email, ip: "1.1.1.1"})
	assert.False(t, bs.isBanned(email))

	// 2nd IP
	an.handleEvent(event{email: email, ip: "2.2.2.2"})
	assert.False(t, bs.isBanned(email), "exactly at limit should not ban")

	// 3rd IP -> triggers ban
	an.handleEvent(event{email: email, ip: "3.3.3.3"})
	assert.True(t, bs.isBanned(email), "over limit should trigger ban")

	// Verify DB record
	var ban database.AntifraudBan
	err := db.Where("email = ?", email).First(&ban).Error
	require.NoError(t, err)
}

func TestUnbanCleaner_ProcessExpired(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		sqlDB, _ := db.DB()
		sqlDB.Close()
	}()

	// 1. Create a user and subscription so Ghost User protection passes
	db.Create(&database.User{ID: "user-1", IsBlocked: false})
	db.Create(&database.Subscription{
		ID:     "sub-1",
		UserID: "user-1",
		Email:  "expired@x.com",
		Status: "active",
	})

	// 2. Create an EXPIRED ban
	past := time.Now().Add(-1 * time.Hour)
	db.Create(&database.AntifraudBan{
		Email:     "expired@x.com",
		BannedAt:  past.Add(-1 * time.Hour),
		ExpiresAt: past,
		Reason:    "old ban",
	})

	// 3. Create an ACTIVE ban
	future := time.Now().Add(1 * time.Hour)
	db.Create(&database.AntifraudBan{
		Email:     "active@x.com",
		BannedAt:  time.Now(),
		ExpiresAt: future,
		Reason:    "active ban",
	})

	cfg := &appconfig.Config{}
	bs := newBanStore()
	bs.setBan("expired@x.com", past)
	bs.setBan("active@x.com", future)
	log := slog.Default()

	uc := newUnbanCleaner(cfg, bs, db, log)

	// Run processExpired
	uc.processExpired()

	// 1. Expired ban should be removed from DB and memory
	assert.False(t, bs.isBanned("expired@x.com"))
	var count int64
	db.Model(&database.AntifraudBan{}).Where("email = ?", "expired@x.com").Count(&count)
	assert.Equal(t, int64(0), count, "expired DB record should be deleted")

	// 2. Active ban should remain
	assert.True(t, bs.isBanned("active@x.com"))
	db.Model(&database.AntifraudBan{}).Where("email = ?", "active@x.com").Count(&count)
	assert.Equal(t, int64(1), count, "active DB record should remain")
}
