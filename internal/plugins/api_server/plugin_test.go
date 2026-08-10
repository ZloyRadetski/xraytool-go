package api_server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"xraytool/internal/appconfig"
	"xraytool/internal/database"
	"xraytool/internal/pluginapi"
	"xraytool/internal/pluginhost"
	api "xraytool/internal/plugins/api_server"
	core "xraytool/internal/plugins/core"
	vpn "xraytool/internal/plugins/engine_xray"
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
	corePlugin := core.NewWithRuntime(cfg, core.Runtime{
		Registry: database.NewRegistry(db),
		Engine:   &vpn.NoopEngine{},
	})
	userPlugin := userManagement.New(cfg)
	runtimePlugin := subscriptionRuntime.New(cfg)
	apiPlugin := api.New(cfg)
	host := pluginhost.New(pluginhost.PluginsConfig{
		"core":                 {Enabled: true, Source: "builtin"},
		"user_management":      {Enabled: true, Source: "builtin"},
		"subscription_runtime": {Enabled: true, Source: "builtin"},
		"api_server":           {Enabled: true, Source: "builtin"},
	}, nil, map[string]func() pluginapi.Plugin{
		"core":                 func() pluginapi.Plugin { return corePlugin },
		"user_management":      func() pluginapi.Plugin { return userPlugin },
		"subscription_runtime": func() pluginapi.Plugin { return runtimePlugin },
		"api_server":           func() pluginapi.Plugin { return apiPlugin },
	}, nil)

	require.NoError(t, host.Load(context.Background()))
	service, err := host.ResolveService(core.ServiceUserService)
	require.NoError(t, err)
	require.Same(t, userPlugin.Service(), service)
	cache, err := host.ResolveService(core.ServiceSubscriptionCache)
	require.NoError(t, err)
	require.Same(t, runtimePlugin.CacheManager(), cache)
	handler, err := host.ResolveService(api.ServiceHTTPHandler)
	require.NoError(t, err)
	require.Same(t, apiPlugin.HTTPHandler(), handler)

	protected := corePlugin.AuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-API-Key", cfg.Server.APIKey)
	rec := httptest.NewRecorder()
	protected.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code)

	require.NoError(t, host.Shutdown(context.Background()))
}
