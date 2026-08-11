package engine_xray

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"xraytool/internal/domain"
)

const staticClientMasterTemplate = `{
  "inbounds": [
    {
      "tag": "vless-main",
      "protocol": "vless",
      "settings": {"clients": [
        {"email": "ops-access", "id": "static-new", "flow": "xtls-rprx-vision", "level": 7},
        {"email": "db-user@example.test", "id": "db-template-id"}
      ]}
    },
    {
      "tag": "trojan-main",
      "protocol": "trojan",
      "settings": {"clients": [
        {"email": "ops-access", "password": "static-password", "custom": {"keep": true}},
        {"email": "db-user@example.test", "password": "db-template-password"}
      ]}
    }
  ]
}`

const staticClientSlaveTemplate = `{
  "inbounds": [
    {
      "tag": "vless-main",
      "protocol": "vless",
      "settings": {"clients": [{"email": "ops-access", "id": "static-old"}]}
    },
    {
      "tag": "trojan-main",
      "protocol": "trojan",
      "settings": {"clients": [{"email": "ops-access", "password": "old-password"}]}
    }
  ]
}`

const staticClientSlaveConfig = `{
  "inbounds": [
    {
      "tag": "vless-main",
      "protocol": "vless",
      "settings": {"clients": [
        {"email": "ops-access", "id": "static-old"},
        {"email": "db-user@example.test", "id": "db-live-id", "subfile": "db-subfile"}
      ]}
    },
    {
      "tag": "trojan-main",
      "protocol": "trojan",
      "settings": {"clients": [
        {"email": "ops-access", "password": "old-password"},
        {"email": "db-user@example.test", "password": "db-live-password", "subfile": "db-subfile"}
      ]}
    }
  ]
}`

