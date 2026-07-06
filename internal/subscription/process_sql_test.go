package subscription

import (
	"context"
	"os"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"xraytool/internal/appconfig"
	"xraytool/internal/database"
	"xraytool/internal/events"
	"xraytool/internal/vpn"
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

	cm := NewCacheManager(cfg, &vpn.NoopEngine{})

	// 2. Mock isBanned to return TRUE
	isBanned := func(email string) bool {
		return email == "banned@example.com"
	}

	dispatcher := events.NewDispatcher(&events.Config{Webhooks: []string{}})

	// 3. Process
	res := ProcessSQL(context.Background(), database.NewRegistry(db), cm, dispatcher, req, isBanned)

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

	cm := NewCacheManager(cfg, &vpn.NoopEngine{})

	// 2. Mock isBanned to return FALSE
	isBanned := func(email string) bool {
		return false
	}

	dispatcher := events.NewDispatcher(&events.Config{Webhooks: []string{}})

	// 3. Process
	res := ProcessSQL(context.Background(), database.NewRegistry(db), cm, dispatcher, req, isBanned)

	// 4. Verify result is NOT the dummy config, but real generation
	// Note: generating real configs will fail inside ProcessSQL because xray_config.json is missing,
	// or it will hit cache logic. Since we just want to ensure it doesn't return antifraud reject:
	assert.NotEqual(t, "antifraud_ban", res.Headers["X-Reject-Reason"])
}

func TestProcessSQL_RealityRotationPlaceholders(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		sqlDB, _ := db.DB()
		sqlDB.Close()
		os.Remove("test_xray_config_rr.json")
		os.Remove("test_configs.txt")
	}()

	os.WriteFile("test_xray_config_rr.json", []byte(`{}`), 0644)
	os.WriteFile("test_configs.txt", []byte("# Header\n{\"pbk\": \"{PBK}\", \"sid\": \"{SID}\"}"), 0644)

	db.Create(&database.User{ID: "u3", Username: "u3", IsBlocked: false})
	sub := database.Subscription{
		ID:         "sub3",
		UserID:     "u3",
		Email:      "rr@example.com",
		Status:     "active",
		XrayUUID:   "11111111-2222-3333-4444-555555555555",
		MaxDevices: 5,
	}
	db.Create(&sub)

	cfg := &appconfig.Config{
		Paths: appconfig.PathsConf{
			XrayConfig: "test_xray_config_rr.json",
			JSONSubscriptionTemplate: "test_configs.txt",
		},
		Subscription: appconfig.SubscriptionConf{
			UserAgentWhitelist: []string{"testclient"},
			UserAgentNoChecks:  []string{"testclient"},
		},
		Reality: appconfig.RealityConf{
			RotationEnabled: true,
			KeysFilepath:    "reality.keys",
		},
	}

	req := &Request{
		Query:      map[string]string{"id": "sub3", "hwid": "my-hwid"},
		UserAgent:  "TestClient/1.0",
		RemoteAddr: "192.168.1.3",
		Headers:    make(map[string]string),
	}

	cm := NewCacheManager(cfg, &vpn.NoopEngine{})
	// Mock preloaded Reality keys in CacheManager
	cm.realityKeys = &vpn.RealityKeys{
		PrivateKey: "mock-priv",
		PublicKey:  "mock-pub",
		ShortIDs:   []string{"mock-sid"},
	}
	cm.subTemplate = "# Header\n{\"pbk\": \"{PBK}\", \"sid\": \"{SID}\"}"

	isBanned := func(email string) bool { return false }
	dispatcher := events.NewDispatcher(&events.Config{Webhooks: []string{}})

	res := ProcessSQL(context.Background(), database.NewRegistry(db), cm, dispatcher, req, isBanned)

	assert.Equal(t, 200, res.StatusCode)
	assert.Contains(t, res.Body, `"pbk": "mock-pub"`)
	assert.Contains(t, res.Body, `"sid": "mock-sid"`)
}

