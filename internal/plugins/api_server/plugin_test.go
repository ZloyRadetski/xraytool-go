package api_server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"xraytool/internal/appconfig"
	"xraytool/internal/database"
	"xraytool/internal/pluginapi"
	"xraytool/internal/pluginhost"
	api "xraytool/internal/plugins/api_server"
	core "xraytool/internal/plugins/core"
	vpn "xraytool/internal/plugins/engine_xray"
	autoBalancer "xraytool/internal/plugins/subscription_autobalancer"
	subscriptionRuntime "xraytool/internal/plugins/subscription_runtime"
	userManagement "xraytool/internal/plugins/user_management"
)

func TestCoreServicesAreComposedByDedicatedPlugins(t *testing.T) {
	db, err := database.NewConnection(database.Config{
		Driver: "sqlite", SQLitePath: "file:api-server-plugin?mode=memory&cache=shared", AutoMigrate: true, Silent: true,
	})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	defer sqlDB.Close()

	cfg := &appconfig.Config{
		Mode:   "master",
		Server: appconfig.ServerConf{APIKey: "test-api-key", Domain: "example.test"},
	}
	configPath := filepath.Join(t.TempDir(), "xray.json")
	require.NoError(t, os.WriteFile(configPath, []byte(`{"inbounds":[{"settings":{"clients":[{"email":"alice@example.test","id":"alice-id","subfile":"alice"}]}}]}`), 0o600))
	corePlugin := core.NewWithRuntime(cfg, core.Runtime{
		Registry: database.NewRegistry(db),
		Engine:   &vpn.NoopEngine{},
	})
	userPlugin := userManagement.New(cfg)
	runtimePlugin := subscriptionRuntime.New(cfg)
	enginePlugin := vpn.NewFromEngine(&vpn.NoopEngine{})
	autoBalancerPlugin := autoBalancer.New()
	apiPlugin := api.New(cfg)
	host := pluginhost.New(pluginhost.PluginsConfig{
		"core":                      {Enabled: true, Source: "builtin"},
		"engine_xray":               {Enabled: true, Source: "builtin", Config: map[string]any{"config_path": configPath}},
		"user_management":           {Enabled: true, Source: "builtin"},
		"subscription_runtime":      {Enabled: true, Source: "builtin"},
		"subscription_autobalancer": {Enabled: true, Source: "builtin"},
		"api_server":                {Enabled: true, Source: "builtin"},
	}, nil, map[string]func() pluginapi.Plugin{
		"core":                      func() pluginapi.Plugin { return corePlugin },
		"engine_xray":               func() pluginapi.Plugin { return enginePlugin },
		"user_management":           func() pluginapi.Plugin { return userPlugin },
		"subscription_runtime":      func() pluginapi.Plugin { return runtimePlugin },
		"subscription_autobalancer": func() pluginapi.Plugin { return autoBalancerPlugin },
		"api_server":                func() pluginapi.Plugin { return apiPlugin },
	}, nil)

	require.NoError(t, host.Load(context.Background()))
	service, err := host.ResolveService(pluginapi.ServiceUserManagement)
	require.NoError(t, err)
	require.Same(t, userPlugin.Service(), service)
	cache, err := host.ResolveService(pluginapi.ServiceSubscriptionRuntime)
	require.NoError(t, err)
	require.Same(t, runtimePlugin.CacheManager(), cache)
	require.Same(t, autoBalancerPlugin, runtimePlugin.CacheManager().SubscriptionTemplateProcessor())
	snapshot, snapshotOK := runtimePlugin.CacheManager().SubscriptionConfigSnapshot()
	require.True(t, snapshotOK)
	require.Len(t, snapshot.ActiveClients, 1)
	require.Equal(t, "alice@example.test", snapshot.ActiveClients[0].Email)
	handler, err := host.ResolveService(pluginapi.ServiceHTTPHandler)
	require.NoError(t, err)
	require.Same(t, apiPlugin.HTTPHandler(), handler)

	protected := apiPlugin.ProtectedMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-API-Key", cfg.Server.APIKey)
	rec := httptest.NewRecorder()
	protected.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code)

	require.NoError(t, host.Shutdown(context.Background()))
}