func TestStaticClientSnapshotAndApplyPreservesDynamicClients(t *testing.T) {
	dir := t.TempDir()
	masterTemplatePath := filepath.Join(dir, "master-template.json")
	if err := os.WriteFile(masterTemplatePath, []byte(staticClientMasterTemplate), 0o600); err != nil {
		t.Fatalf("write master template: %v", err)
	}

	master := NewAdapter("127.0.0.1:1", filepath.Join(dir, "master-config.json"), masterTemplatePath, false, "", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	snapshot, err := master.StaticClientSnapshot(context.Background(), []domain.VPNUserConfig{{Email: "db-user@example.test"}})
	if err != nil {
		t.Fatalf("StaticClientSnapshot: %v", err)
	}
	if len(snapshot) != 2 {
		t.Fatalf("expected two static inbound snapshots, got %d", len(snapshot))
	}
	if string(snapshot[0].Clients) == "" || strings.Contains(string(snapshot[0].Clients), "db-template-id") {
		t.Fatalf("database client leaked into static snapshot: %s", snapshot[0].Clients)
	}
	if !strings.Contains(string(snapshot[0].Clients), `"level":7`) {
		t.Fatalf("protocol-specific static field was lost: %s", snapshot[0].Clients)
	}

	slaveTemplatePath := filepath.Join(dir, "slave-template.json")
	slaveConfigPath := filepath.Join(dir, "slave-config.json")
	if err := os.WriteFile(slaveTemplatePath, []byte(staticClientSlaveTemplate), 0o600); err != nil {
		t.Fatalf("write slave template: %v", err)
	}
	if err := os.WriteFile(slaveConfigPath, []byte(staticClientSlaveConfig), 0o600); err != nil {
		t.Fatalf("write slave config: %v", err)
	}

	addr, cleanup := startMockGRPCServer(t, nil, &mockHandlerServer{})
	defer cleanup()
	slave := NewAdapter(addr, slaveConfigPath, slaveTemplatePath, false, "", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := slave.ApplyStaticClientSnapshot(ctx, snapshot); err != nil {
		t.Fatalf("ApplyStaticClientSnapshot: %v", err)
	}

	template := readRawConfig(t, slaveTemplatePath)
	vlessTemplateClients := clientsForInbound(t, template, "vless-main")
	if len(vlessTemplateClients) != 1 || vlessTemplateClients[0].GetString("id") != "static-new" {
		t.Fatalf("slave template did not receive the static VLESS client: %+v", vlessTemplateClients)
	}
	if level, ok := vlessTemplateClients[0].GetNumber("level"); !ok || level != 7 {
		t.Fatalf("static VLESS fields were not preserved: %+v", vlessTemplateClients[0])
	}

	active := readRawConfig(t, slaveConfigPath)
	vlessActive := clientsForInbound(t, active, "vless-main")
	if clientByEmail(vlessActive, "db-user@example.test").GetString("subfile") != "db-subfile" {
		t.Fatalf("database-managed VLESS client was changed: %+v", vlessActive)
	}
	if clientByEmail(vlessActive, "ops-access").GetString("id") != "static-new" {
		t.Fatalf("static VLESS client was not updated: %+v", vlessActive)
	}

	trojanActive := clientsForInbound(t, active, "trojan-main")
	if clientByEmail(trojanActive, "db-user@example.test").GetString("subfile") != "db-subfile" {
		t.Fatalf("database-managed Trojan client was changed: %+v", trojanActive)
	}
	if clientByEmail(trojanActive, "ops-access").GetString("password") != "static-password" {
		t.Fatalf("static Trojan client was not updated: %+v", trojanActive)
	}
}

func TestStaticClientSnapshotProtectsDirectConfigClients(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, []byte(staticClientMasterTemplate), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	adapter := NewAdapter("127.0.0.1:1", configPath, "", false, "", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	_, err := adapter.StaticClientSnapshot(context.Background(), []domain.VPNUserConfig{{Email: "db-user@example.test"}})
	if err != nil {
		t.Fatalf("StaticClientSnapshot: %v", err)
	}
	if _, err := os.Stat(configPath + ".static-clients.json"); err != nil {
		t.Fatalf("direct static client state was not written: %v", err)
	}

	protected := adapter.getProtectedTemplateUsers(map[string]bool{"db-user@example.test": true})
	if !protected["ops-access"] {
		t.Fatal("hardcoded direct-config client is not protected")
	}
	if protected["db-user@example.test"] {
		t.Fatal("database client was incorrectly marked as static")
	}
}

func TestStaticClientSnapshotProjectsUserToNodeSpecificInbounds(t *testing.T) {
	dir := t.TempDir()
	masterTemplatePath := filepath.Join(dir, "master-template.json")
	masterTemplate := `{
  "inbounds": [
    {"tag":"master-vless","protocol":"vless","settings":{"clients":[{"email":"ops-access","id":"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa","level":7}]}}
  ]
}`
	if err := os.WriteFile(masterTemplatePath, []byte(masterTemplate), 0o600); err != nil {
		t.Fatalf("write master template: %v", err)
	}
	master := NewAdapter("127.0.0.1:1", filepath.Join(dir, "master-config.json"), masterTemplatePath, false, "", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	snapshot, err := master.StaticClientSnapshot(context.Background(), nil)
	if err != nil {
		t.Fatalf("snapshot master static clients: %v", err)
	}

	// Tags and protocols intentionally differ from the master. The replicated
	// hardcoded user must still be generated for every local client inbound.
	slaveTemplatePath := filepath.Join(dir, "slave-template.json")
	slaveConfigPath := filepath.Join(dir, "slave-config.json")
	slaveTemplate := `{
  "inbounds": [
    {"tag":"msk-xhttp","protocol":"xhttp","settings":{"clients":[]}},
    {"tag":"msk-trojan","protocol":"trojan","settings":{"clients":[]}},
    {"tag":"msk-hy2","protocol":"hy2","settings":{"users":[]}}
  ]
}`
	if err := os.WriteFile(slaveTemplatePath, []byte(slaveTemplate), 0o600); err != nil {
		t.Fatalf("write slave template: %v", err)
	}
	if err := os.WriteFile(slaveConfigPath, []byte(slaveTemplate), 0o600); err != nil {
		t.Fatalf("write slave config: %v", err)
	}

	addr, cleanup := startMockGRPCServer(t, nil, &mockHandlerServer{})
	defer cleanup()
	slave := NewAdapter(addr, slaveConfigPath, slaveTemplatePath, false, "", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := slave.ApplyStaticClientSnapshot(ctx, snapshot); err != nil {
		t.Fatalf("apply node-specific static clients: %v", err)
	}

	active := readRawConfig(t, slaveConfigPath)
	xhttp := clientByEmail(clientsForInbound(t, active, "msk-xhttp"), "ops-access")
	if xhttp == nil || xhttp.GetString("id") != "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa" {
		t.Fatalf("static user was not generated for local xhttp inbound: %+v", xhttp)
	}
	trojan := clientByEmail(clientsForInbound(t, active, "msk-trojan"), "ops-access")
	if trojan == nil || trojan.GetString("password") != "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa" {
		t.Fatalf("static user was not generated for local trojan inbound: %+v", trojan)
	}
	hy2 := clientByEmail(clientsForInbound(t, active, "msk-hy2"), "ops-access")
	if hy2 == nil || hy2.GetString("auth") != BuildDeterministicHy2Pass("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "ops-access") {
		t.Fatalf("static user was not generated for local hy2 inbound: %+v", hy2)
	}
}

func clientsForInbound(t *testing.T, cfg RawConfig, tag string) []RawClient {
	t.Helper()
	inbounds, err := cfg.GetInbounds()
	if err != nil {
		t.Fatalf("GetInbounds: %v", err)
	}
	for _, inbound := range inbounds {
		if inbound.Tag() != tag {
			continue
		}
		clients, err := inbound.GetClients()
		if err != nil {
			t.Fatalf("GetClients for %s: %v", tag, err)
		}
		return clients
	}
	t.Fatalf("inbound %q was not found", tag)
	return nil
}

func clientByEmail(clients []RawClient, email string) RawClient {
	for _, client := range clients {
		if client.Email() == email {
			return client
		}
	}
	return nil
}
