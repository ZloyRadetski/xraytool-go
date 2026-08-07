package pluginhost

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"xraytool/internal/pluginapi"
)

type dummyEngineProvider struct {
	id string
}

func (e *dummyEngineProvider) Metadata() pluginapi.Metadata                                { return pluginapi.Metadata{} }
func (e *dummyEngineProvider) Init(context.Context, pluginapi.RawConfig, pluginapi.ServiceResolver) error { return nil }
func (e *dummyEngineProvider) Start(context.Context) error                                 { return nil }
func (e *dummyEngineProvider) Stop(context.Context) error                                  { return nil }
func (e *dummyEngineProvider) Health(context.Context) error                                { return nil }
func (e *dummyEngineProvider) ID() string                                                  { return e.id }
func (e *dummyEngineProvider) BuildClientLinks(context.Context, pluginapi.VPNUserConfig) ([]pluginapi.ClientLink, error) { return nil, nil }
func (e *dummyEngineProvider) AddUser(context.Context, pluginapi.VPNUserConfig) error      { return nil }
func (e *dummyEngineProvider) AddUsersBulk(context.Context, []pluginapi.VPNUserConfig) error { return nil }
func (e *dummyEngineProvider) RemoveUser(context.Context, string) error                    { return nil }
func (e *dummyEngineProvider) RemoveUsersBulk(context.Context, []string) error             { return nil }
func (e *dummyEngineProvider) SetExpire(context.Context, string, string) error             { return nil }
func (e *dummyEngineProvider) SetLimit(context.Context, string, float64) error             { return nil }
func (e *dummyEngineProvider) RebuildInbound(context.Context, string) error                { return nil }
func (e *dummyEngineProvider) QueryStats(context.Context) ([]pluginapi.TrafficStat, error) { return nil, nil }
func (e *dummyEngineProvider) BanUser(context.Context, string) error                       { return nil }
func (e *dummyEngineProvider) UnbanUser(context.Context, string) error                     { return nil }
func (e *dummyEngineProvider) RestartLogger(context.Context) error                         { return nil }
func (e *dummyEngineProvider) ListUsers(context.Context) ([]pluginapi.VPNUserConfig, error) { return nil, nil }
func (e *dummyEngineProvider) SyncUsers(context.Context, []pluginapi.VPNUserConfig, bool) (*pluginapi.EngineSyncResult, error) { return nil, nil }

func TestNewConfiguredEngineRouter_Validations(t *testing.T) {
	t.Run("defaults to broadcast on empty mode", func(t *testing.T) {
		router, err := NewConfiguredEngineRouter("", nil)
		require.NoError(t, err)
		require.Equal(t, RoutingModeBroadcast, router.Mode())
	})

	t.Run("rejects unsupported mode", func(t *testing.T) {
		_, err := NewConfiguredEngineRouter("unsupported-mode", nil)
		require.Error(t, err)
		require.Contains(t, err.Error(), "unsupported engine routing mode")
	})

	t.Run("rejects duplicate engine ID", func(t *testing.T) {
		providers := []pluginapi.EngineProvider{
			&dummyEngineProvider{id: "xray"},
			&dummyEngineProvider{id: "xray"},
		}
		_, err := NewConfiguredEngineRouter("broadcast", providers)
		require.Error(t, err)
		require.Contains(t, err.Error(), "duplicate routing provider ID \"xray\"")
	})
}

func TestConfiguredEngineRouter_EnginesForChecked(t *testing.T) {
	providers := []pluginapi.EngineProvider{
		&dummyEngineProvider{id: "xray"},
		&dummyEngineProvider{id: "singbox"},
	}

	routerByPlan, err := NewConfiguredEngineRouter("by-plan", providers)
	require.NoError(t, err)

	routerBroadcast, err := NewConfiguredEngineRouter("broadcast", providers)
	require.NoError(t, err)

	t.Run("subscription override takes precedence in broadcast mode", func(t *testing.T) {
		user := pluginapi.VPNUserConfig{
			SubscriptionEngineIDs: []string{"singbox"},
			PlanEngineIDs:         []string{"xray"},
		}
		selected, err := routerBroadcast.EnginesForChecked(user)
		require.NoError(t, err)
		require.Len(t, selected, 1)
		require.Equal(t, "singbox", selected[0].ID())
	})

	t.Run("subscription override takes precedence in by-plan mode", func(t *testing.T) {
		user := pluginapi.VPNUserConfig{
			SubscriptionEngineIDs: []string{"xray"},
			PlanEngineIDs:         []string{"singbox"},
		}
		selected, err := routerByPlan.EnginesForChecked(user)
		require.NoError(t, err)
		require.Len(t, selected, 1)
		require.Equal(t, "xray", selected[0].ID())
	})

	t.Run("by-plan selects plan engines", func(t *testing.T) {
		user := pluginapi.VPNUserConfig{
			PlanEngineIDs: []string{"singbox", "xray"},
		}
		selected, err := routerByPlan.EnginesForChecked(user)
		require.NoError(t, err)
		require.Len(t, selected, 2)
		require.Equal(t, "xray", selected[0].ID()) // preserves registered provider order
		require.Equal(t, "singbox", selected[1].ID())
	})

	t.Run("broadcast fallback when no overrides or plan ids", func(t *testing.T) {
		user := pluginapi.VPNUserConfig{}
		selected, err := routerBroadcast.EnginesForChecked(user)
		require.NoError(t, err)
		require.Len(t, selected, 2) // all enabled
	})

	t.Run("by-plan fallback to broadcast when no plan ids", func(t *testing.T) {
		user := pluginapi.VPNUserConfig{}
		selected, err := routerByPlan.EnginesForChecked(user)
		require.NoError(t, err)
		require.Len(t, selected, 2) // fallback to all
	})

	t.Run("reports error for unknown engine ID", func(t *testing.T) {
		user := pluginapi.VPNUserConfig{
			SubscriptionEngineIDs: []string{"xray", "unknown-engine"},
		}
		_, err := routerBroadcast.EnginesForChecked(user)
		require.Error(t, err)
		require.Contains(t, err.Error(), "references unloaded engine IDs: unknown-engine")
	})
}
