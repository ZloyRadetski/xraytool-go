package templates

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"xraytool/internal/xrayconfig"
)

func TestBuildFromTemplate(t *testing.T) {
	dir := t.TempDir()

	// VLESS Template
	vlessPath := filepath.Join(dir, "vless-in.txt")
	vlessTmpl := map[string]string{
		"email":   "",
		"id":      "",
		"flow":    "",
		"subfile": "",
		"expire":  "",
	}
	data, _ := json.Marshal(vlessTmpl)
	os.WriteFile(vlessPath, data, 0644)

	// HY2 Template
	hy2Path := filepath.Join(dir, "hy2-in.txt")
	hy2Tmpl := map[string]string{
		"email":    "",
		"password": "", // some use auth, some password
		"subfile":  "",
		"expire":   "",
	}
	data, _ = json.Marshal(hy2Tmpl)
	os.WriteFile(hy2Path, data, 0644)

	limit := 1024.0
	params := ClientParams{
		Email:   "test@example.com",
		UUID:    "12345678-1234-1234-1234-123456789012",
		Subfile: "sub1.txt",
		Expire:  "2025-01-01",
		Limit:   &limit,
	}

	// Test VLESS
	vlessClient, err := buildFromTemplate(dir, "vless-in", params)
	if err != nil {
		t.Fatalf("VLESS build error: %v", err)
	}

	if vlessClient.GetString("email") != "test@example.com" {
		t.Errorf("VLESS email mismatch: %s", vlessClient.GetString("email"))
	}
	if vlessClient.GetString("id") != params.UUID {
		t.Errorf("VLESS id mismatch: %s", vlessClient.GetString("id"))
	}
	limitVal, _ := vlessClient.GetNumber("limit")
	if limitVal != 1024.0 {
		t.Errorf("VLESS limit mismatch: %f", limitVal)
	}

	// Test HY2 (auto-generates password since Auth param is empty)
	hy2Client, err := buildFromTemplate(dir, "hy2-in", params)
	if err != nil {
		t.Fatalf("HY2 build error: %v", err)
	}

	if hy2Client.GetString("email") != "test@example.com" {
		t.Errorf("HY2 email mismatch: %s", hy2Client.GetString("email"))
	}
	if hy2Client.GetString("password") == "" {
		t.Errorf("HY2 password is empty")
	}

	// Validation check: empty required fields should fail
	badParams := ClientParams{
		Email:   "",
		UUID:    "12345678-1234-1234-1234-123456789012",
		Subfile: "sub1.txt",
		Expire:  "2025-01-01",
	}
	_, err = buildFromTemplate(dir, "vless-in", badParams)
	if err == nil {
		t.Errorf("Expected error when email is empty")
	}
}

func TestDefaultFlowForTag(t *testing.T) {
	if f := DefaultFlowForTag("reality-in-443"); f != "xtls-rprx-vision" {
		t.Errorf("Unexpected flow: %s", f)
	}
	if f := DefaultFlowForTag("reality-in-1234"); f != "xtls-rprx-vision" {
		t.Errorf("Unexpected flow: %s", f)
	}
	if f := DefaultFlowForTag("vless-tcp"); f != "" {
		t.Errorf("Unexpected flow: %s", f)
	}
}

func TestBuildDeterministicHy2Pass(t *testing.T) {
	uuid1 := "12345678-1234-1234-1234-123456789012"
	email1 := "test@example.com"

	pass1 := buildDeterministicHy2Pass(uuid1, email1)
	if len(pass1) != 32 {
		t.Errorf("Expected pass length 32, got %d", len(pass1))
	}

	// Should be deterministic
	pass2 := buildDeterministicHy2Pass(uuid1, email1)
	if pass1 != pass2 {
		t.Errorf("Expected deterministic output")
	}

	// Fallback to email if UUID is empty
	passEmail := buildDeterministicHy2Pass("", email1)
	if len(passEmail) != 32 {
		t.Errorf("Expected pass length 32 from email, got %d", len(passEmail))
	}
}

func TestValidate(t *testing.T) {
	dir := t.TempDir()

	inboundsJSON := `[
		{
			"tag": "vless-tcp",
			"protocol": "vless",
			"settings": {
				"clients": []
			}
		},
		{
			"tag": "hy2-udp",
			"protocol": "hysteria2",
			"settings": {
				"users": []
			}
		}
	]`

	var inbounds []xrayconfig.RawInbound
	json.Unmarshal([]byte(inboundsJSON), &inbounds)

	inboundsData, _ := json.Marshal(inbounds)
	cfg := xrayconfig.RawConfig{
		"inbounds": inboundsData,
	}

	err := Validate(dir, cfg)
	if err != nil {
		t.Fatalf("Validate error: %v", err)
	}

	// Check if templates were generated
	if _, err := os.Stat(filepath.Join(dir, "vless-tcp.txt")); os.IsNotExist(err) {
		t.Errorf("vless-tcp.txt was not created")
	}
	if _, err := os.Stat(filepath.Join(dir, "hy2-udp.txt")); os.IsNotExist(err) {
		t.Errorf("hy2-udp.txt was not created")
	}
}
