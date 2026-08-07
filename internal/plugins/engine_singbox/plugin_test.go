package engine_singbox

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"xraytool/internal/pluginapi"
)

const testSingboxConfig = `{
  "inbounds": [
    {
      "type": "vless",
      "tag": "vless-in",
      "listen": "0.0.0.0",
      "listen_port": 443,
      "tls": {"enabled": true, "server_name": "edge.example.test"},
      "users": [{"name": "legacy@example.test", "uuid": "legacy-uuid"}]
    },
    {
      "type": "hysteria2",
      "tag": "hy2-in",
      "listen_port": 8443,
      "tls": {"enabled": true, "server_name": "hy2.example.test"},
      "users": [{"name": "legacy@example.test", "password": "legacy-secret"}]
    },
    {
      "type": "shadowsocks",
      "tag": "ss-in",
      "listen_port": 8388,
      "method": "2022-blake3-aes-128-gcm",
      "users": []
    }
  ]
}`

func writeTestConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sing-box.json")
	require.NoError(t, os.WriteFile(path, []byte(testSingboxConfig), 0o600))
	return path
}

func newTestPlugin(t *testing.T, configPath string, extra pluginapi.RawConfig, runner CommandRunner) *Plugin {
	t.Helper()
	if runner == nil {
		runner = CommandRunnerFunc(func(context.Context, string, ...string) error { return nil })
	}
	cfg := pluginapi.RawConfig{
		"config_path":    configPath,
		"server_address": "vpn.example.test",
		"reload_command": []string{"reload"},
	}
	for key, value := range extra {
		cfg[key] = value
	}
	p := NewWithRunner(runner)
	require.NoError(t, p.Init(context.Background(), cfg, nil))
	return p
}

func readTestDocument(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	document, err := decodeConfig(data)
	require.NoError(t, err)
	return document
}

func TestPluginMutatesNativeSingboxUsersAndBuildsLinks(t *testing.T) {
	configPath := writeTestConfig(t)
	var mu sync.Mutex
	var calls []string
	p := newTestPlugin(t, configPath, pluginapi.RawConfig{
		"check_command": []string{"check", "{config_path}"},
	}, CommandRunnerFunc(func(_ context.Context, command string, args ...string) error {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, command+" "+strings.Join(args, " "))
		return nil
	}))

	user := pluginapi.VPNUserConfig{
		Email:      "person@example.test",
		UUID:       "user-uuid",
		Auth:       "user-secret",
		Cipher:     "2022-blake3-aes-128-gcm",
		Flow:       "xtls-rprx-vision",
		Expire:     "01.01.2030",
		MaxDevices: 5,
	}
	require.NoError(t, p.AddUser(context.Background(), user))

	document := readTestDocument(t, configPath)
	inbounds := managedInbounds(document, nil)
	require.Len(t, inbounds, 3)
	for _, inbound := range inbounds {
		found := false
		for _, native := range inboundUsers(inbound) {
			if userEmail(native) == user.Email {
				found = true
				metadata, _ := native["xraytool"].(map[string]any)
				require.Equal(t, "01.01.2030", metadata["expire"])
			}
		}
		require.Truef(t, found, "inbound %q does not contain added user", mapString(inbound, "tag"))
	}

	links, err := p.BuildClientLinks(context.Background(), user)
	require.NoError(t, err)
	require.Len(t, links, 3)
	require.Contains(t, links[0].URI, "vless://user-uuid@vpn.example.test:443")
	require.Contains(t, links[1].URI, "hysteria2://user-secret@vpn.example.test:8443")
	require.True(t, strings.HasPrefix(links[2].URI, "ss://"))

	users, err := p.ListUsers(context.Background())
	require.NoError(t, err)
	require.Len(t, users, 2)
	require.Equal(t, user.Email, users[1].Email)

	require.NoError(t, p.SetLimit(context.Background(), user.Email, 7))
	require.NoError(t, p.SetExpire(context.Background(), user.Email, "02.02.2030"))
	require.NoError(t, p.BanUser(context.Background(), user.Email))
	users, err = p.ListUsers(context.Background())
	require.NoError(t, err)
	require.Len(t, users, 1)
	require.NoError(t, p.UnbanUser(context.Background(), user.Email))
	users, err = p.ListUsers(context.Background())
	require.NoError(t, err)
	require.Len(t, users, 2)

	mu.Lock()
	defer mu.Unlock()
	// AddUser runs configured check+reload. Subsequent mutable operations run
	// reload too; this verifies the plugin genuinely crosses the configured
	// process boundary rather than only editing an in-memory config.
	require.GreaterOrEqual(t, len(calls), 6)
	require.Contains(t, calls[0], "check ")
	require.Contains(t, calls[1], "reload")
}