func TestProcessSQL_InferredTrafficStats(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		sqlDB, _ := db.DB()
		sqlDB.Close()
	}()

	tempDir := t.TempDir()
	statsStateFile := tempDir + "/stats_state.json"
	inferredStatsFile := tempDir + "/inferred_stats.json"

	// 1. Write stats state JSON (local master traffic)
	statsStateJSON := `{
		"users": {
			"traffic_user@example.com": {
				"cumulative_up": 100,
				"cumulative_down": 200
			},
			"local_only@example.com": {
				"cumulative_up": 50,
				"cumulative_down": 60
			}
		}
	}`
	require.NoError(t, os.WriteFile(statsStateFile, []byte(statsStateJSON), 0644))

	// 2. Write inferred stats JSON (combined master+slave traffic)
	inferredStatsJSON := `{
		"users": {
			"traffic_user@example.com": {
				"cumulative_up": 1000,
				"cumulative_down": 2000
			}
		}
	}`
	require.NoError(t, os.WriteFile(inferredStatsFile, []byte(inferredStatsJSON), 0644))

	os.WriteFile(tempDir+"/xray_config.json", []byte(`{}`), 0644)
	os.WriteFile(tempDir+"/configs.txt", []byte("# Header\n{\"email\": \"{EMAIL}\", \"up\": {UP}, \"down\": {DOWN}}"), 0644)

	db.Create(&database.User{ID: "u_traffic", Username: "u_traffic", RefCode: "ref_traffic", IsBlocked: false})
	db.Create(&database.Subscription{
		ID:         "sub_traffic",
		UserID:     "u_traffic",
		Email:      "traffic_user@example.com",
		Status:     "active",
		XrayUUID:   "00000000-0000-0000-0000-000000000001",
		MaxDevices: 3,
	})

	db.Create(&database.User{ID: "u_local", Username: "u_local", RefCode: "ref_local", IsBlocked: false})
	db.Create(&database.Subscription{
		ID:         "sub_local",
		UserID:     "u_local",
		Email:      "local_only@example.com",
		Status:     "active",
		XrayUUID:   "00000000-0000-0000-0000-000000000002",
		MaxDevices: 3,
	})

	cfg := &appconfig.Config{
		Paths: appconfig.PathsConf{
			XrayConfig:               tempDir + "/xray_config.json",
			JSONSubscriptionTemplate: tempDir + "/configs.txt",
			StatsState:               statsStateFile,
			InferredStats:            inferredStatsFile,
		},
		Subscription: appconfig.SubscriptionConf{
			UserAgentWhitelist: []string{"testclient"},
			UserAgentNoChecks:  []string{"testclient"},
		},
	}

	cm := NewCacheManager(cfg, &vpn.NoopEngine{})
	cm.subTemplate = "# Header\n{\"email\": \"{EMAIL}\", \"up\": {UP}, \"down\": {DOWN}}"
	isBanned := func(email string) bool { return false }
	dispatcher := events.NewDispatcher(&events.Config{Webhooks: []string{}})

	// Scenario A: User exists in InferredStats -> should return inferred stats (1000/2000)
	reqA := &Request{
		Query:      map[string]string{"id": "sub_traffic", "hwid": "my-hwid"},
		UserAgent:  "TestClient/1.0",
		RemoteAddr: "192.168.1.3",
		Headers:    make(map[string]string),
	}
	resA := ProcessSQL(context.Background(), database.NewRegistry(db), cm, dispatcher, reqA, isBanned)
	assert.Equal(t, 200, resA.StatusCode)
	assert.Contains(t, resA.Body, `"email": "traffic_user@example.com"`)
	assert.Contains(t, resA.Body, `"up": 1000`)
	assert.Contains(t, resA.Body, `"down": 2000`)

	// Scenario B: User only exists in StatsState -> should fallback to local master stats (50/60)
	reqB := &Request{
		Query:      map[string]string{"id": "sub_local", "hwid": "my-hwid"},
		UserAgent:  "TestClient/1.0",
		RemoteAddr: "192.168.1.3",
		Headers:    make(map[string]string),
	}
	resB := ProcessSQL(context.Background(), database.NewRegistry(db), cm, dispatcher, reqB, isBanned)
	assert.Equal(t, 200, resB.StatusCode)
	assert.Contains(t, resB.Body, `"email": "local_only@example.com"`)
	assert.Contains(t, resB.Body, `"up": 50`)
	assert.Contains(t, resB.Body, `"down": 60`)
}

func TestProcessSQL_BlacklistedAdmin(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		sqlDB, _ := db.DB()
		sqlDB.Close()
		os.Remove("test_xray_config_bla.json")
		os.Remove("test_xray_template_bla.json")
	}()

	// Config does NOT contain the admin (because they are blacklisted and filtered out)
	os.WriteFile("test_xray_config_bla.json", []byte(`{"inbounds":[]}`), 0644)

	// Template contains the hardcoded admin
	tmplJSON := `{
		"inbounds": [
			{
				"tag": "vless",
				"protocol": "vless",
				"settings": {
					"clients": [
						{
							"email": "admin-blacklisted@example.com",
							"id": "admin-blacklisted-id",
							"subfile": "admin-blacklisted-id"
						}
					]
				}
			}
		]
	}`
	os.WriteFile("test_xray_template_bla.json", []byte(tmplJSON), 0644)

	cfg := &appconfig.Config{
		Paths: appconfig.PathsConf{
			XrayConfig:   "test_xray_config_bla.json",
			XrayTemplate: "test_xray_template_bla.json",
		},
		Subscription: appconfig.SubscriptionConf{
			UserAgentWhitelist: []string{"testclient"},
			DummyConfigs: appconfig.DummyConfigsConf{
				Expired: []string{"ADMIN_EXPIRED_DUMMY"},
			},
		},
	}

	cm := NewCacheManager(cfg, &vpn.NoopEngine{})
	isBanned := func(email string) bool { return false }
	dispatcher := events.NewDispatcher(&events.Config{Webhooks: []string{}})

	req := &Request{
		Query:      map[string]string{"id": "admin-blacklisted-id"},
		UserAgent:  "TestClient/1.0",
		RemoteAddr: "192.168.1.1",
		Headers:    make(map[string]string),
	}

	res := ProcessSQL(context.Background(), database.NewRegistry(db), cm, dispatcher, req, isBanned)

	assert.Equal(t, 200, res.StatusCode, "Should return 200 OK")
	assert.Contains(t, res.Body, "ADMIN_EXPIRED_DUMMY", "Should return the expired dummy warning config")
}
