package subscription

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"xraytool/internal/appconfig"
)

// setupTestEnv creates a temporary directory with dummy configuration files for testing.
// Returns the temp dir path, a configured appconfig.Config, and a cleanup function.
func setupTestEnv(t *testing.T) (string, *appconfig.Config, func()) {
	tmpDir, err := os.MkdirTemp("", "xraytool_cache_test_*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}

	xrayConfigPath := filepath.Join(tmpDir, "config.json")
	limitedDBPath := filepath.Join(tmpDir, "limited_users.db")
	subTmplPath := filepath.Join(tmpDir, "configs.txt")
	routingPath := filepath.Join(tmpDir, "routing.json")
	routingRUPath := filepath.Join(tmpDir, "routing_ALL_RU.json")

	// Dummy xray config with one VLESS and one Hysteria2 inbound
	xrayConfigContent := `{
		"inbounds": [
			{
				"tag": "vless-in",
				"protocol": "vless",
				"settings": {
					"clients": [
						{"id": "uuid-1", "email": "user1", "subfile": "sub1", "limit": 5, "expire": "01-01-2030"}
					]
				}
			},
			{
				"tag": "hy2-in",
				"protocol": "hysteria2",
				"settings": {
					"users": [
						{"auth": "hy2-pass-1", "email": "user1", "subfile": "sub1"}
					]
				}
			}
		]
	}`
	os.WriteFile(xrayConfigPath, []byte(xrayConfigContent), 0644)

	// Dummy limited DB
	limitedDBContent := "user2|sub2\nuser3|sub3\n"
	os.WriteFile(limitedDBPath, []byte(limitedDBContent), 0644)

	// Dummy templates
	os.WriteFile(subTmplPath, []byte("dummy_sub_template"), 0644)
	os.WriteFile(routingPath, []byte(`{"rules":["global"]}`), 0644)
	os.WriteFile(routingRUPath, []byte(`{"rules":["ru"]}`), 0644)

	cfg := &appconfig.Config{
		Paths: appconfig.PathsConf{
			XrayConfig:               xrayConfigPath,
			LimitedDB:                limitedDBPath,
			JSONSubscriptionTemplate: subTmplPath,
			RoutingTemplate:          routingPath,
			RoutingRUTemplate:        routingRUPath,
			DevicesState:             filepath.Join(tmpDir, "devices_state.json"),
		},
	}

	cleanup := func() {
		os.RemoveAll(tmpDir)
	}

	return tmpDir, cfg, cleanup
}

func TestCacheManager_InitializationAndLoad(t *testing.T) {
	_, cfg, cleanup := setupTestEnv(t)
	defer cleanup()

	cm := NewCacheManager(cfg)

	// Ensure initially empty
	active, limited := cm.GetUserBySubfile("sub1")
	if active != nil || limited != nil {
		t.Fatalf("Expected empty cache before refresh, got active=%v, limited=%v", active, limited)
	}

	// First load
	cm.Refresh()

	// Verify User 1 (Active, merged from VLESS and HY2)
	active, limited = cm.GetUserBySubfile("sub1")
	if active == nil {
		t.Fatalf("Expected active user 'sub1', got nil")
	}
	if limited != nil {
		t.Fatalf("Expected limited user to be nil, got %v", limited)
	}
	if active.ID != "uuid-1" {
		t.Errorf("Expected active ID 'uuid-1', got '%s'", active.ID)
	}
	if active.Hy2Auth != "hy2-pass-1" {
		t.Errorf("Expected active Hy2Auth 'hy2-pass-1', got '%s'", active.Hy2Auth)
	}
	if active.Limit != 5 {
		t.Errorf("Expected active Limit 5, got %d", active.Limit)
	}

	// Verify User 2 (Limited)
	active, limited = cm.GetUserBySubfile("sub2")
	if active != nil {
		t.Fatalf("Expected active user to be nil for 'sub2'")
	}
	if limited == nil {
		t.Fatalf("Expected limited user 'sub2', got nil")
	}
	if limited.Email != "user2" {
		t.Errorf("Expected limited email 'user2', got '%s'", limited.Email)
	}

	// Verify Templates
	subTmpl, routeGlobal, routeRU := cm.GetTemplates()
	if subTmpl != "dummy_sub_template" {
		t.Errorf("Expected 'dummy_sub_template', got '%s'", subTmpl)
	}
	if routeGlobal != `{"rules":["global"]}` {
		t.Errorf("Expected routeGlobal, got '%s'", routeGlobal)
	}
	if routeRU != `{"rules":["ru"]}` {
		t.Errorf("Expected routeRU, got '%s'", routeRU)
	}
}

func TestCacheManager_SmartInvalidation(t *testing.T) {
	_, cfg, cleanup := setupTestEnv(t)
	defer cleanup()

	cm := NewCacheManager(cfg)
	cm.Refresh()

	// Ensure user exists
	active, _ := cm.GetUserBySubfile("sub1")
	if active == nil {
		t.Fatalf("Initial load failed")
	}

	// Modify the config.json on disk
	time.Sleep(10 * time.Millisecond) // Ensure ModTime is different (some OS have low resolution)
	newXrayConfigContent := `{
		"inbounds": [
			{
				"tag": "vless-in",
				"protocol": "vless",
				"settings": {
					"clients": [
						{"id": "uuid-changed", "email": "user1", "subfile": "sub1"}
					]
				}
			}
		]
	}`
	os.WriteFile(cfg.Paths.XrayConfig, []byte(newXrayConfigContent), 0644)

	// Call refresh. It should detect the change and reload.
	cm.Refresh()

	// Verify update
	active, _ = cm.GetUserBySubfile("sub1")
	if active == nil {
		t.Fatalf("Expected active user 'sub1' after reload")
	}
	if active.ID != "uuid-changed" {
		t.Errorf("Expected active ID to be updated to 'uuid-changed', got '%s'", active.ID)
	}
	// Since we removed HY2 inbound in the new config, Hy2Auth should now be empty in the merged user
	if active.Hy2Auth != "" {
		t.Errorf("Expected Hy2Auth to be cleared, got '%s'", active.Hy2Auth)
	}
}

func TestCacheManager_Concurrency(t *testing.T) {
	_, cfg, cleanup := setupTestEnv(t)
	defer cleanup()

	cm := NewCacheManager(cfg)
	cm.Refresh()

	var wg sync.WaitGroup
	// 50 goroutines constantly reading
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				cm.GetUserBySubfile("sub1")
				cm.GetTemplates()
				time.Sleep(1 * time.Millisecond)
			}
		}()
	}

	// 5 goroutines constantly writing (calling Refresh)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				// We touch the file to force an actual map rebuild
				now := time.Now()
				os.Chtimes(cfg.Paths.XrayConfig, now, now)
				cm.Refresh()
				time.Sleep(5 * time.Millisecond)
			}
		}()
	}

	// If there is a data race (concurrent map read/write), the test will crash.
	wg.Wait()
}
