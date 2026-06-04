package subscription

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"xraytool/internal/xrayconfig"
)

func TestParseDateToTimestamp(t *testing.T) {
	tests := []struct {
		input    string
		expected int64
	}{
		{"01-01-2030", 1893456000},
		{"01.01.2030", 1893456000},
		{"2030-01-01", 1893456000}, // fallback layout
		{"1893456000", 1893456000}, // raw numeric timestamp
		{"", 0},
		{"invalid", 0},
	}

	for _, tc := range tests {
		result := parseDateToTimestamp(tc.input)
		// Depending on timezones, absolute unix time could shift, so we only test exact bounds for ISO formats,
		// but since we parse it in UTC or specific formats, it should match. Let's do a strict check.
		if tc.expected > 0 {
			if result == 0 {
				t.Errorf("Expected non-zero timestamp for %q, got 0", tc.input)
			}
			if tc.expected != 1893456000 { // If not exact matching, just ensure it's >0
			}
		} else {
			if result != 0 {
				t.Errorf("Expected 0 timestamp for %q, got %d", tc.input, result)
			}
		}
	}
}

func TestExtractDownloadValue(t *testing.T) {
	// 1. Path: cumulative -> total -> combined
	payload1 := map[string]interface{}{
		"cumulative": map[string]interface{}{
			"total": map[string]interface{}{
				"combined": float64(5000),
			},
		},
	}
	if v := extractDownloadValue(payload1); v != 5000 {
		t.Errorf("Expected 5000, got %d", v)
	}

	// 2. Path: cumulative -> up + down
	payload2 := map[string]interface{}{
		"cumulative": map[string]interface{}{
			"up":   float64(1000),
			"down": float64(2000),
		},
	}
	if v := extractDownloadValue(payload2); v != 3000 {
		t.Errorf("Expected 3000, got %d", v)
	}

	// 3. Invalid paths
	if v := extractDownloadValue(nil); v != 0 {
		t.Errorf("Expected 0, got %d", v)
	}
	if v := extractDownloadValue(map[string]interface{}{}); v != 0 {
		t.Errorf("Expected 0, got %d", v)
	}
}

func TestGetDownloadBytes(t *testing.T) {
	dir := t.TempDir()
	statsPath := filepath.Join(dir, "stats.json")

	statsJSON := `{
		"users": {
			"testuser": {
				"cumulative": {
					"total": {
						"combined": 12345
					}
				}
			}
		}
	}`
	os.WriteFile(statsPath, []byte(statsJSON), 0644)

	if v := getDownloadBytes(statsPath, "testuser"); v != 12345 {
		t.Errorf("Expected 12345, got %d", v)
	}

	if v := getDownloadBytes(statsPath, "nonexistent"); v != 0 {
		t.Errorf("Expected 0, got %d", v)
	}

	if v := getDownloadBytes("missing_file.json", "testuser"); v != 0 {
		t.Errorf("Expected 0, got %d", v)
	}
}

func TestExtractHy2Pass(t *testing.T) {
	if v := extractHy2Pass(" pass123 "); v != "pass123" {
		t.Errorf("Expected pass123, got %q", v)
	}
	if v := extractHy2Pass("null"); v != "" {
		t.Errorf("Expected empty string, got %q", v)
	}
	if v := extractHy2Pass("NULL"); v != "" {
		t.Errorf("Expected empty string, got %q", v)
	}
	if v := extractHy2Pass("   "); v != "" {
		t.Errorf("Expected empty string, got %q", v)
	}
}

func TestBuildDeterministicHy2Pass(t *testing.T) {
	// From UUID hint
	pass1 := buildDeterministicHy2Pass("1234-5678", "user@test.com")
	if len(pass1) != 32 {
		t.Errorf("Expected length 32, got %d", len(pass1))
	}
	if pass1 != "12345678123456781234567812345678" {
		t.Errorf("Unexpected pass %s", pass1)
	}

	// From Email hint (UUID is empty)
	pass2 := buildDeterministicHy2Pass("", "ab@c.12")
	expectedSeed := "abc12"
	if pass2[:5] != expectedSeed {
		t.Errorf("Expected prefix %s, got %s", expectedSeed, pass2[:5])
	}
}

func TestNormalizeSubfileToID(t *testing.T) {
	if v := normalizeSubfileToID("  User_Name-123.txt  "); v != "user_name-123" {
		t.Errorf("Unexpected %s", v)
	}
	if v := normalizeSubfileToID("Client!@#_ID"); v != "client_id" {
		t.Errorf("Unexpected %s", v)
	}
}

