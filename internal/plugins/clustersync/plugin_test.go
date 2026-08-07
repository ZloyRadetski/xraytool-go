package clustersync

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"xraytool/internal/domain"
	"xraytool/internal/pluginapi"
	vpn "xraytool/internal/plugins/engine_xray"
)

func TestMetadataDeclaresCoreDependencyAndPublishedProvider(t *testing.T) {
	t.Parallel()

	meta := New().Metadata()
	require.Equal(t, "cluster_sync", meta.Name)
	require.Equal(t, "cluster_sync", meta.Kind)
	require.False(t, meta.Mandatory)
	require.Equal(t, []pluginapi.ServiceRef{{Name: "subscription_repository"}}, meta.Requires)
	require.Equal(t, []pluginapi.ServiceRef{{Name: ServiceClusterSyncProvider}}, meta.Publishes)
}

func TestInitPublishesClusterSyncProvider(t *testing.T) {
	t.Parallel()

	p := NewWithRuntime(testRuntime(&stateSyncProviderStub{}, &statsProviderStub{}))
	resolver := resolverStub{services: map[string]any{
		"subscription_repository": subscriptionRepoStub{},
	}}

	require.NoError(t, p.Init(context.Background(), pluginapi.RawConfig{
		"sync_interval": "5m",
		"sync_on_start": false,
	}, resolver))
	require.NoError(t, p.Health(context.Background()))
	require.Equal(t, Config{SyncInterval: 5 * time.Minute, SyncOnStart: false}, p.Config())

	services := p.PublishedServices()
	require.Len(t, services, 1)
	require.Same(t, p, services[ServiceClusterSyncProvider])
}

func TestInitRejectsMissingOrWrongSubscriptionRepository(t *testing.T) {
	t.Parallel()

	missing := NewWithRuntime(testRuntime(&stateSyncProviderStub{}, &statsProviderStub{}))
	err := missing.Init(context.Background(), nil, resolverStub{})
	require.ErrorContains(t, err, "resolve subscription_repository")

	wrongType := NewWithRuntime(testRuntime(&stateSyncProviderStub{}, &statsProviderStub{}))
	err = wrongType.Init(context.Background(), nil, resolverStub{services: map[string]any{
		"subscription_repository": "not-a-repository",
	}})
	require.ErrorContains(t, err, "unexpected type string")
}

func TestInitRejectsIncompleteRuntime(t *testing.T) {
	t.Parallel()

	p := New()
	err := p.Init(context.Background(), nil, nil)
	require.ErrorContains(t, err, "runtime Registry is required")
}

func TestParseConfig(t *testing.T) {
	t.Parallel()

	cfg, err := parseConfig(nil)
	require.NoError(t, err)
	require.Equal(t, defaultSyncInterval, cfg.SyncInterval)
	require.True(t, cfg.SyncOnStart)

	cfg, err = parseConfig(pluginapi.RawConfig{
		"sync_interval": time.Minute,
		"sync_on_start": false,
	})
	require.NoError(t, err)
	require.Equal(t, time.Minute, cfg.SyncInterval)
	require.False(t, cfg.SyncOnStart)

	_, err = parseConfig(pluginapi.RawConfig{"sync_interval": "never"})
	require.ErrorContains(t, err, "invalid sync_interval")
	_, err = parseConfig(pluginapi.RawConfig{"sync_interval": "0s"})
	require.ErrorContains(t, err, "must be positive")
	_, err = parseConfig(pluginapi.RawConfig{"sync_on_start": "yes"})
	require.ErrorContains(t, err, "must be a boolean")
}

func TestSyncAllSlavesDelegatesAndConvertsResults(t *testing.T) {
	t.Parallel()

	provider := &stateSyncProviderStub{results: []domain.SyncResult{{
		ServerName: "edge-1",
		Success:    true,
	}}}
	p := NewWithRuntime(testRuntime(provider, &statsProviderStub{}))
	require.NoError(t, p.Init(context.Background(), nil, nil))

	results, err := p.SyncAllSlaves(context.Background(), true, true)
	require.NoError(t, err)
	require.Equal(t, 1, provider.calls)
	require.True(t, provider.dryRun)
	require.True(t, provider.forceFull)
	require.Equal(t, []pluginapi.SyncResult{{ServerName: "edge-1", Success: true}}, results)
}

func TestCollectSlaveTotalsDelegatesAndConvertsValues(t *testing.T) {
	t.Parallel()

	stats := &statsProviderStub{
		totals: []domain.SlaveUserTotal{{Email: "user@example.test", Slave: 42}},
		report: domain.SlaveReport{
			Enabled:       true,
			TotalServers:  2,
			OKServers:     1,
			FailedServers: 1,
		},
	}
	p := NewWithRuntime(testRuntime(&stateSyncProviderStub{}, stats))
	require.NoError(t, p.Init(context.Background(), nil, nil))

	totals, report := p.CollectSlaveTotals()
	require.Equal(t, []pluginapi.SlaveUserTotal{{Email: "user@example.test", Slave: 42}}, totals)
	require.Equal(t, pluginapi.SlaveReport{
		Enabled:       true,
		TotalServers:  2,
		OKServers:     1,
		FailedServers: 1,
	}, report)
}

