package cmd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"xraytool/internal/appconfig"
	"xraytool/internal/domain"
	"xraytool/internal/pluginapi"
	"xraytool/internal/pluginhost"
)

type engineProviderStub struct {
	pluginapi.EngineProvider
	id    string
	added []pluginapi.VPNUserConfig
}

func (s *engineProviderStub) ID() string { return s.id }

func (s *engineProviderStub) AddUser(_ context.Context, user pluginapi.VPNUserConfig) error {
	s.added = append(s.added, user)
	return nil
}

var _ pluginapi.EngineProvider = (*engineProviderStub)(nil)

func TestBuildPluginsConfig_UsesTypedPluginAndEngineSections(t *testing.T) {
	cfg := &appconfig.Config{
		Plugins: appconfig.PluginsConf{
			"core": {
				Enabled: true,
				Source:  "builtin",
				Config:  map[string]any{"core_option": "value"},
			},
			"mailer_resend": {
				Enabled: true,
				Source:  "builtin",
				Config:  map[string]any{"from_email": "noreply@example.test"},
			},
			"cluster_sync": {
				Enabled: true,
				Source:  "builtin",
				Config:  map[string]any{"sync_interval": "3m"},
			},
		},
		Engines: appconfig.EnginesConf{Entries: map[string]appconfig.PluginConf{
			"xray": {
				Enabled: true,
				Source:  "builtin",
				Config:  map[string]any{"grpc_addr": "127.0.0.1:10085"},
			},
		}},
	}

	plugins := buildPluginsConfig(cfg)
	require.Len(t, plugins, 4)
	require.True(t, plugins["core"].Enabled)
	require.Equal(t, "value", plugins["core"].Config["core_option"])
	require.True(t, plugins["mailer_resend"].Enabled)
	require.True(t, plugins["cluster_sync"].Enabled)
	require.True(t, plugins["engine_xray"].Enabled)
	require.Equal(t, "127.0.0.1:10085", plugins["engine_xray"].Config["grpc_addr"])

	// The host must not retain a map it can accidentally mutate in appconfig.
	plugins["engine_xray"].Config["grpc_addr"] = "changed"
	require.Equal(t, "127.0.0.1:10085", cfg.Engines.Entries["xray"].Config["grpc_addr"])
}

func TestPrepareMultiEngine_ReusesInstancesReturnedToHost(t *testing.T) {
	instance := &engineProviderStub{id: "xray"}
	factories := map[string]func() pluginapi.Plugin{
		"engine_xray": func() pluginapi.Plugin { return instance },
	}

	multi, err := prepareMultiEngine(pluginhost.PluginsConfig{
		"engine_xray": {Enabled: true, Source: "builtin"},
	}, factories, nil)
	require.NoError(t, err)
	require.NotNil(t, multi)
	require.NoError(t, multi.Validate())
	require.Same(t, instance, factories["engine_xray"]())
}

func TestPrepareMultiEngine_AppliesConfiguredRoutingMode(t *testing.T) {
	xray := &engineProviderStub{id: "xray"}
	singbox := &engineProviderStub{id: "singbox"}
	factories := map[string]func() pluginapi.Plugin{
		"engine_xray":    func() pluginapi.Plugin { return xray },
		"engine_singbox": func() pluginapi.Plugin { return singbox },
	}

	multi, err := prepareMultiEngine(pluginhost.PluginsConfig{
		"engine_xray":    {Enabled: true, Source: "builtin"},
		"engine_singbox": {Enabled: true, Source: "builtin"},
	}, factories, nil, "by-plan")
	require.NoError(t, err)

	require.NoError(t, multi.AddUser(context.Background(), domain.VPNUserConfig{
		Email:         "person@example.test",
		PlanEngineIDs: []string{"singbox"},
	}))
	require.Empty(t, xray.added)
	require.Equal(t, []pluginapi.VPNUserConfig{{
		Email:         "person@example.test",
		PlanEngineIDs: []string{"singbox"},
	}}, singbox.added)
}
