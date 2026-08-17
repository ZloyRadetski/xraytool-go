package engine_xray

import (
	json "github.com/goccy/go-json"
	"os"
	"path/filepath"
	"testing"

	"xraytool/internal/domain"
)

// minXrayTemplate is the smallest valid xray template with static clients.
const minXrayTemplate = `{
  "inbounds": [
    {
      "tag": "vless-reality-443",
      "protocol": "vless",
      "settings": {
        "decryption": "none",
        "clients": [
          {"email": "reverse-proxy-us", "id": "static-uuid-1", "flow": "xtls-rprx-vision"},
          {"email": "admin-debug", "id": "static-uuid-2", "flow": "xtls-rprx-vision"}
        ]
      }
    },
    {
      "tag": "trojan-443",
      "protocol": "trojan",
      "settings": {
        "clients": [
          {"email": "reverse-proxy-us", "password": "static-pass-1"}
        ]
      }
    },
    {
      "tag": "api",
      "protocol": "dokodemo-door",
      "settings": {}
    }
  ],
  "routing": {"rules": []},
  "outbounds": [{"protocol": "freedom", "tag": "direct"}]
}`

func writeTempFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "template.json")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	return path
}

func readRawConfig(t *testing.T, path string) RawConfig {
	t.Helper()
	cfg, err := Read(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	return cfg
}

// ---------------------------------------------------------------------------
// MergeUsers
// ---------------------------------------------------------------------------

func TestMergeUsers_UsesOnlySnapshotUsers(t *testing.T) {
	tmplPath := writeTempFile(t, minXrayTemplate)
	tmpl := readRawConfig(t, tmplPath)

	// Empty DB users — nothing should change except empty-email cleanup.
	merged, err := MergeUsers(tmpl, nil)
	if err != nil {
		t.Fatalf("MergeUsers: %v", err)
	}

	inbounds, _ := merged.GetInbounds()

	// vless inbound
	clients, _ := inbounds[0].GetClients()
	if len(clients) != 0 {
		t.Fatalf("vless: expected no users without a snapshot, got %d", len(clients))
	}
	if len(clients) > 0 && clients[0].Email() != "reverse-proxy-us" {
		t.Errorf("vless client[0]: expected reverse-proxy-us, got %q", clients[0].Email())
	}
	if len(clients) > 1 && clients[1].Email() != "admin-debug" {
		t.Errorf("vless client[1]: expected admin-debug, got %q", clients[1].Email())
	}

	// trojan inbound
	clients, _ = inbounds[1].GetClients()
	if len(clients) != 0 {
		t.Fatalf("trojan: expected no users without a snapshot, got %d", len(clients))
	}
	if len(clients) > 0 && clients[0].Email() != "reverse-proxy-us" {
		t.Errorf("trojan client[0]: expected reverse-proxy-us, got %q", clients[0].Email())
	}

	// dokodemo-door inbound — no clients at all
	if inbounds[2].HasClientList() {
		t.Error("api inbound should not have clients")
	}
}

func TestMergeUsers_AddsDBUsers(t *testing.T) {
	tmplPath := writeTempFile(t, minXrayTemplate)
	tmpl := readRawConfig(t, tmplPath)

	dbUsers := []domain.VPNUserConfig{
		{Email: "client1@example.com", UUID: "db-uuid-client1"},
		{Email: "client2@example.com", UUID: "db-uuid-client2"},
	}

	merged, err := MergeUsers(tmpl, dbUsers)
	if err != nil {
		t.Fatalf("MergeUsers: %v", err)
	}

	inbounds, _ := merged.GetInbounds()

	// vless: the desired snapshot has two users.
	clients, _ := inbounds[0].GetClients()
	if len(clients) != 2 {
		t.Fatalf("vless: expected 2 snapshot users, got %d", len(clients))
	}

	// Snapshot order is preserved.
	emails := make([]string, len(clients))
	for i, c := range clients {
		emails[i] = c.Email()
	}
	assertOrder(t, emails, "client1@example.com", "client2@example.com")

	// trojan: the same two snapshot users.
	clients, _ = inbounds[1].GetClients()
	if len(clients) != 2 {
		t.Fatalf("trojan: expected 2 clients, got %d", len(clients))
	}
}

func TestMergeUsers_DBWinsOnCollision(t *testing.T) {
	// Template has a client whose email ALSO appears in DB.
	// DB must win — the static client is replaced by the DB-generated one.
	tmplPath := writeTempFile(t, minXrayTemplate)
	tmpl := readRawConfig(t, tmplPath)

	// reverse-proxy-us exists in template → DB version replaces it
	dbUsers := []domain.VPNUserConfig{
		{Email: "reverse-proxy-us", UUID: "db-override-uuid", Flow: "xtls-rprx-vision"},
	}

	merged, err := MergeUsers(tmpl, dbUsers)
	if err != nil {
		t.Fatalf("MergeUsers: %v", err)
	}

	inbounds, _ := merged.GetInbounds()
	clients, _ := inbounds[0].GetClients()

	// vless: admin-debug (static) + reverse-proxy-us (from DB, replacing static)
	if len(clients) != 1 {
		t.Fatalf("vless: expected 1 snapshot client, got %d", len(clients))
	}

	// First client should be admin-debug (the non-colliding static)
	if clients[0].Email() != "reverse-proxy-us" {
		t.Errorf("expected reverse-proxy-us, got %q", clients[0].Email())
	}
	// It has the snapshot UUID, not the template one.
	if clients[0].GetString("id") != "db-override-uuid" {
		t.Errorf("expected DB UUID 'db-override-uuid', got %q", clients[0].GetString("id"))
	}
}

func TestMergeUsers_EmptyEmailStaticPreserved(t *testing.T) {
	// Clients with empty email are always kept as static.
	tmpl := RawConfig{}
	tmpl["inbounds"] = json.RawMessage(`[{
		"tag": "test",
		"protocol": "vless",
		"settings": {
			"clients": [
				{"email": "", "id": "orphan-uuid", "flow": "xtls-rprx-vision"}
			]
		}
	}]`)

	dbUsers := []domain.VPNUserConfig{
		{Email: "real@example.com", UUID: "real-uuid"},
	}

	merged, err := MergeUsers(tmpl, dbUsers)
	if err != nil {
		t.Fatalf("MergeUsers: %v", err)
	}

	inbounds, _ := merged.GetInbounds()
	clients, _ := inbounds[0].GetClients()

	// empty-email static + 1 DB = 2
	if len(clients) != 2 {
		t.Fatalf("expected 2 clients, got %d", len(clients))
	}
	// The empty-email client should still be there
	if clients[0].Email() != "" || clients[0].GetString("id") != "orphan-uuid" {
		t.Errorf("empty-email client was dropped: %+v", clients[0])
	}
}

func TestMergeUsers_NoClientInboundsUntouched(t *testing.T) {
	tmpl := RawConfig{}
	tmpl["inbounds"] = json.RawMessage(`[{
		"tag": "api",
		"protocol": "dokodemo-door",
		"settings": {"address": "127.0.0.1"}
	}]`)
	tmpl["routing"] = json.RawMessage(`{"domainStrategy": "AsIs"}`)

	dbUsers := []domain.VPNUserConfig{
		{Email: "someone@example.com", UUID: "uuid"},
	}

	merged, err := MergeUsers(tmpl, dbUsers)
	if err != nil {
		t.Fatalf("MergeUsers: %v", err)
	}

	// routing should survive
	if _, ok := merged["routing"]; !ok {
		t.Error("routing key lost")
	}

	inbounds, _ := merged.GetInbounds()
	if inbounds[0].HasClientList() {
		t.Error("api inbound should not gain clients")
	}
}

func TestMergeUsers_SkipsEmptyEmailDBUsers(t *testing.T) {
	tmplPath := writeTempFile(t, minXrayTemplate)
	tmpl := readRawConfig(t, tmplPath)

	dbUsers := []domain.VPNUserConfig{
		{Email: "", UUID: "bad-empty"},
		{Email: "good@example.com", UUID: "good-uuid"},
	}

	merged, err := MergeUsers(tmpl, dbUsers)
	if err != nil {
		t.Fatalf("MergeUsers: %v", err)
	}

	inbounds, _ := merged.GetInbounds()
	clients, _ := inbounds[0].GetClients()

	// The empty-email snapshot user is skipped.
	if len(clients) != 1 {
		t.Fatalf("expected 1 client, got %d", len(clients))
	}
}

func TestMergeUsers_DeduplicatesDBUsers(t *testing.T) {
	tmplPath := writeTempFile(t, minXrayTemplate)
	tmpl := readRawConfig(t, tmplPath)

	dbUsers := []domain.VPNUserConfig{
		{Email: "dup@example.com", UUID: "uuid-first"},
		{Email: "dup@example.com", UUID: "uuid-second"},
	}

	merged, err := MergeUsers(tmpl, dbUsers)
	if err != nil {
		t.Fatalf("MergeUsers: %v", err)
	}

	inbounds, _ := merged.GetInbounds()
	clients, _ := inbounds[0].GetClients()

	// Duplicate snapshot users are collapsed.
	if len(clients) != 1 {
		t.Fatalf("expected 1 client, got %d", len(clients))
	}
	if clients[0].GetString("id") != "uuid-first" {
		t.Errorf("expected first occurrence of duplicate user to win, got uuid: %q", clients[0].GetString("id"))
	}
}

func TestMergeUsers_DoesNotMutateTemplate(t *testing.T) {
	tmplPath := writeTempFile(t, minXrayTemplate)
	tmpl := readRawConfig(t, tmplPath)

	originalInboundsRaw := string(tmpl["inbounds"])

	dbUsers := []domain.VPNUserConfig{
		{Email: "new@example.com", UUID: "some-uuid"},
	}

	_, err := MergeUsers(tmpl, dbUsers)
	if err != nil {
		t.Fatalf("MergeUsers: %v", err)
	}

	if string(tmpl["inbounds"]) != originalInboundsRaw {
		t.Error("MergeUsers mutated the original template object")
	}
}

// ---------------------------------------------------------------------------
// RegenerateConfig
// ---------------------------------------------------------------------------

func TestRegenerateConfig_WritesMergedConfig(t *testing.T) {
	tmplPath := writeTempFile(t, minXrayTemplate)

	dir := t.TempDir()
	outPath := filepath.Join(dir, "config.json")

	dbUsers := []domain.VPNUserConfig{
		{Email: "client1@example.com", UUID: "uuid-1"},
	}

	err := RegenerateConfig(tmplPath, outPath, dbUsers, false, "")
	if err != nil {
		t.Fatalf("RegenerateConfig: %v", err)
	}

	// Read back and verify.
	cfg := readRawConfig(t, outPath)
	inbounds, _ := cfg.GetInbounds()
	clients, _ := inbounds[0].GetClients()

	if len(clients) != 1 {
		t.Fatalf("expected 1 snapshot client in regenerated config, got %d", len(clients))
	}
}

func TestRegenerateConfig_TemplateNotFound(t *testing.T) {
	err := RegenerateConfig("/nonexistent/template.json", "/tmp/out.json", nil, false, "")
	if err == nil {
		t.Fatal("expected error for missing template")
	}
}

func TestRegenerateConfig_BadTemplateJSON(t *testing.T) {
	dir := t.TempDir()
	badPath := filepath.Join(dir, "bad.json")
	_ = os.WriteFile(badPath, []byte(`{bad json`), 0644)

	err := RegenerateConfig(badPath, filepath.Join(dir, "out.json"), nil, false, "")
	if err == nil {
		t.Fatal("expected error for bad template JSON")
	}
}

func TestRegenerateConfig_RealityRotation(t *testing.T) {
	tmplRaw := `{
		"inbounds": [
			{
				"tag": "vless-reality",
				"protocol": "vless",
				"settings": {
					"clients": []
				},
				"streamSettings": {
					"network": "tcp",
					"security": "reality",
					"realitySettings": {
						"privateKey": "original-private-key",
						"shortIds": ["original-sid"]
					}
				}
			}
		]
	}`
	tmplPath := writeTempFile(t, tmplRaw)

	dir := t.TempDir()
	outPath := filepath.Join(dir, "config.json")
	keysPath := filepath.Join(dir, "reality.keys")

	dbUsers := []domain.VPNUserConfig{
		{Email: "user@example.com", UUID: "user-uuid"},
	}

	// 1. First run: should generate new keys and inject them
	err := RegenerateConfig(tmplPath, outPath, dbUsers, true, keysPath)
	if err != nil {
		t.Fatalf("RegenerateConfig: %v", err)
	}

	// Verify keys file was created
	keys, err := LoadOrCreateRealityKeys(keysPath)
	if err != nil {
		t.Fatalf("LoadOrCreateRealityKeys: %v", err)
	}
	if keys.PrivateKey == "" || keys.PublicKey == "" || len(keys.ShortIDs) != 15 {
		t.Errorf("generated keys file invalid: %+v", keys)
	}

	// Read generated config and verify keys/sids are injected
	cfg := readRawConfig(t, outPath)
	inbounds, _ := cfg.GetInbounds()
	if len(inbounds) != 1 {
		t.Fatalf("expected 1 inbound, got %d", len(inbounds))
	}
	ib := inbounds[0]

	// Check client merged
	clients, _ := ib.GetClients()
	if len(clients) != 1 || clients[0].Email() != "user@example.com" {
		t.Errorf("dynamic client not merged properly")
	}

	// Check streamSettings realitySettings updated
	rawStream := ib["streamSettings"]
	var stream map[string]json.RawMessage
	json.Unmarshal(rawStream, &stream) //nolint:errcheck
	var reality map[string]json.RawMessage
	json.Unmarshal(stream["realitySettings"], &reality) //nolint:errcheck

	var pkey string
	json.Unmarshal(reality["privateKey"], &pkey) //nolint:errcheck
	var sids []string
	json.Unmarshal(reality["shortIds"], &sids) //nolint:errcheck

	if pkey != keys.PrivateKey {
		t.Errorf("expected privateKey %q, got %q", keys.PrivateKey, pkey)
	}
	if len(sids) != 15 || sids[0] != keys.ShortIDs[0] {
		t.Errorf("expected shortIds %+v, got %+v", keys.ShortIDs, sids)
	}
}

func TestTemplateUsers_ExcludeBlacklistedAdmins(t *testing.T) {
	tmplPath := writeTempFile(t, minXrayTemplate)
	tmpl := readRawConfig(t, tmplPath)

	// "admin-debug" is static in minXrayTemplate. We blacklist it.
	blacklisted := []string{"admin-debug"}

	users, err := templateUsers(tmpl, nil, blacklisted)
	if err != nil {
		t.Fatalf("MergeUsers: %v", err)
	}

	if len(users) != 1 {
		t.Fatalf("expected 1 template user, got %d", len(users))
	}
	if users[0].Email != "reverse-proxy-us" {
		t.Errorf("expected reverse-proxy-us, got %q", users[0].Email)
	}
}

func TestRegenerateConfig_PreservesExistingRoutingAndOutbounds(t *testing.T) {
	tmplPath := writeTempFile(t, minXrayTemplate)

	dir := t.TempDir()
	outPath := filepath.Join(dir, "config.json")

	// Pre-populate outPath with custom routing and outbounds
	existingContent := `{
		"inbounds": [],
		"outbounds": [{"tag": "relay-NLD", "protocol": "vless"}],
		"routing": {
			"rules": [{"outboundTag": "relay-NLD", "domain": ["example.com"]}]
		}
	}`
	if err := os.WriteFile(outPath, []byte(existingContent), 0o600); err != nil {
		t.Fatalf("write existing config: %v", err)
	}

	dbUsers := []domain.VPNUserConfig{
		{Email: "client1@example.com", UUID: "uuid-1"},
	}

	err := RegenerateConfig(tmplPath, outPath, dbUsers, false, "")
	if err != nil {
		t.Fatalf("RegenerateConfig: %v", err)
	}

	// Read back and verify routing and outbounds were NOT wiped out
	cfg := readRawConfig(t, outPath)
	var routing struct {
		Rules []map[string]any `json:"rules"`
	}
	if rawRouting, ok := cfg["routing"]; !ok {
		t.Fatal("routing missing from regenerated config")
	} else if err := json.Unmarshal(rawRouting, &routing); err != nil {
		t.Fatalf("unmarshal routing: %v", err)
	}

	if len(routing.Rules) != 1 || routing.Rules[0]["outboundTag"] != "relay-NLD" {
		t.Errorf("expected routing rule to be preserved, got: %+v", routing.Rules)
	}

	var outbounds []map[string]any
	if rawOutbounds, ok := cfg["outbounds"]; !ok {
		t.Fatal("outbounds missing from regenerated config")
	} else if err := json.Unmarshal(rawOutbounds, &outbounds); err != nil {
		t.Fatalf("unmarshal outbounds: %v", err)
	}
	if len(outbounds) != 1 || outbounds[0]["tag"] != "relay-NLD" {
		t.Errorf("expected outbounds to be preserved, got: %+v", outbounds)
	}
}


// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func assertOrder(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) < len(want) {
		t.Fatalf("too few elements: got %d, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("position %d: got %q, want %q", i, got[i], w)
		}
	}
}