func TestStartExitsWhenContextIsCancelled(t *testing.T) {
	t.Parallel()

	p := NewWithRuntime(testRuntime(&stateSyncProviderStub{}, &statsProviderStub{}))
	require.NoError(t, p.Init(context.Background(), pluginapi.RawConfig{
		"sync_interval": "1h",
		"sync_on_start": false,
	}, nil))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Start(ctx) }()
	cancel()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("Start did not return after its context was cancelled")
	}
}

func TestHealthAndOperationsBeforeInitFailSafely(t *testing.T) {
	t.Parallel()

	p := New()
	require.Error(t, p.Health(context.Background()))
	_, err := p.SyncAllSlaves(context.Background(), false, false)
	require.ErrorContains(t, err, "not initialised")
	totals, report := p.CollectSlaveTotals()
	require.Nil(t, totals)
	require.Equal(t, pluginapi.SlaveReport{}, report)
	_, err = p.BuildSnapshot(context.Background())
	require.ErrorContains(t, err, "not initialised")
	_, err = p.MasterState(context.Background())
	require.ErrorContains(t, err, "not initialised")
	require.ErrorContains(t, p.PropagateCommand(context.Background(), "rmuser", nil), "not initialised")
}

func TestPropagateCommandUsesRuntimePort(t *testing.T) {
	t.Parallel()

	propagator := &eventPropagatorStub{}
	runtime := testRuntime(&stateSyncProviderStub{}, &statsProviderStub{})
	runtime.Propagator = propagator
	p := NewWithRuntime(runtime)
	require.NoError(t, p.Init(context.Background(), nil, nil))

	require.NoError(t, p.PropagateCommand(context.Background(), "rmuser", map[string]string{"email": "person@example.test"}))
	require.Equal(t, "rmuser", propagator.command)
	require.Equal(t, "person@example.test", propagator.params["email"])
	require.ErrorContains(t, p.PropagateCommand(context.Background(), "", nil), "must not be empty")
}

func testRuntime(syncProvider domain.StateSyncSlaveProvider, statsProvider domain.ClusterStatsProvider) Runtime {
	return Runtime{
		Registry:      &registryStub{},
		Engine:        &vpn.NoopEngine{},
		SlaveProvider: syncProvider,
		StatsProvider: statsProvider,
	}
}

// registryStub is sufficient for Init and SyncAllSlaves; the latter delegates
// directly to the state-sync provider and does not read repositories. Tests
// that exercise the worker's self-healing belong to internal/statesync.
type registryStub struct{ domain.Registry }

type subscriptionRepoStub struct {
	pluginapi.SubscriptionRepository
}

type stateSyncProviderStub struct {
	results   []domain.SyncResult
	err       error
	calls     int
	dryRun    bool
	forceFull bool
}

func (s *stateSyncProviderStub) SyncAllSlaves(_ context.Context, dryRun bool, forceFull bool) ([]domain.SyncResult, error) {
	s.calls++
	s.dryRun = dryRun
	s.forceFull = forceFull
	return s.results, s.err
}

type statsProviderStub struct {
	totals []domain.SlaveUserTotal
	report domain.SlaveReport
}

type eventPropagatorStub struct {
	command string
	params  map[string]string
}

func (s *eventPropagatorStub) PropagateAll(command string, params map[string]string) {
	s.command = command
	s.params = make(map[string]string, len(params))
	for key, value := range params {
		s.params[key] = value
	}
}

func (s *statsProviderStub) CollectSlaveTotals() ([]domain.SlaveUserTotal, domain.SlaveReport) {
	return s.totals, s.report
}

type resolverStub struct {
	services map[string]any
	err      error
}

func (r resolverStub) Resolve(name string) (any, error) {
	if r.err != nil {
		return nil, r.err
	}
	service, ok := r.services[name]
	if !ok {
		return nil, fmt.Errorf("service %q is unavailable", name)
	}
	return service, nil
}

func (resolverStub) Logger() pluginapi.Logger                         { return noopLogger{} }
func (resolverStub) EmitEvent(string, map[string]any, map[string]any) {}
func (resolverStub) DB() pluginapi.PluginDBHandle                     { return nil }

type noopLogger struct{}

func (noopLogger) Debug(string, ...any) {}
func (noopLogger) Info(string, ...any)  {}
func (noopLogger) Warn(string, ...any)  {}
func (noopLogger) Error(string, ...any) {}
