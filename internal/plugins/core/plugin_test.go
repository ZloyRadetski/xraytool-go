package core_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"xraytool/internal/appconfig"
	"xraytool/internal/database"
	"xraytool/internal/domain"
	"xraytool/internal/pluginapi"
	"xraytool/internal/pluginhost"
	core "xraytool/internal/plugins/core"
	vpn "xraytool/internal/plugins/engine_xray"
)

func TestCoreInitPublishesAllDeclaredServices(t *testing.T) {
	db, err := database.NewConnection(database.Config{
		Driver:      "sqlite",
		SQLitePath:  "file:core-plugin-publication?mode=memory&cache=shared",
		AutoMigrate: true,
		Silent:      true,
	})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	defer sqlDB.Close()

	registry := database.NewRegistry(db)
	p := core.NewWithRuntime(&appconfig.Config{}, core.Runtime{
		Registry: registry,
		Engine:   &vpn.NoopEngine{},
	})

	require.NoError(t, p.Init(context.Background(), nil, nil))

	services := p.PublishedServices()
	require.Len(t, services, len(p.Metadata().Publishes))
	require.IsType(t, registry, services[core.ServiceDomainRegistry])
	require.IsType(t, p, services[core.ServiceSubscriptionLifecycle])

	_, ok := services[core.ServiceDomainRegistry].(domain.Registry)
	require.True(t, ok)
	_, ok = services[core.ServiceUserRepository].(pluginapi.UserRepository)
	require.True(t, ok)
	_, ok = services[core.ServiceSubscriptionRepository].(pluginapi.SubscriptionRepository)
	require.True(t, ok)
	_, ok = services[core.ServiceDeviceRepository].(pluginapi.DeviceRepository)
	require.True(t, ok)
	_, ok = services[core.ServicePlanRepository].(pluginapi.PlanRepository)
	require.True(t, ok)
	_, ok = services[core.ServiceSubscriptionLifecycle].(core.SubscriptionLifecycle)
	require.True(t, ok)

	users := p.UserRepository()
	require.NoError(t, users.Create(context.Background(), &pluginapi.User{
		ID:       "core-plugin-user",
		Username: "core-plugin-user@example.com",
		Metadata: map[string]any{"platform": "test"},
	}))
	user, err := users.FindByID(context.Background(), "core-plugin-user")
	require.NoError(t, err)
	require.Equal(t, "core-plugin-user@example.com", user.Username)
	require.Equal(t, "test", user.Metadata["platform"])
}

var _ pluginapi.ServiceProvider = (*core.Plugin)(nil)

func TestHostLoadsAndExposesCoreProvider(t *testing.T) {
	db, err := database.NewConnection(database.Config{
		Driver:      "sqlite",
		SQLitePath:  "file:core-plugin-host?mode=memory&cache=shared",
		AutoMigrate: true,
		Silent:      true,
	})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	defer sqlDB.Close()

	p := core.NewWithRuntime(&appconfig.Config{}, core.Runtime{
		Registry: database.NewRegistry(db),
		Engine:   &vpn.NoopEngine{},
	})
	host := pluginhost.New(pluginhost.PluginsConfig{
		"core": {Enabled: true, Source: "builtin"},
	}, nil, map[string]func() pluginapi.Plugin{
		"core": func() pluginapi.Plugin { return p },
	}, nil)

	require.NoError(t, host.Load(context.Background()))
	require.Same(t, p, host.Core())

	service, err := host.ResolveService(core.ServiceUserRepository)
	require.NoError(t, err)
	_, ok := service.(pluginapi.UserRepository)
	require.True(t, ok)

	require.NoError(t, host.Shutdown(context.Background()))
}
