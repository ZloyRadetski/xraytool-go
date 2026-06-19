package cmd

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"xraytool/internal/userdb"
	"xraytool/internal/xrayconfig"
)

func TestApplyBatchCmd(t *testing.T) {
	setupTest(t)
	defer teardownTest()

	// 1. Create a dummy xray config
	dummyCfg := `{
		"inbounds": [
			{
				"tag": "vless-tcp",
				"protocol": "vless",
				"settings": {
					"clients": [
						{"id": "uuid-1", "email": "user1@example.com"},
						{"id": "uuid-2", "email": "user2@example.com"}
					]
				}
			}
		]
	}`
	os.WriteFile("test_xray_config.json", []byte(dummyCfg), 0644)

	// 2. Prepare limits DB
	db := userdb.New("test_limited.db")
	_ = db.Upsert(userdb.Entry{Email: "user2@example.com", Subfile: "sub2", Limit: nil})

	// 3. Prepare payload:
	// - Remove user2@example.com
	// - Add user3@example.com
	// - Limit user3@example.com to 10
	// - Unlimit user2@example.com
	var limit10 float64 = 10
	var limit0 float64 = 0

	payload := BatchPayload{
		Add: []SnapshotUser{
			{Email: "user3@example.com", UUID: "uuid-3", Subfile: "sub3", Expire: "2030-01-01"},
		},
		Remove: []string{"user2@example.com"},
		Limit: []SnapshotLimited{
			{Email: "user3@example.com", Limit: &limit10, Subfile: "sub3"},
			{Email: "user2@example.com", Limit: &limit0}, // remove limit
		},
	}
	payloadBytes, _ := json.Marshal(payload)

	// 4. Run command
	rootCmd.SetArgs([]string{"apply-batch", "--payload", string(payloadBytes)})
	out := captureOutput(func() {
		rootCmd.Execute()
	})

	// 5. Check output
	if !strings.Contains(out, `"status":"success"`) {
		t.Fatalf("Expected success, got %s", out)
	}

	// 6. Verify config changes
	xrayCfg, _ := xrayconfig.Read("test_xray_config.json")
	users, _ := xrayconfig.ListUsers(xrayCfg)
	
	foundUser1 := false
	foundUser2 := false
	foundUser3 := false
	for _, u := range users {
		if u.Email() == "user1@example.com" { foundUser1 = true }
		if u.Email() == "user2@example.com" { foundUser2 = true }
		if u.Email() == "user3@example.com" { foundUser3 = true }
	}

	if !foundUser1 { t.Errorf("user1 should remain") }
	if foundUser2 { t.Errorf("user2 should be removed") }
	if !foundUser3 { t.Errorf("user3 should be added") }

	// 7. Verify limits changes
	entries, _ := db.All()
	foundLimit3 := false
	for _, e := range entries {
		if e.Email == "user3@example.com" && e.Limit != nil && *e.Limit == 10 {
			foundLimit3 = true
		}
		if e.Email == "user2@example.com" {
			t.Errorf("user2 limit should be removed")
		}
	}
	if !foundLimit3 { t.Errorf("user3 limit should be set to 10") }
}

func TestApplyBatchCmdEmpty(t *testing.T) {
	setupTest(t)
	defer teardownTest()

	rootCmd.SetArgs([]string{"apply-batch", "--payload", ""})
	out := captureOutput(func() {
		rootCmd.Execute()
	})

	if !strings.Contains(out, "payload is required") {
		t.Errorf("Expected payload required error, got %s", out)
	}
}

func TestApplyBatchCmdInvalidJSON(t *testing.T) {
	setupTest(t)
	defer teardownTest()

	rootCmd.SetArgs([]string{"apply-batch", "--payload", "{"})
	out := captureOutput(func() {
		rootCmd.Execute()
	})

	if !strings.Contains(out, "invalid json payload") {
		t.Errorf("Expected invalid json error, got %s", out)
	}
}
