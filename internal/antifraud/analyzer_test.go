package antifraud

import (
	"fmt"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"xraytool/internal/appconfig"
	"xraytool/internal/database"
)

// setupTestDB creates an isolated SQLite database for testing.
// Each call gets its own temp-file DB to prevent cross-test contamination.
func setupTestDB(t testing.TB) *gorm.DB {
	t.Helper()
	// Use a unique file per test; TempDir is cleaned up automatically.
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{
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

func TestAnalyzer_HandleEvent_DefaultOneDevice(t *testing.T) {
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

	// maxIPs = 2, no subscription in DB → device limit defaults to 1 → threshold = 2*1 = 2
	an := newAnalyzer(cfg, state, bs, nil, 5*time.Minute, 2, db, log)

	email := "user@x.com"

	// 1st IP
	an.handleEvent(event{email: email, ip: "1.1.1.1"})
	assert.False(t, bs.isBanned(email))

	// 2nd IP — exactly at limit, no ban
	an.handleEvent(event{email: email, ip: "2.2.2.2"})
	assert.False(t, bs.isBanned(email), "exactly at limit should not ban")

	// 3rd IP → triggers ban (count 3 > threshold 2)
	an.handleEvent(event{email: email, ip: "3.3.3.3"})
	assert.True(t, bs.isBanned(email), "over limit should trigger ban")

	// Verify DB record
	var ban database.AntifraudBan
	err := db.Where("email = ?", email).First(&ban).Error
	require.NoError(t, err)
}

func TestAnalyzer_HandleEvent_MultiDevice(t *testing.T) {
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

	// Create a subscription with MaxDevices = 2
	email := "multidev@x.com"
	db.Create(&database.User{ID: "u-1"})
	db.Create(&database.Subscription{
		ID:         "sub-1",
		UserID:     "u-1",
		Email:      email,
		Status:     "active",
		MaxDevices: 2,
	})

	state := newState()
	bs := newBanStore()
	log := slog.Default()

	// maxIPs = 3, MaxDevices = 2 → dynamic threshold = 3 * 2 = 6
	an := newAnalyzer(cfg, state, bs, nil, 5*time.Minute, 3, db, log)

	// 6 unique IPs — must NOT trigger ban (at limit)
	for i := 1; i <= 6; i++ {
		an.handleEvent(event{email: email, ip: generateIP(i)})
	}
	assert.False(t, bs.isBanned(email), "6 IPs at threshold 6 should not ban")

	// 7th IP — must trigger ban
	an.handleEvent(event{email: email, ip: generateIP(7)})
	assert.True(t, bs.isBanned(email), "7th IP over threshold 6 should trigger ban")
}

func TestAnalyzer_GetDeviceLimit_Fallback(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		sqlDB, _ := db.DB()
		sqlDB.Close()
	}()

	cfg := &appconfig.Config{}
	an := newAnalyzer(cfg, newState(), newBanStore(), nil, time.Minute, 3, db, slog.Default())

	// User not in DB — should return 1 (safe fallback)
	limit := an.getDeviceLimit("unknown@example.com")
	assert.Equal(t, 1, limit, "unknown user should default to 1 device")
}

func TestAnalyzer_GetDeviceLimit_CacheHit(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		sqlDB, _ := db.DB()
		sqlDB.Close()
	}()

	email := "cached@x.com"
	db.Create(&database.User{ID: "u-2"})
	db.Create(&database.Subscription{
		ID:         "sub-2",
		UserID:     "u-2",
		Email:      email,
		Status:     "active",
		MaxDevices: 5,
	})

	cfg := &appconfig.Config{}
	an := newAnalyzer(cfg, newState(), newBanStore(), nil, time.Minute, 3, db, slog.Default())

	// First call: DB miss → fetches from DB and caches
	limit := an.getDeviceLimit(email)
	assert.Equal(t, 5, limit)

	// Verify it's now in cache
	an.deviceCache.mu.RLock()
	cached, ok := an.deviceCache.limits[email]
	an.deviceCache.mu.RUnlock()
	assert.True(t, ok, "value should be cached after first lookup")
	assert.Equal(t, 5, cached)

	// Second call: cache hit (no extra DB query)
	limit2 := an.getDeviceLimit(email)
	assert.Equal(t, 5, limit2)
}

func TestAnalyzer_RefreshDeviceCache(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		sqlDB, _ := db.DB()
		sqlDB.Close()
	}()

	db.Create(&database.User{ID: "u-3"})
	db.Create(&database.Subscription{
		ID: "sub-3", UserID: "u-3",
		Email: "refresh@x.com", Status: "active", MaxDevices: 4,
	})

	cfg := &appconfig.Config{}
	an := newAnalyzer(cfg, newState(), newBanStore(), nil, time.Minute, 3, db, slog.Default())

	an.refreshDeviceCache()

	an.deviceCache.mu.RLock()
	limit, ok := an.deviceCache.limits["refresh@x.com"]
	an.deviceCache.mu.RUnlock()

	assert.True(t, ok)
	assert.Equal(t, 4, limit)
}

func TestFraudReason(t *testing.T) {
	reason := fraudReason("user@x.com", 7, 6, 3, 2, 3*time.Minute)
	assert.Contains(t, reason, "7 unique IPs")
	assert.Contains(t, reason, "limit 6 = 3 base × 2 devices")
	assert.Contains(t, reason, "3m0s window")
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

// generateIP produces a deterministic IP string for testing.
func generateIP(n int) string {
	return fmt.Sprintf("10.0.0.%d", n)
}
