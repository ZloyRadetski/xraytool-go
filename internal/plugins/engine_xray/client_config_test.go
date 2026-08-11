package engine_xray

import (
	"context"
	"encoding/base64"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"xraytool/internal/pluginapi"
)

func TestBuildClientLinks_UsesXrayInboundConfiguration(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "xray.json")
	require.NoError(t, os.WriteFile(configPath, []byte(`{
  "inbounds": [
    {
      "tag": "VLESS Reality",
      "port": 443,
      "protocol": "vless",
      "settings": {"clients": [{
        "email": "alice@example.test",
        "id": "11111111-2222-3333-4444-555555555555",
        "flow": "xtls-rprx-vision"
      }]},
      "streamSettings": {
        "network": "tcp",
        "security": "reality",
        "realitySettings": {
          "publicKey": "public-key",
          "serverNames": ["cdn.example.test"],
          "shortIds": ["abcd"],
          "fingerprint": "chrome"
        }
      }
    },
    {
      "tag": "ss2022-in",
      "port": 8443,
      "protocol": "shadowsocks",
      "settings": {
        "method": "2022-blake3-aes-256-gcm",
        "password": "server-secret",
        "clients": [{"email": "alice@example.test", "password": "user-secret"}]
      }
    },
    {
      "tag": "hy2-in",
      "port": 8444,
      "protocol": "hysteria2",
      "settings": {
        "clients": [{"email": "alice@example.test", "auth": "hy2-secret"}],
        "obfs": {"password": "obfs-secret"},
        "tls": {"sni": "hy2.example.test"}
      }
    }
  ]
}`), 0o600))

	p := NewFromEngine(&NoopEngine{})
	require.NoError(t, p.Init(context.Background(), pluginapi.RawConfig{
		"config_path":    configPath,
		"server_address": "example.test",
	}, nil))

	links, err := p.BuildClientLinks(context.Background(), pluginapi.VPNUserConfig{
		Email: "alice@example.test",
		UUID:  "11111111-2222-3333-4444-555555555555",
	})
	require.NoError(t, err)
	require.Len(t, links, 3)

	require.Equal(t, "vless", links[0].Protocol)
	vlessURL, err := url.Parse(links[0].URI)
	require.NoError(t, err)
	require.Equal(t, "vless", vlessURL.Scheme)
	require.Equal(t, "example.test:443", vlessURL.Host)
	require.Equal(t, "11111111-2222-3333-4444-555555555555", vlessURL.User.Username())
	require.Equal(t, "reality", vlessURL.Query().Get("security"))
	require.Equal(t, "public-key", vlessURL.Query().Get("pbk"))
	require.Equal(t, "abcd", vlessURL.Query().Get("sid"))
	require.Equal(t, "cdn.example.test", vlessURL.Query().Get("sni"))
	require.Equal(t, "xtls-rprx-vision", vlessURL.Query().Get("flow"))

	require.Equal(t, "shadowsocks", links[1].Protocol)
	ssParts := strings.SplitN(strings.TrimPrefix(links[1].URI, "ss://"), "@", 2)
	require.Len(t, ssParts, 2)
	credentials, err := base64.StdEncoding.DecodeString(ssParts[0])
	require.NoError(t, err)
	require.Equal(t, "2022-blake3-aes-256-gcm:server-secret:user-secret", string(credentials))
	require.Contains(t, ssParts[1], "example.test:8443")

	require.Equal(t, "hysteria2", links[2].Protocol)
	hy2URL, err := url.Parse(links[2].URI)
	require.NoError(t, err)
	require.Equal(t, "hysteria2", hy2URL.Scheme)
	require.Equal(t, "hy2-secret", hy2URL.User.Username())
	require.Equal(t, "example.test:8444", hy2URL.Host)
	require.Equal(t, "hy2.example.test", hy2URL.Query().Get("sni"))
	require.Equal(t, "salamander", hy2URL.Query().Get("obfs"))
	require.Equal(t, "obfs-secret", hy2URL.Query().Get("obfs-password"))
}

func TestBuildClientLinks_RequiresPluginOwnedEndpoint(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "xray.json")
	require.NoError(t, os.WriteFile(configPath, []byte(`{"inbounds":[]}`), 0o600))
	p := NewFromEngine(&NoopEngine{})
	require.NoError(t, p.Init(context.Background(), pluginapi.RawConfig{"config_path": configPath}, nil))

	_, err := p.BuildClientLinks(context.Background(), pluginapi.VPNUserConfig{})
	require.ErrorContains(t, err, "server_address")
}

func TestSubscriptionConfigSnapshot_ProjectsOnlySubscriptionData(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "xray.json")
	templatePath := filepath.Join(dir, "template.json")
	require.NoError(t, os.WriteFile(configPath, []byte(`{
  "inbounds": [{
    "tag": "vless", "protocol": "vless",
    "settings": {"clients": [{"email": "alice@example.test", "id": "user-id", "subfile": "alice", "limit": 7}]},
    "streamSettings": {"realitySettings": {"publicKey": "public-key", "serverNames": ["cdn.example.test"], "shortIds": ["one", "two"]}}
  }, {
    "tag": "ss2022-in", "protocol": "shadowsocks", "settings": {"password": "server-secret"}
  }, {
    "tag": "hy2", "protocol": "hysteria2", "settings": {"obfs": {"password": "obfs-secret"}}
  }]
}`), 0o600))
	require.NoError(t, os.WriteFile(templatePath, []byte(`{"inbounds":[{"settings":{"clients":[{"email":"admin@example.test","id":"admin-id","subfile":"admin"}]}}]}`), 0o600))

	p := NewFromEngine(&NoopEngine{})
	require.NoError(t, p.Init(context.Background(), pluginapi.RawConfig{
		"config_path": configPath, "template_path": templatePath,
	}, nil))

	snapshot, err := p.SubscriptionConfigSnapshot(context.Background())
	require.NoError(t, err)
	require.Len(t, snapshot.ActiveClients, 1)
	require.Equal(t, "alice@example.test", snapshot.ActiveClients[0].Email)
	require.Equal(t, 7, snapshot.ActiveClients[0].MaxDevices)
	require.Len(t, snapshot.TemplateClients, 1)
	require.Equal(t, "admin@example.test", snapshot.TemplateClients[0].Email)
	require.Equal(t, "public-key", snapshot.RealityPublicKey)
	require.Equal(t, []string{"one", "two"}, snapshot.RealityShortIDs)
	require.Equal(t, "cdn.example.test", snapshot.RealityServerName)
	require.Equal(t, "server-secret", snapshot.ShadowSocksSecret)
	require.Equal(t, "obfs-secret", snapshot.HysteriaObfsSecret)
	require.NotZero(t, snapshot.Revision)
}
