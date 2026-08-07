package engine_xray

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"xraytool/internal/domain"
)

func setupTrackerMockXray(t *testing.T) string {
	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "calls.log")

	// Create the call log file
	if err := os.WriteFile(logPath, []byte(""), 0644); err != nil {
		t.Fatalf("failed to create call log file: %v", err)
	}

	batPath := filepath.Join(tmp, "xray.bat")
	batContent := `@echo off
echo %* >> "` + logPath + `"
exit /b 0
`
	if err := os.WriteFile(batPath, []byte(batContent), 0755); err != nil {
		t.Fatalf("failed to write tracker xray.bat: %v", err)
	}

	oldPath := os.Getenv("PATH")
	t.Setenv("PATH", tmp+string(os.PathListSeparator)+oldPath)
	return logPath
}

func TestAdapter_HysteriaRebuildOptimization(t *testing.T) {
	logPath := setupTrackerMockXray(t)

	tmp := t.TempDir()
	configPath := filepath.Join(tmp, "config.json")

	// Config with one VLESS inbound and one Hysteria2 inbound
	initialConfig := `{
		"inbounds": [
			{
				"tag": "vless-inbound",
				"protocol": "vless",
				"port": 443,
				"settings": {
					"clients": []
				}
			},
			{
				"tag": "hysteria-inbound",
				"protocol": "hysteria2",
				"port": 8443,
				"settings": {
					"users": []
				}
			}
		]
	}`

	if err := os.WriteFile(configPath, []byte(initialConfig), 0644); err != nil {
		t.Fatalf("failed to write initial config: %v", err)
	}

	// Create adapter
	adapter := NewAdapter("127.0.0.1:10085", configPath, "", false, "", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	// Helper to read call log
	readCalls := func() []string {
		data, err := os.ReadFile(logPath)
		if err != nil {
			return nil
		}
		lines := strings.Split(strings.TrimSpace(string(data)), "\n")
		var out []string
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line != "" {
				out = append(out, line)
			}
		}
		return out
	}

	clearCalls := func() {
		os.WriteFile(logPath, []byte(""), 0644) //nolint:errcheck
	}

	// Scenario 1: Add a user for the first time.
	// Since they are added to all active inbounds (VLESS and Hysteria), the Hysteria inbound config
	// changes, and it should rebuild Hysteria.
	user := domain.VPNUserConfig{
		Email: "testuser@test.com",
		UUID:  "11111111-1111-1111-1111-111111111111",
		Auth:  BuildDeterministicHy2Pass("11111111-1111-1111-1111-111111111111", "testuser@test.com"),
	}

	if err := adapter.AddUser(context.Background(), user); err != nil {
		t.Fatalf("AddUser failed: %v", err)
	}

	calls := readCalls()
	rebuilt := false
	for _, call := range calls {
		if strings.Contains(call, "rmi") && strings.Contains(call, "hysteria-inbound") {
			rebuilt = true
		}
	}
	if !rebuilt {
		t.Errorf("expected hysteria-inbound to be rebuilt on first user addition, but it was not. Calls: %v", calls)
	}

	clearCalls()

	// Scenario 2: Add the same user again (idempotent, no changes).
	// Since the user is already in the config, the Hysteria inbound config does NOT change,
	// and it should NOT rebuild Hysteria.
	if err := adapter.AddUser(context.Background(), user); err != nil {
		t.Fatalf("Idempotent AddUser failed: %v", err)
	}

	calls = readCalls()
	for _, call := range calls {
		if strings.Contains(call, "rmi") || strings.Contains(call, "adi") {
			if strings.Contains(call, "hysteria-inbound") {
				t.Errorf("hysteria-inbound was unexpectedly rebuilt for idempotent add: %s", call)
			}
		}
	}

	clearCalls()

	// Scenario 3: Sync users with no changes.
	// Hysteria inbound should NOT be rebuilt.
	if _, err := adapter.SyncUsers(context.Background(), []domain.VPNUserConfig{user}, true); err != nil {
		t.Fatalf("SyncUsers failed: %v", err)
	}

	calls = readCalls()
	for _, call := range calls {
		if strings.Contains(call, "rmi") || strings.Contains(call, "adi") {
			if strings.Contains(call, "hysteria-inbound") {
				t.Errorf("hysteria-inbound was unexpectedly rebuilt for no-change SyncUsers: %s", call)
			}
		}
	}
}

func TestAdapter_SyncUsersHashOptimization(t *testing.T) {
	_ = setupTrackerMockXray(t)

	// Start mock gRPC server so that gRPC calls succeed and state is not marked dirty
	mockHandler := &mockHandlerServer{}
	addr, cleanup := startMockGRPCServer(t, nil, mockHandler)
	defer cleanup()

	tmp := t.TempDir()
	configPath := filepath.Join(tmp, "config.json")

	// Config with VLESS inbound
	initialConfig := `{
		"inbounds": [
			{
				"tag": "vless-inbound",
				"protocol": "vless",
				"port": 443,
				"settings": {
					"clients": []
				}
			}
		]
	}`

	if err := os.WriteFile(configPath, []byte(initialConfig), 0644); err != nil {
		t.Fatalf("failed to write initial config: %v", err)
	}

	adapter := NewAdapter(addr, configPath, "", false, "", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	user := domain.VPNUserConfig{
		Email: "testuser@test.com",
		UUID:  "11111111-1111-1111-1111-111111111111",
		Auth:  BuildDeterministicHy2Pass("11111111-1111-1111-1111-111111111111", "testuser@test.com"),
	}

	// 1. First SyncUsers: Should run full sync and add the user
	res, err := adapter.SyncUsers(context.Background(), []domain.VPNUserConfig{user}, true)
	if err != nil {
		t.Fatalf("First SyncUsers failed: %v", err)
	}
	if res.Added != 1 {
		t.Errorf("expected 1 user added, got %d", res.Added)
	}

	// Verify that the user was added to config.json on disk
	cfgContent, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("failed to read config.json: %v", err)
	}
	if !strings.Contains(string(cfgContent), "testuser@test.com") {
		t.Errorf("expected user testuser@test.com to be in config.json, but it was not")
	}

	// Verify that the hash file was created
	hashPath := configPath + ".hash"
	if _, err := os.Stat(hashPath); err != nil {
		t.Errorf("expected hash file %s to be created, but it was not", hashPath)
	}

	// 2. Second SyncUsers with same users: Should skip sync because hash matches
	res2, err := adapter.SyncUsers(context.Background(), []domain.VPNUserConfig{user}, true)
	if err != nil {
		t.Fatalf("Second SyncUsers failed: %v", err)
	}
	if res2.Added != 0 || res2.Removed != 0 {
		t.Errorf("expected 0 added/removed on skipped sync, got Added=%d, Removed=%d", res2.Added, res2.Removed)
	}

	// 3. Invalidate hash
	adapter.invalidateHash()

	// Verify hash file was deleted
	if _, err := os.Stat(hashPath); err == nil {
		t.Errorf("expected hash file to be deleted, but it exists")
	}

	// 4. Third SyncUsers: Should run full sync again because hash file is missing
	if _, err := adapter.SyncUsers(context.Background(), []domain.VPNUserConfig{user}, true); err != nil {
		t.Fatalf("Third SyncUsers failed: %v", err)
	}
	// Since user is already in config, the sync did run (which we know because the hash file was recreated)
	if _, err := os.Stat(hashPath); err != nil {
		t.Errorf("expected hash file to be recreated after sync, but it was not")
	}
}
