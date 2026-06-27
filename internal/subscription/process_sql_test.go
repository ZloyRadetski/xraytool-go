package subscription

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"xraytool/internal/appconfig"
	"xraytool/internal/database"
	"xraytool/internal/events"
)

func setupTestDB(t *testing.T) *gorm.DB {
	db, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{
		Logger: nil,
	})
	require.NoError(t, err)

	err = db.AutoMigrate(&database.User{}, &database.Subscription{})
	require.NoError(t, err)

	return db
}

func TestProcessSQL_AntiFraudBan(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		sqlDB, _ := db.DB()
		sqlDB.Close()
		os.Remove("test_xray_config_af.json")
	}()

	os.WriteFile("test_xray_config_af.json", []byte(`{}`), 0644)

	// 1. Create a valid subscription
	db.Create(&database.User{ID: "u1", Username: "u1", IsBlocked: false})
	sub := database.Subscription{
		ID:         "sub1",
		UserID:     "u1",
		Email:      "banned@example.com",
		Status:     "active",
		XrayUUID:   "11111111-2222-3333-4444-555555555555",
		MaxDevices: 5,
	}
	db.Create(&sub)

	cfg := &appconfig.Config{
		Paths: appconfig.PathsConf{
			XrayConfig: "test_xray_config_af.json",
		},
		Subscription: appconfig.SubscriptionConf{
			UserAgentWhitelist: []string{"testclient"},
			DummyConfigs: appconfig.DummyConfigsConf{
				AntiFraud: []string{"YOU_ARE_BANNED"},
			},
		},
	}

	req := &Request{
		Query:      map[string]string{"id": "sub1"},
		UserAgent:  "TestClient/1.0",
		RemoteAddr: "192.168.1.1",
		Headers:    make(map[string]string),
	}

	cm := NewCacheManager(cfg)

	// 2. Mock isBanned to return TRUE
	isBanned := func(email string) bool {
		return email == "banned@example.com"
	}

	dispatcher := events.NewDispatcher(cfg)

	// 3. Process
	res := ProcessSQL(db, cm, dispatcher, req, isBanned)

	// 4. Verify result is the dummy config
	assert.Equal(t, 200, res.StatusCode, "Should return 200 OK")
	assert.Equal(t, "text/plain; charset=utf-8", res.Headers["Content-Type"])
	assert.Equal(t, "antifraud_ban", res.Headers["X-Reject-Reason"])
	assert.Contains(t, res.Body, "YOU_ARE_BANNED", "Body should contain dummy config")
	assert.NotContains(t, res.Body, "11111111-2222-3333-4444-555555555555", "Real UUID must not leak")
}

func TestProcessSQL_Normal(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		sqlDB, _ := db.DB()
		sqlDB.Close()
		os.Remove("test_xray_config_normal.json")
	}()

	os.WriteFile("test_xray_config_normal.json", []byte(`{}`), 0644)

	// 1. Create a valid subscription
	db.Create(&database.User{ID: "u2", Username: "u2", IsBlocked: false})
	sub := database.Subscription{
		ID:         "sub2",
		UserID:     "u2",
		Email:      "good@example.com",
		Status:     "active",
		XrayUUID:   "11111111-2222-3333-4444-555555555555",
		MaxDevices: 5,
	}
	db.Create(&sub)

	cfg := &appconfig.Config{
		Paths: appconfig.PathsConf{
			XrayConfig: "test_xray_config_normal.json",
		},
		Subscription: appconfig.SubscriptionConf{
			UserAgentWhitelist: []string{"testclient"},
		},
	}

	req := &Request{
		Query:      map[string]string{"id": "sub2"},
		UserAgent:  "TestClient/1.0",
		RemoteAddr: "192.168.1.2",
		Headers:    make(map[string]string),
	}

	cm := NewCacheManager(cfg)

	// 2. Mock isBanned to return FALSE
	isBanned := func(email string) bool {
		return false
	}

	dispatcher := events.NewDispatcher(cfg)

	// 3. Process
	res := ProcessSQL(db, cm, dispatcher, req, isBanned)

	// 4. Verify result is NOT the dummy config, but real generation
	// Note: generating real configs will fail inside ProcessSQL because xray_config.json is missing,
	// or it will hit cache logic. Since we just want to ensure it doesn't return antifraud reject:
	assert.NotEqual(t, "antifraud_ban", res.Headers["X-Reject-Reason"])
}
