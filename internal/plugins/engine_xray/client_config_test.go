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