func TestPluginSyncUsersReconcilesAllManagedInbounds(t *testing.T) {
	configPath := writeTestConfig(t)
	p := newTestPlugin(t, configPath, nil, nil)

	result, err := p.SyncUsers(context.Background(), []pluginapi.VPNUserConfig{
		{Email: "one@example.test", UUID: "one", Auth: "secret-one"},
		{Email: "two@example.test", UUID: "two", Auth: "secret-two"},
	}, true)
	require.NoError(t, err)
	require.Equal(t, &pluginapi.EngineSyncResult{Added: 2, Removed: 1}, result)

	document := readTestDocument(t, configPath)
	for _, inbound := range managedInbounds(document, nil) {
		users := inboundUsers(inbound)
		require.Len(t, users, 2)
		require.Equal(t, "one@example.test", userEmail(users[0]))
		require.Equal(t, "two@example.test", userEmail(users[1]))
	}
}

func TestPluginSyncUsersUsesConfiguredTemplate(t *testing.T) {
	configPath := writeTestConfig(t)
	currentConfig := strings.Replace(testSingboxConfig, `"inbounds": [`, `"current_only": true, "inbounds": [`, 1)
	require.NoError(t, os.WriteFile(configPath, []byte(currentConfig), 0o600))

	templatePath := filepath.Join(t.TempDir(), "sing-box-template.json")
	templateConfig := strings.Replace(testSingboxConfig, `"inbounds": [`, `"template_only": true, "inbounds": [`, 1)
	require.NoError(t, os.WriteFile(templatePath, []byte(templateConfig), 0o600))

	p := newTestPlugin(t, configPath, pluginapi.RawConfig{"template_path": templatePath}, nil)
	result, err := p.SyncUsers(context.Background(), []pluginapi.VPNUserConfig{
		{Email: "one@example.test", UUID: "one", Auth: "secret-one"},
	}, true)
	require.NoError(t, err)
	require.Equal(t, &pluginapi.EngineSyncResult{Added: 1, Removed: 1}, result)

	document := readTestDocument(t, configPath)
	require.Equal(t, true, document["template_only"])
	_, hasCurrentOnly := document["current_only"]
	require.False(t, hasCurrentOnly)
	for _, inbound := range managedInbounds(document, nil) {
		users := inboundUsers(inbound)
		require.Len(t, users, 1)
		require.Equal(t, "one@example.test", userEmail(users[0]))
	}
}

func TestPluginRestoresConfigWhenReloadFails(t *testing.T) {
	configPath := writeTestConfig(t)
	original, err := os.ReadFile(configPath)
	require.NoError(t, err)
	p := newTestPlugin(t, configPath, nil, CommandRunnerFunc(func(_ context.Context, command string, _ ...string) error {
		if command == "reload" {
			return errors.New("service manager rejected reload")
		}
		return nil
	}))

	err = p.AddUser(context.Background(), pluginapi.VPNUserConfig{Email: "person@example.test", UUID: "uuid"})
	require.ErrorContains(t, err, "rejected reload")
	got, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.Equal(t, original, got)
}

func TestPluginReadsStatsEndpoint(t *testing.T) {
	statsServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		require.Equal(t, "/stats", request.URL.Path)
		_, _ = writer.Write([]byte(`{"users":[{"email":"person@example.test","upload":12,"download":34}]}`))
	}))
	defer statsServer.Close()

	p := newTestPlugin(t, writeTestConfig(t), pluginapi.RawConfig{
		"stats_endpoint": statsServer.URL + "/stats",
	}, nil)
	stats, err := p.QueryStats(context.Background())
	require.NoError(t, err)
	require.Equal(t, []pluginapi.TrafficStat{{Email: "person@example.test", Up: 12, Down: 34}}, stats)
}

func TestPluginStatsAreOptional(t *testing.T) {
	p := newTestPlugin(t, writeTestConfig(t), nil, nil)
	_, err := p.QueryStats(context.Background())
	require.ErrorIs(t, err, ErrStatsUnavailable)
}
