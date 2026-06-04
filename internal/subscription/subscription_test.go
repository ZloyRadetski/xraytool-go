package subscription

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"xraytool/internal/appconfig"
)

// setupProcessTestEnv creates a specialized environment for testing ProcessSubscription.
func setupProcessTestEnv(t *testing.T) (string, *appconfig.Config, func()) {
	tmpDir, err := os.MkdirTemp("", "xraytool_sub_test_*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}

	xrayConfigPath := filepath.Join(tmpDir, "config.json")
	limitedDBPath := filepath.Join(tmpDir, "limited_users.db")
	subTmplPath := filepath.Join(tmpDir, "configs.txt")
	routingPath := filepath.Join(tmpDir, "routing.json")
	routingRUPath := filepath.Join(tmpDir, "routing_ALL_RU.json")

	// Set up users:
	// 1. user1: normal, active, limit 5, no expire (or future expire)
	// 2. user2: active, but expired
	// 3. user3: active, limit 1 (for testing device limit)
	xrayConfigContent := `{
		"inbounds": [
			{
				"tag": "vless-in",
				"protocol": "vless",
				"settings": {
					"clients": [
						{"id": "uuid-1", "email": "user1", "subfile": "sub1", "limit": 5, "expire": "01-01-2099"},
						{"id": "uuid-2", "email": "user2", "subfile": "sub2", "limit": 5, "expire": "01-01-2000"},
						{"id": "uuid-3", "email": "user3", "subfile": "sub3", "limit": 1, "expire": "01-01-2099"}
					]
				}
			}
		]
	}`
	os.WriteFile(xrayConfigPath, []byte(xrayConfigContent), 0644)

	// limited user
	limitedDBContent := "user4|sub4\n"
	os.WriteFile(limitedDBPath, []byte(limitedDBContent), 0644)

	// Add '---' so parseSubscriptionTemplate finds it.
	os.WriteFile(subTmplPath, []byte(`// HEADER
---
{"dummy": "dummy_sub_template_for_{EMAIL}"}`), 0644)
	os.WriteFile(routingPath, []byte(`{"rules":["global"]}`), 0644)
	os.WriteFile(routingRUPath, []byte(`{"rules":["ru"]}`), 0644)

	cfg := &appconfig.Config{
		Paths: appconfig.PathsConf{
			XrayConfig:               xrayConfigPath,
			LimitedDB:                limitedDBPath,
			JSONSubscriptionTemplate: subTmplPath,
			RoutingTemplate:          routingPath,
			RoutingRUTemplate:        routingRUPath,
		},
		Server: appconfig.ServerConf{
			Domain: "sub.example.com",
		},
	}

	return tmpDir, cfg, func() { os.RemoveAll(tmpDir) }
}

func TestProcessSubscription_Normal(t *testing.T) {
	_, cfg, cleanup := setupProcessTestEnv(t)
	defer cleanup()

	cm := NewCacheManager(cfg)
	cm.Refresh() // Force sync

	req := &Request{
		RemoteAddr: "1.1.1.1",
		UserAgent:  "happ/1000",
		Query:      map[string]string{"id": "sub1"},
		Headers: map[string]string{
			"Host":   "sub.example.com",
			"X-Hwid": "device1",
		},
	}

	res := Process(cm, req)

	if res.StatusCode != 200 {
		t.Errorf("Expected status 200, got %d", res.StatusCode)
	}
	if !strings.Contains(res.Body, "dummy_sub_template_for_user1") {
		t.Errorf("Expected template body for user1, got: %s", res.Body)
	}
}

func TestProcessSubscription_Expired(t *testing.T) {
	_, cfg, cleanup := setupProcessTestEnv(t)
	defer cleanup()

	cm := NewCacheManager(cfg)
	cm.Refresh()

	req := &Request{
		RemoteAddr: "1.1.1.1",
		UserAgent:  "happ/1000",
		Query:      map[string]string{"id": "sub2"},
		Headers: map[string]string{
			"Host":   "sub.example.com",
			"X-Hwid": "device1",
		},
	}

	res := Process(cm, req)

	if res.StatusCode != 200 {
		t.Errorf("Expected status 200 for expired user, got %d", res.StatusCode)
	}
	if !strings.Contains(res.Body, "ПОДПИСКА ЗАКОНЧИЛАСЬ") {
		t.Errorf("Unexpected body for expired user: %s", res.Body)
	}
}

func TestProcessSubscription_DeviceLimit(t *testing.T) {
	_, cfg, cleanup := setupProcessTestEnv(t)
	defer cleanup()

	cm := NewCacheManager(cfg)
	cm.Refresh()

	// user3 has limit=1
	req1 := &Request{
		RemoteAddr: "1.1.1.1",
		UserAgent:  "happ/1000",
		Query:      map[string]string{"id": "sub3"},
		Headers: map[string]string{
			"Host":   "sub.example.com",
			"X-Hwid": "device1", // HWID injected from PHP
		},
	}

	req2 := &Request{
		RemoteAddr: "1.1.1.1",
		UserAgent:  "happ/1000",
		Query:      map[string]string{"id": "sub3"},
		Headers: map[string]string{
			"Host":   "sub.example.com",
			"X-Hwid": "device2", // Different device
		},
	}

	// First request should succeed
	res1 := Process(cm, req1)
	if res1.StatusCode != 200 {
		t.Errorf("Expected status 200 for first device, got %d", res1.StatusCode)
	}

	// Second request should be blocked (limit=1 reached)
	res2 := Process(cm, req2)
	if res2.StatusCode != 200 {
		t.Errorf("Expected status 200 for second device, got %d", res2.StatusCode)
	}
	if !strings.Contains(res2.Body, "Лимит устройств") {
		t.Errorf("Expected limit exceeded body, got: %s", res2.Body)
	}
}

func TestProcessSubscription_NotFound(t *testing.T) {
	_, cfg, cleanup := setupProcessTestEnv(t)
	defer cleanup()

	cm := NewCacheManager(cfg)
	cm.Refresh()

	req := &Request{
		RemoteAddr: "1.1.1.1",
		UserAgent:  "happ/1000",
		Query:      map[string]string{"id": "sub999"},
		Headers: map[string]string{
			"Host": "sub.example.com",
		},
	}

	res := Process(cm, req)

	if res.StatusCode != 404 {
		t.Errorf("Expected status 404 for non-existent user, got %d", res.StatusCode)
	}
}

func TestProcessSubscription_LimitedUser(t *testing.T) {
	_, cfg, cleanup := setupProcessTestEnv(t)
	defer cleanup()

	cm := NewCacheManager(cfg)
	cm.Refresh()

	// user4 is in limited_users.db
	req := &Request{
		RemoteAddr: "1.1.1.1",
		UserAgent:  "happ/1000",
		Query:      map[string]string{"id": "sub4"},
		Headers: map[string]string{
			"Host": "sub.example.com",
		},
	}

	res := Process(cm, req)

	// Since they are limited, they should receive SOCKS block.
	if res.StatusCode != 200 {
		t.Errorf("Expected status 200 for limited user, got %d", res.StatusCode)
	}
	if !strings.Contains(res.Body, "ПОДПИСКА ЗАКОНЧИЛАСЬ") {
		t.Errorf("Unexpected body for limited user: %s", res.Body)
	}
}
