package subscription

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"xraytool/internal/appconfig"
	"xraytool/internal/pluginapi"
	vpn "xraytool/internal/plugins/engine_xray"
)

type contributorEngine struct {
	vpn.NoopEngine
	received pluginapi.VPNUserConfig
}

func (e *contributorEngine) BuildClientLinks(_ context.Context, user pluginapi.VPNUserConfig) ([]pluginapi.ClientLink, error) {
	e.received = user
	return []pluginapi.ClientLink{
		{Protocol: "vless", URI: "vless://first"},
		{Protocol: "hysteria2", URI: "  hysteria2://second  "},
	}, nil
}

func TestCacheManager_UsesEngineAgnosticClientConfigContributor(t *testing.T) {
	engine := &contributorEngine{}
	cache := NewCacheManager(&appconfig.Config{}, engine)

	links, available, err := cache.BuildClientLinks(context.Background(), pluginapi.VPNUserConfig{
		Email: "alice@example.test",
		UUID:  "user-uuid",
	})
	require.NoError(t, err)
	require.True(t, available)
	require.Equal(t, "alice@example.test", engine.received.Email)
	require.Equal(t, "user-uuid", engine.received.UUID)
	require.Equal(t, "vless://first\nhysteria2://second", clientLinksText(links))
}

func TestCacheManager_ClientConfigContributorIsOptional(t *testing.T) {
	cache := NewCacheManager(&appconfig.Config{}, &vpn.NoopEngine{})
	links, available, err := cache.BuildClientLinks(context.Background(), pluginapi.VPNUserConfig{})
	require.NoError(t, err)
	require.False(t, available)
	require.Nil(t, links)
}
