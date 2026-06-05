package xrayapi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"xraytool/internal/xrayconfig"
)

// setupMockXray creates a fake xray.bat script in a temporary directory and prepends it to PATH.
func setupMockXray(t *testing.T) {
	tmp := t.TempDir()
	batPath := filepath.Join(tmp, "xray.bat")
	
	// A batch script to simulate xray behavior based on environment variables
	batContent := `@echo off
if "%MOCK_XRAY_FAIL%"=="1" (
	echo Mock error output
	exit /b 1
)
if "%MOCK_XRAY_PARTIAL_FAIL%"=="1" (
	type %5 | findstr "failtag" >nul
	if not errorlevel 1 (
		echo Single tag fail
		exit /b 1
	)
)
if "%MOCK_XRAY_STATS%"=="1" (
	echo { "stat": [ { "name": "user>>>test@example.com>>>traffic>>>uplink", "value": 123 } ] }
	exit /b 0
)
if "%MOCK_XRAY_STATS_EMPTY%"=="1" (
	echo null
	exit /b 0
)
if "%MOCK_XRAY_STATS_BAD_JSON%"=="1" (
	echo { bad json
	exit /b 0
)
if "%MOCK_XRAY_ADU_OUTPUT%"=="1" (
	echo Successfully added
	exit /b 0
)
if "%MOCK_XRAY_RMU_OUTPUT%"=="1" (
	echo Successfully removed
	exit /b 0
)
exit /b 0
`
	if err := os.WriteFile(batPath, []byte(batContent), 0755); err != nil {
		t.Fatalf("failed to write mock xray.bat: %v", err)
	}

	oldPath := os.Getenv("PATH")
	t.Setenv("PATH", tmp+string(os.PathListSeparator)+oldPath)
}

func TestClient_AddUser(t *testing.T) {
	setupMockXray(t)
	c := New("127.0.0.1:10085")

	// Create a dummy config
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "config.json")
	os.WriteFile(cfgPath, []byte(`{"inbounds":[
		{"tag":"tag1","protocol":"vless","port":443,"settings":{"clients":[]}},
		{"tag":"tag2","protocol":"vmess","port":8443,"settings":null}
	]}`), 0644)

	var rc xrayconfig.RawClient
	json.Unmarshal([]byte(`{"id":"uuid-1","email":"test@example.com"}`), &rc)

	payload := []xrayconfig.TaggedClient{
		{Tag: "tag1", Client: rc},
		{Tag: "tag2", Client: rc},
	}

	// 1. Success case
	t.Setenv("MOCK_XRAY_ADU_OUTPUT", "1")
	if err := c.AddUser(payload, cfgPath); err != nil {
		t.Errorf("expected no error, got %v", err)
	}

	// 2. Empty payload case
	if err := c.AddUser(nil, cfgPath); err != nil {
		t.Errorf("expected no error, got %v", err)
	}

	// 3. Batch fail, fallback success
	t.Setenv("MOCK_XRAY_ADU_OUTPUT", "")
	t.Setenv("MOCK_XRAY_FAIL", "")
	t.Setenv("MOCK_XRAY_PARTIAL_FAIL", "1")

	payloadWithFail := []xrayconfig.TaggedClient{
		{Tag: "tag1", Client: rc},
		{Tag: "failtag", Client: rc},
	}
	
	err := c.AddUser(payloadWithFail, cfgPath)
	if err == nil {
		t.Errorf("expected error from failtag, got nil")
	}

	// 4. Broken payload marshal
	// AddUser doesn't fail easily on marshal unless the types are unsupported.
	// But we can trigger an error on bad config if needed.
	// Actually, just having coverage on the fallback is good.
}

func TestClient_RemoveUser(t *testing.T) {
	setupMockXray(t)
	c := New("127.0.0.1:10085")

	// 1. Success case
	t.Setenv("MOCK_XRAY_RMU_OUTPUT", "1")
	if err := c.RemoveUser("test@example.com", []string{"tag1", "tag2"}); err != nil {
		t.Errorf("expected no error, got %v", err)
	}

	// 2. Empty tags
	if err := c.RemoveUser("test@example.com", nil); err != nil {
		t.Errorf("expected no error, got %v", err)
	}

	// 3. Fail case
	t.Setenv("MOCK_XRAY_FAIL", "1")
	if err := c.RemoveUser("test@example.com", []string{"tag1"}); err == nil {
		t.Errorf("expected error, got nil")
	}
}

func TestClient_QueryStats(t *testing.T) {
	setupMockXray(t)
	c := New("127.0.0.1:10085")

	// 1. Success
	t.Setenv("MOCK_XRAY_STATS", "1")
	stats, err := c.QueryStats()
	if err != nil {
		t.Errorf("expected no error, got %v", err)
	}
	if len(stats) != 1 || stats[0].Email != "test@example.com" {
		t.Errorf("unexpected stats: %+v", stats)
	}

	// 2. Empty stats
	t.Setenv("MOCK_XRAY_STATS", "")
	t.Setenv("MOCK_XRAY_STATS_EMPTY", "1")
	stats, err = c.QueryStats()
	if err != nil {
		t.Errorf("expected no error, got %v", err)
	}
	if len(stats) != 0 {
		t.Errorf("expected empty stats, got %+v", stats)
	}

	// 3. Bad JSON
	t.Setenv("MOCK_XRAY_STATS_EMPTY", "")
	t.Setenv("MOCK_XRAY_STATS_BAD_JSON", "1")
	_, err = c.QueryStats()
	if err == nil {
		t.Errorf("expected error on bad json, got nil")
	}

	// 4. Exec fail
	t.Setenv("MOCK_XRAY_STATS_BAD_JSON", "")
	t.Setenv("MOCK_XRAY_FAIL", "1")
	_, err = c.QueryStats()
	if err == nil {
		t.Errorf("expected error on exec fail, got nil")
	}
}
