package clusterreplication_test

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"xraytool/internal/database"
	"xraytool/internal/domain"
	"xraytool/internal/pluginapi"
	"xraytool/internal/pluginhost"
	clusterreplication "xraytool/internal/plugins/cluster_replication"
	vpn "xraytool/internal/plugins/engine_xray"
)

type hostCoreStub struct {
	registry domain.Registry
	engine   domain.Engine
}

func (*hostCoreStub) Metadata() pluginapi.Metadata {
	return pluginapi.Metadata{
		Name: "core", Kind: "core", APIVersion: pluginapi.CurrentAPIVersion, Mandatory: true,
		Publishes: []pluginapi.ServiceRef{{Name: "domain_registry"}, {Name: "domain_engine"}},
	}
}
func (*hostCoreStub) Init(context.Context, pluginapi.RawConfig, pluginapi.ServiceResolver) error {
	return nil
}
func (*hostCoreStub) Start(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
func (*hostCoreStub) Stop(context.Context) error      { return nil }
func (*hostCoreStub) Health(context.Context) error    { return nil }
func (s *hostCoreStub) PublishedServices() map[string]any {
	return map[string]any{"domain_registry": s.registry, "domain_engine": s.engine}
}

func TestPluginHostPublishesResolvableReplicationService(t *testing.T) {
	db, err := database.NewConnection(database.Config{
		Driver: "sqlite", SQLitePath: "file:cluster-replication-host?mode=memory&cache=shared", Silent: true,
	})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	registry := database.NewRegistry(db)
	replication := clusterreplication.NewWithRuntime(clusterreplication.Runtime{Registry: registry, Engine: &vpn.NoopEngine{}, Logger: slog.Default()})
	host := pluginhost.New(
		pluginhost.PluginsConfig{
			"core": {Enabled: true, Source: "builtin"},
			"cluster_replication": {Enabled: true, Source: "builtin", Config: map[string]any{
				"mode": "slave", "node_id": "slave-a", "master_address": "127.0.0.1:9443",
				"ca_file": "unused-ca.pem", "cert_file": "unused-cert.pem", "key_file": "unused-key.pem",
			}},
		},
		slog.Default(),
		map[string]func() pluginapi.Plugin{
			"core":                func() pluginapi.Plugin { return &hostCoreStub{registry: registry, engine: &vpn.NoopEngine{}} },
			"cluster_replication": func() pluginapi.Plugin { return replication },
		},
		nil,
		pluginhost.WithPluginDBFactory(database.NewPluginDBFactory(db)),
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, host.Load(ctx))
	service, err := host.ResolveService(clusterreplication.ServiceClusterReplicationProvider)
	require.NoError(t, err)
	require.Same(t, replication, service)
	require.NoError(t, host.Shutdown(context.Background()))
}
