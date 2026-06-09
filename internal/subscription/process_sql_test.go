package subscription

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"xraytool/internal/appconfig"
	"xraytool/internal/database"
	"xraytool/internal/events"
)

func setupSQLProcessTestEnv(t *testing.T) (*gorm.DB, *CacheManager, func()) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open sqlite: %v", err)
	}

	if err := db.AutoMigrate(&database.User{}, &database.Subscription{}, &database.Device{}); err != nil {
		t.Fatalf("failed to migrate: %v", err)
	}

	user := database.User{
		ID:        "user-uuid-1",
		Username:  "testuser",
		Balance:   100,
		Metadata:  database.Metadata{"telegram_id": int64(12345)},
		CreatedAt: time.Now(),
	}
	db.Create(&user)

	_, cfg, cleanup := setupProcessTestEnv(t)
	cfg.Subscription = appconfig.SubscriptionConf{
		UserAgentWhitelist: []string{"happ", "incy", "megasupersecretua", "v2ray"},
		UserAgentNoChecks:  []string{"megasupersecretua", "v2ray"},
		DummyConfigs: appconfig.DummyConfigsConf{
			Expired:           []string{"🛑 ПОДПИСКА ЗАКОНЧИЛАСЬ 🛑"},
			DeviceLimit:       []string{"🛑 Лимит устройств 🛑"},
			UnsupportedClient: []string{"🛑 Приложение не поддерживается 🛑"},
		},
	}

	sub := database.Subscription{
		ID:         "sub-uuid-1",
		UserID:     "user-uuid-1",
		Email:      "user1", // using user1 to match cache_test data if needed
		XrayUUID:   "xray-uuid-1",
		Status:     "active",
		MaxDevices: 3,
		EndsAt:     nil, // no expiry
		CreatedAt:  time.Now(),
	}
	db.Create(&sub)

	cm := NewCacheManager(cfg)
	cm.Refresh()

	return db, cm, cleanup
}

func TestProcessSQL_Normal(t *testing.T) {
	db, cm, cleanup := setupSQLProcessTestEnv(t)
	defer cleanup()

	dispatcher := events.NewDispatcher(cm.cfg)

	req := &Request{
		RemoteAddr: "1.1.1.1",
		UserAgent:  "happ/1000",
		Query:      map[string]string{"id": "xray-uuid-1"},
		Headers: map[string]string{
			"Host":   "sub.example.com",
			"X-Hwid": "device1",
		},
	}

	res := ProcessSQL(db, cm, dispatcher, req)

	if res.StatusCode != 200 {
		t.Errorf("Expected status 200, got %d. Body: %s", res.StatusCode, res.Body)
	}

	if res.Body == "" {
		t.Errorf("Expected non-empty body")
	}

	var count int64
	db.Model(&database.Device{}).Where("hw_id = ?", "device1").Count(&count)
	if count != 1 {
		t.Errorf("Expected 1 device, got %d", count)
	}
}

func TestProcessSQL_DeviceLimit(t *testing.T) {
	db, cm, cleanup := setupSQLProcessTestEnv(t)
	defer cleanup()

	dispatcher := events.NewDispatcher(cm.cfg)

	// User sub-uuid-1 has limit 3. Let's create 3 devices.
	for i := 1; i <= 3; i++ {
		db.Create(&database.Device{
			SubscriptionID: "sub-uuid-1", // maps to sub's primary key
			HWID:           fmt.Sprintf("dev-%d", i),
			DeviceModel:    "test",
		})
	}

	req := &Request{
		RemoteAddr: "1.1.1.1",
		UserAgent:  "happ/1000",
		Query:      map[string]string{"id": "xray-uuid-1"},
		Headers: map[string]string{
			"Host":   "sub.example.com",
			"X-Hwid": "device-new-4",
		},
	}

	res := ProcessSQL(db, cm, dispatcher, req)

	if res.StatusCode != 200 {
		t.Errorf("Expected status 200, got %d", res.StatusCode)
	}
	if !strings.Contains(res.Body, "Лимит устройств") {
		t.Errorf("Expected device limit dummy text, got: %s", res.Body)
	}
}