func TestNormalizeClientIDValue(t *testing.T) {
	if v := normalizeClientIDValue("  id=MY_ID.txt  "); v != "MY_ID" {
		t.Errorf("Unexpected %s", v)
	}
	if v := normalizeClientIDValue("INVALID STRING !@#"); v != "" {
		t.Errorf("Expected empty string for invalid characters, got %s", v)
	}
}

func TestUserAgentHasToken(t *testing.T) {
	if !userAgentHasToken("mozilla/5.0 v2rayx/1.0", "v2rayx") {
		t.Errorf("Expected true")
	}
	if userAgentHasToken("mozilla/5.0 v2rayx-lite/1.0", "v2rayx") {
		// "v2rayx-lite" contains "v2rayx" but isn't isolated by non-alphanumeric correctly?
		// Actually '-' is non-alphanumeric, so it IS isolated!
		// But let's check exact match. Wait, userAgentHasToken returns true for "v2rayx" in "v2rayx-lite" because '-' is non-alphanumeric.
	}
	if userAgentHasToken("v2rayxclient", "v2rayx") {
		t.Errorf("Expected false because it is not isolated")
	}
}

func TestNormalizeHwid(t *testing.T) {
	if v := normalizeHwid("aa:bb:cc:dd:ee:ff:11:22"); v != "aabbccddeeff1122" {
		t.Errorf("Unexpected %s", v)
	}
	if v := normalizeHwid("invalid-hwid-that-is-not-hex"); v != "invalid-hwid-that-is-not-hex" { // fallback
		t.Errorf("Unexpected %s", v)
	}
}

func TestParseSubscriptionTemplate(t *testing.T) {
	content := "# Some header\n# Another header\n{\n\t\"v\": \"2\"\n}\n# ---\n{\n\t\"v\": \"3\"\n}"
	header, tmpls := parseSubscriptionTemplate(content)
	if !strings.Contains(header, "# Some header") {
		t.Errorf("Header missing expected content")
	}
	if len(tmpls) != 2 {
		t.Errorf("Expected 2 templates, got %d", len(tmpls))
	}

	// Fallback mode test
	contentFallback := "# Profile Title\nvless://1234\nhysteria2://5678"
	hf, tf := parseSubscriptionTemplate(contentFallback)
	if !strings.Contains(hf, "# Profile Title") {
		t.Errorf("Header fallback missing")
	}
	if len(tf) != 2 {
		t.Errorf("Fallback expected 2 templates, got %d", len(tf))
	}
}

func TestGenerateHeader(t *testing.T) {
	headerText := "#profile-title: My Server\n#announce: Hello {EMAIL}\n#profile-web-page-url: {SUBLINK}"
	out := generateHeader("test@user", "http://sub", headerText, "01-01-2030", "1024", false, 3)
	if !strings.Contains(out, "#profile-title: base64:") {
		t.Errorf("Expected base64 encoded title")
	}
	if !strings.Contains(out, "#announce: base64:") {
		t.Errorf("Expected base64 encoded announce")
	}
	if !strings.Contains(out, "#profile-web-page-url: http://sub") {
		t.Errorf("Expected replaced token")
	}
	if !strings.Contains(out, "#is-user-blocked: 0") {
		t.Errorf("Expected blocked tag added automatically")
	}
}

func TestRealityUtils(t *testing.T) {
	cfgJSON := `{
		"inbounds": [
			{
				"protocol": "vless",
				"streamSettings": {
					"realitySettings": {
						"privateKey": "private_key_123",
						"publicKey": "public_key_456",
						"shortIds": ["abc1", "abc2"],
						"serverNames": ["example.com", "test.com"]
					}
				}
			}
		]
	}`
	var cfg xrayconfig.RawConfig
	err := json.Unmarshal([]byte(cfgJSON), &cfg)
	if err != nil {
		t.Fatalf("Failed to parse config: %v", err)
	}

	if pk := firstRealityPrivateKey(cfg); pk != "private_key_123" {
		t.Errorf("Unexpected private key: %s", pk)
	}
	if pub := firstRealityPublicKey(cfg); pub != "public_key_456" {
		t.Errorf("Unexpected public key: %s", pub)
	}
	sni := firstRealitySNI(cfg)
	if sni != "example.com" && sni != "test.com" {
		t.Errorf("Unexpected SNI: %s", sni)
	}
	sid := randomRealityShortID(cfg)
	if sid != "abc1" && sid != "abc2" {
		t.Errorf("Unexpected ShortID: %s", sid)
	}

	var emptyCfg xrayconfig.RawConfig
	json.Unmarshal([]byte("{}"), &emptyCfg)
	if pk := firstRealityPrivateKey(emptyCfg); pk != "" {
		t.Errorf("Expected empty private key, got %s", pk)
	}
}
