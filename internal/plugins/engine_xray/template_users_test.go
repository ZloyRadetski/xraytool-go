package engine_xray

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"xraytool/internal/domain"
)

func TestTemplateUserSnapshotReadsTemplateWithoutModifyingIt(t *testing.T) {
	dir := t.TempDir()
	templatePath := filepath.Join(dir, "template.json")
	template := `{
  "inbounds": [
    {"tag":"vless-main","protocol":"vless","settings":{"clients":[
      {"email":"ops-access","id":"ops-id","flow":"xtls-rprx-vision","limit":3},
      {"email":"db@example.test","id":"stale-template-id"}
    ]}},
    {"tag":"trojan-main","protocol":"trojan","settings":{"clients":[
      {"email":"ops-access","password":"ops-password"}
    ]}}
  ]
}`
	require.NoError(t, os.WriteFile(templatePath, []byte(template), 0o600))
	before, err := os.ReadFile(templatePath)
	require.NoError(t, err)

	adapter := NewAdapter("127.0.0.1:1", filepath.Join(dir, "config.json"), templatePath, false, "", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	users, err := adapter.TemplateUserSnapshot(context.Background(), []domain.VPNUserConfig{{Email: "db@example.test", UUID: "db-id"}})
	require.NoError(t, err)
	require.Equal(t, []domain.VPNUserConfig{{
		Email: "ops-access", UUID: "ops-id", Auth: "ops-password", MaxDevices: 3, Flow: "xtls-rprx-vision",
	}}, users)

	after, err := os.ReadFile(templatePath)
	require.NoError(t, err)
	require.Equal(t, before, after, "template must be read-only during snapshot creation")
}

func TestReconcileUsersUsesSnapshotAndRemovesLegacySidecar(t *testing.T) {
	dir := t.TempDir()
	templatePath := filepath.Join(dir, "slave-template.json")
	configPath := filepath.Join(dir, "config.json")
	template := `{
  "inbounds": [{"tag":"vless-main","protocol":"vless","settings":{"clients":[
    {"email":"slave-only","id":"slave-only-id"},
    {"email":"ops-access","id":"slave-old-id"}
  ]}}]
}`
	require.NoError(t, os.WriteFile(templatePath, []byte(template), 0o600))
	require.NoError(t, os.WriteFile(configPath, []byte(template), 0o600))
	require.NoError(t, os.WriteFile(configPath+".static-clients.json", []byte(`[]`), 0o600))
	before, err := os.ReadFile(templatePath)
	require.NoError(t, err)

	addr, cleanup := startMockGRPCServer(t, nil, &mockHandlerServer{})
	defer cleanup()
	adapter := NewAdapter(addr, configPath, templatePath, false, "", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	_, err = adapter.ReconcileUsers(context.Background(), []domain.VPNUserConfig{
		{Email: "db@example.test", UUID: "db-id"},
		{Email: "ops-access", UUID: "master-ops-id", Flow: "xtls-rprx-vision"},
	})
	require.NoError(t, err)

	after, err := os.ReadFile(templatePath)
	require.NoError(t, err)
	require.Equal(t, before, after, "reconciliation must not modify the template")
	_, err = os.Stat(configPath + ".static-clients.json")
	require.True(t, os.IsNotExist(err), "obsolete static-client sidecar must be removed")

	config := readRawConfig(t, configPath)
	inbounds, err := config.GetInbounds()
	require.NoError(t, err)
	clients, err := inbounds[0].GetClients()
	require.NoError(t, err)
	require.Len(t, clients, 2)
	require.Equal(t, "db@example.test", clients[0].Email())
	require.Equal(t, "ops-access", clients[1].Email())
	require.Equal(t, "master-ops-id", clients[1].GetString("id"))
}
