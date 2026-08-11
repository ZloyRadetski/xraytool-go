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

type snapshotEngine struct {
	vpn.NoopEngine
	snapshot pluginapi.SubscriptionConfigSnapshot
}

func (e *snapshotEngine) SubscriptionConfigSnapshot(context.Context) (pluginapi.SubscriptionConfigSnapshot, error) {
	return e.snapshot, nil
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

func TestCacheManager_UsesEngineSubscriptionConfigSnapshot(t *testing.T) {
	engine := &snapshotEngine{snapshot: pluginapi.SubscriptionConfigSnapshot{
		Revision: 1,
		ActiveClients: []pluginapi.SubscriptionClient{{
			Email: "alice@example.test", ID: "uuid", Subfile: "Alice.txt", Password: "ss-secret", Auth: "hy2-secret", MaxDevices: 5,
		}},
	}}
	cache := NewCacheManager(&appconfig.Config{}, engine)
	cache.Refresh()

	user := cache.GetUserBySubfile("alice")
	require.NotNil(t, user)
	require.Equal(t, "alice@example.test", user.Email)
	require.Equal(t, "ss-secret", user.Password)
	require.Equal(t, "hy2-secret", user.Hy2Auth)
	require.Equal(t, 5, user.Limit)

	snapshot, ok := cache.SubscriptionConfigSnapshot()
	require.True(t, ok)
	snapshot.ActiveClients[0].Email = "changed@example.test"
	stored, ok := cache.SubscriptionConfigSnapshot()
	require.True(t, ok)
	require.Equal(t, "alice@example.test", stored.ActiveClients[0].Email)
}
