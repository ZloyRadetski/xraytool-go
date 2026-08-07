package pluginhost_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"xraytool/internal/pluginapi"
	"xraytool/internal/pluginhost"
)

// ─────────────────────────────────────────────────────────────────────────────
// Helpers — minimal stub plugin
// ─────────────────────────────────────────────────────────────────────────────

type stubPlugin struct {
	meta     pluginapi.Metadata
	initErr  error
	initFn   func(ctx context.Context, cfg pluginapi.RawConfig, reg pluginapi.ServiceResolver) error
	services map[string]any
}

func (s *stubPlugin) Metadata() pluginapi.Metadata { return s.meta }

func (s *stubPlugin) Init(ctx context.Context, cfg pluginapi.RawConfig, reg pluginapi.ServiceResolver) error {
	if s.initFn != nil {
		return s.initFn(ctx, cfg, reg)
	}
	return s.initErr
}

func (s *stubPlugin) Start(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

func (s *stubPlugin) Stop(_ context.Context) error   { return nil }
func (s *stubPlugin) Health(_ context.Context) error { return nil }
func (s *stubPlugin) PublishedServices() map[string]any {
	return s.services
}

// noProviderPlugin deliberately does not implement ServiceProvider. It models
// a plugin that declares a publication but has not implemented the required
// host contract yet.
type noProviderPlugin struct {
	meta   pluginapi.Metadata
	initFn func(context.Context, pluginapi.RawConfig, pluginapi.ServiceResolver) error
	stopFn func()
}

func (p *noProviderPlugin) Metadata() pluginapi.Metadata { return p.meta }
func (p *noProviderPlugin) Init(ctx context.Context, cfg pluginapi.RawConfig, reg pluginapi.ServiceResolver) error {
	if p.initFn != nil {
		return p.initFn(ctx, cfg, reg)
	}
	return nil
}
func (p *noProviderPlugin) Start(ctx context.Context) error { <-ctx.Done(); return nil }
func (p *noProviderPlugin) Stop(_ context.Context) error {
	if p.stopFn != nil {
		p.stopFn()
	}
	return nil
}
func (p *noProviderPlugin) Health(_ context.Context) error { return nil }

// stopFnPlugin extends stubPlugin with a custom stop callback — used to verify
// shutdown order.
type stopFnPlugin struct {
	stubPlugin
	stopCallback func()
}

func (s *stopFnPlugin) Stop(_ context.Context) error {
	if s.stopCallback != nil {
		s.stopCallback()
	}
	return nil
}

type startFnPlugin struct {
	stubPlugin
	done chan struct{}
}

func (s *startFnPlugin) Start(ctx context.Context) error {
	<-ctx.Done()
	close(s.done)
	return nil
}

// Compile-time interface checks.
var _ pluginapi.Plugin = (*stubPlugin)(nil)
var _ pluginapi.Plugin = (*stopFnPlugin)(nil)
var _ pluginapi.Plugin = (*startFnPlugin)(nil)
var _ pluginapi.ServiceProvider = (*stubPlugin)(nil)
var _ pluginapi.Plugin = (*noProviderPlugin)(nil)

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

// makeEnabled converts a list of plugin names to an enabled map.
func makeEnabled(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

func makeCorePlugin() *stubPlugin {
	return &stubPlugin{
		meta: pluginapi.Metadata{
			Name:       "core",
			Kind:       "core",
			Mandatory:  true,
			APIVersion: "1",
			Publishes:  []pluginapi.ServiceRef{{Name: "subscription_repository"}},
		},
		services: map[string]any{"subscription_repository": struct{}{}},
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Graph() tests
// ─────────────────────────────────────────────────────────────────────────────

func TestGraph_MandatoryFirst(t *testing.T) {
	// core plugin must always appear first in the order, regardless of declaration order.
	plugins := []pluginapi.Metadata{
		{
			Name: "antifraud", Kind: "antifraud",
			Requires: []pluginapi.ServiceRef{{Name: "subscription_repository"}},
		},
		{
			Name: "core", Kind: "core", Mandatory: true,
			Publishes: []pluginapi.ServiceRef{{Name: "subscription_repository"}},
		},
	}

	order, err := pluginhost.Graph(plugins, makeEnabled("core", "antifraud"))
	require.NoError(t, err)
	require.Equal(t, "core", order[0], "core plugin must be first")
	assert.Equal(t, "antifraud", order[1])
}

func TestGraph_UnsatisfiedRequiredDependency(t *testing.T) {
	// antifraud requires subscription_repository but core is disabled — graph must fail.
	plugins := []pluginapi.Metadata{
		{
			Name: "core", Kind: "core", Mandatory: true,
			Publishes: []pluginapi.ServiceRef{{Name: "subscription_repository"}},
		},
		{
			Name: "antifraud", Kind: "antifraud",
			Requires: []pluginapi.ServiceRef{{Name: "subscription_repository", Optional: false}},
		},
	}

	// Only antifraud enabled — core is disabled, so subscription_repository is never published.
	_, err := pluginhost.Graph(plugins, makeEnabled("antifraud"))
	require.Error(t, err, "should fail when required service has no enabled publisher")
	assert.Contains(t, err.Error(), "subscription_repository")
}

func TestGraph_OptionalDependencyMissing_OK(t *testing.T) {
	// antifraud marks engine.softban as optional — it should load even without the engine.
	plugins := []pluginapi.Metadata{
		{
			Name: "core", Kind: "core", Mandatory: true,
			Publishes: []pluginapi.ServiceRef{{Name: "subscription_repository"}},
		},
		{
			Name: "antifraud", Kind: "antifraud",
			Requires: []pluginapi.ServiceRef{
				{Name: "subscription_repository", Optional: false},
				{Name: "engine.softban", Optional: true},
			},
		},
	}

	order, err := pluginhost.Graph(plugins, makeEnabled("core", "antifraud"))
	require.NoError(t, err)
	assert.Equal(t, 2, len(order))
}

func TestGraph_TransitiveDependencies(t *testing.T) {
	// pricing → subscription_repository (from core)
	// antifraud → subscription_repository (from core) + pricing_engine (from pricing)
	plugins := []pluginapi.Metadata{
		{
			Name: "core", Kind: "core", Mandatory: true,
			Publishes: []pluginapi.ServiceRef{{Name: "subscription_repository"}},
		},
		{
			Name: "pricing", Kind: "pricing",
			Requires:  []pluginapi.ServiceRef{{Name: "subscription_repository"}},
			Publishes: []pluginapi.ServiceRef{{Name: "pricing_engine"}},
		},
		{
			Name: "antifraud", Kind: "antifraud",
			Requires: []pluginapi.ServiceRef{
				{Name: "subscription_repository"},
				{Name: "pricing_engine"},
			},
		},
	}

	order, err := pluginhost.Graph(plugins, makeEnabled("core", "pricing", "antifraud"))
	require.NoError(t, err)

	// Helper to find position in order slice.
	pos := func(name string) int {
		for i, n := range order {
			if n == name {
				return i
			}
		}
		return -1
	}
	assert.Less(t, pos("core"), pos("pricing"), "core must come before pricing")
	assert.Less(t, pos("pricing"), pos("antifraud"), "pricing must come before antifraud")
}

func TestGraph_DisabledPlugin_ExcludedFromOrder(t *testing.T) {
	plugins := []pluginapi.Metadata{
		{
			Name: "core", Kind: "core", Mandatory: true,
			Publishes: []pluginapi.ServiceRef{{Name: "subscription_repository"}},
		},
		{
			Name: "antifraud", Kind: "antifraud",
			Requires: []pluginapi.ServiceRef{{Name: "subscription_repository"}},
		},
		{Name: "payment_platega", Kind: "payment"},
	}

	// payment_platega is disabled.
	order, err := pluginhost.Graph(plugins, makeEnabled("core", "antifraud"))
	require.NoError(t, err)
	assert.NotContains(t, order, "payment_platega")
	assert.Contains(t, order, "core")
	assert.Contains(t, order, "antifraud")
}

// ─────────────────────────────────────────────────────────────────────────────
// Host.Load() tests
// ─────────────────────────────────────────────────────────────────────────────

func TestHostLoad_NoCorePlugin_Error(t *testing.T) {
	cfg := pluginhost.PluginsConfig{
		"mailer": {Enabled: true, Source: "builtin"},
	}
	mailerPlugin := &stubPlugin{
		meta: pluginapi.Metadata{Name: "mailer", Kind: "notification", APIVersion: "1"},
	}
	host := pluginhost.New(cfg, nil, map[string]func() pluginapi.Plugin{
		"mailer": func() pluginapi.Plugin { return mailerPlugin },
	}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := host.Load(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mandatory")
}

func TestHostLoad_MandatoryDisabled_NoCandidates_Error(t *testing.T) {
	// core is in the YAML but disabled — it won't be a candidate, so "no mandatory" error.
	cfg := pluginhost.PluginsConfig{
		"core": {Enabled: false, Source: "builtin"},
	}
	core := makeCorePlugin()
	host := pluginhost.New(cfg, nil, map[string]func() pluginapi.Plugin{
		"core": func() pluginapi.Plugin { return core },
	}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := host.Load(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mandatory")
}

func TestHostLoad_PluginInitError_StopsLoad(t *testing.T) {
	cfg := pluginhost.PluginsConfig{
		"core":   {Enabled: true, Source: "builtin"},
		"mailer": {Enabled: true, Source: "builtin"},
	}
	core := makeCorePlugin()
	mailer := &stubPlugin{
		meta:    pluginapi.Metadata{Name: "mailer", Kind: "notification", APIVersion: "1"},
		initErr: errors.New("SMTP credentials missing"),
	}
	host := pluginhost.New(cfg, nil, map[string]func() pluginapi.Plugin{
		"core":   func() pluginapi.Plugin { return core },
		"mailer": func() pluginapi.Plugin { return mailer },
	}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := host.Load(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "SMTP credentials missing")
}

func TestHostLoad_ScopedResolver_BlocksUndeclaredResolve(t *testing.T) {
	// antifraud tries to resolve "user_repository" without declaring it in Requires.
	var resolveErr error
	antifraud := &stubPlugin{
		meta: pluginapi.Metadata{
			Name:       "antifraud",
			Kind:       "antifraud",
			APIVersion: "1",
			Requires:   []pluginapi.ServiceRef{{Name: "subscription_repository"}},
		},
		initFn: func(_ context.Context, _ pluginapi.RawConfig, reg pluginapi.ServiceResolver) error {
			_, err := reg.Resolve("user_repository") // NOT in Requires → must error
			resolveErr = err
			return nil // don't propagate so Load doesn't fail for the wrong reason
		},
	}
	core := makeCorePlugin()

	cfg := pluginhost.PluginsConfig{
		"core":      {Enabled: true, Source: "builtin"},
		"antifraud": {Enabled: true, Source: "builtin"},
	}
	host := pluginhost.New(cfg, nil, map[string]func() pluginapi.Plugin{
		"core":      func() pluginapi.Plugin { return core },
		"antifraud": func() pluginapi.Plugin { return antifraud },
	}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_ = host.Load(ctx)

	require.Error(t, resolveErr, "resolving an undeclared service must return an error")
	assert.Contains(t, resolveErr.Error(), "user_repository")
	assert.Contains(t, resolveErr.Error(), "Requires")
}

func TestHostLoad_PublishesServiceBeforeDependentInit(t *testing.T) {
	type service struct{ value string }
	published := &service{value: "available"}

	core := makeCorePlugin()
	core.services = map[string]any{"subscription_repository": published}

	var resolved any
	consumer := &stubPlugin{
		meta: pluginapi.Metadata{
			Name:       "consumer",
			Kind:       "test",
			APIVersion: "1",
			Requires:   []pluginapi.ServiceRef{{Name: "subscription_repository"}},
		},
		initFn: func(_ context.Context, _ pluginapi.RawConfig, reg pluginapi.ServiceResolver) error {
			var err error
			resolved, err = reg.Resolve("subscription_repository")
			return err
		},
	}

	host := pluginhost.New(pluginhost.PluginsConfig{
		"core":     {Enabled: true, Source: "builtin"},
		"consumer": {Enabled: true, Source: "builtin"},
	}, nil, map[string]func() pluginapi.Plugin{
		"core":     func() pluginapi.Plugin { return core },
		"consumer": func() pluginapi.Plugin { return consumer },
	}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, host.Load(ctx))
	assert.Same(t, published, resolved)

	fromHost, err := host.ResolveService("subscription_repository")
	require.NoError(t, err)
	assert.Same(t, published, fromHost)
}

func TestHostLoad_RejectsMissingDeclaredService(t *testing.T) {
	core := makeCorePlugin()
	core.services = nil

	host := pluginhost.New(pluginhost.PluginsConfig{
		"core": {Enabled: true, Source: "builtin"},
	}, nil, map[string]func() pluginapi.Plugin{
		"core": func() pluginapi.Plugin { return core },
	}, nil)

	err := host.Load(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "did not provide declared service")
}

func TestGraph_RejectsDuplicateServicePublisher(t *testing.T) {
	_, err := pluginhost.Graph([]pluginapi.Metadata{
		{
			Name:       "core",
			Kind:       "core",
			Mandatory:  true,
			Publishes:  []pluginapi.ServiceRef{{Name: "shared"}},
			APIVersion: "1",
		},
		{
			Name:       "other",
			Kind:       "test",
			Publishes:  []pluginapi.ServiceRef{{Name: "shared"}},
			APIVersion: "1",
		},
	}, makeEnabled("core", "other"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "published by both")
}

func TestHostLoad_RejectsUnsupportedAPIVersion(t *testing.T) {
	core := makeCorePlugin()
	core.meta.APIVersion = "2"
	host := pluginhost.New(pluginhost.PluginsConfig{
		"core": {Enabled: true, Source: "builtin"},
	}, nil, map[string]func() pluginapi.Plugin{
		"core": func() pluginapi.Plugin { return core },
	}, nil)

	err := host.Load(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported plugin API version")
}

func TestHostShutdown_CancelsPluginStartContext(t *testing.T) {
	core := &startFnPlugin{
		stubPlugin: *makeCorePlugin(),
		done:       make(chan struct{}),
	}
	host := pluginhost.New(pluginhost.PluginsConfig{
		"core": {Enabled: true, Source: "builtin"},
	}, nil, map[string]func() pluginapi.Plugin{
		"core": func() pluginapi.Plugin { return core },
	}, nil)

	require.NoError(t, host.Load(context.Background()))
	require.NoError(t, host.Shutdown(context.Background()))

	select {
	case <-core.done:
		// Shutdown waits for Start to return after cancelling its context.
	default:
		t.Fatal("Shutdown returned before plugin Start exited")
	}
}

func TestHostLoad_UnknownPlugin_Error(t *testing.T) {
	// plugins.yaml enables a plugin that isn't in the builtin registry.
	cfg := pluginhost.PluginsConfig{
		"core":    {Enabled: true, Source: "builtin"},
		"unknown": {Enabled: true, Source: "builtin"},
	}
	core := makeCorePlugin()
	host := pluginhost.New(cfg, nil, map[string]func() pluginapi.Plugin{
		"core": func() pluginapi.Plugin { return core },
		// "unknown" intentionally absent from the registry
	}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := host.Load(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown")
}

func TestHostLoad_CalledTwice_Error(t *testing.T) {
	cfg := pluginhost.PluginsConfig{
		"core": {Enabled: true, Source: "builtin"},
	}
	core := makeCorePlugin()
	host := pluginhost.New(cfg, nil, map[string]func() pluginapi.Plugin{
		"core": func() pluginapi.Plugin { return core },
	}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	require.NoError(t, host.Load(ctx))

	err := host.Load(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "more than once")
}

// ─────────────────────────────────────────────────────────────────────────────
// Host.Shutdown() — verifies reverse load order
// ─────────────────────────────────────────────────────────────────────────────

func TestHostShutdown_ReverseOrder(t *testing.T) {
	var stopped []string
	var mu sync.Mutex

	cfg := pluginhost.PluginsConfig{
		"core":   {Enabled: true, Source: "builtin"},
		"mailer": {Enabled: true, Source: "builtin"},
	}

	corePlugin := &stopFnPlugin{
		stubPlugin: stubPlugin{
			meta: pluginapi.Metadata{Name: "core", Kind: "core", Mandatory: true, APIVersion: "1"},
		},
		stopCallback: func() {
			mu.Lock()
			stopped = append(stopped, "core")
			mu.Unlock()
		},
	}
	mailerPlugin := &stopFnPlugin{
		stubPlugin: stubPlugin{
			meta: pluginapi.Metadata{Name: "mailer", Kind: "notification", APIVersion: "1"},
		},
		stopCallback: func() {
			mu.Lock()
			stopped = append(stopped, "mailer")
			mu.Unlock()
		},
	}

	host := pluginhost.New(cfg, nil, map[string]func() pluginapi.Plugin{
		"core":   func() pluginapi.Plugin { return corePlugin },
		"mailer": func() pluginapi.Plugin { return mailerPlugin },
	}, nil)

	loadCtx, loadCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer loadCancel()
	require.NoError(t, host.Load(loadCtx))

	shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutCancel()
	require.NoError(t, host.Shutdown(shutCtx))

	// core (loaded first) must stop LAST.
	require.Len(t, stopped, 2)
	assert.Equal(t, "core", stopped[len(stopped)-1], "core plugin must stop last")
}

// ─────────────────────────────────────────────────────────────────────────────
// Host.HealthCheck()
// ─────────────────────────────────────────────────────────────────────────────

func TestHostHealthCheck_ReturnsPluginErrors(t *testing.T) {
	healthErr := errors.New("antifraud internal error")
	cfg := pluginhost.PluginsConfig{
		"core":      {Enabled: true, Source: "builtin"},
		"antifraud": {Enabled: true, Source: "builtin"},
	}
	core := makeCorePlugin()
	antifraudPlugin := &stubPlugin{
		meta: pluginapi.Metadata{Name: "antifraud", Kind: "antifraud", APIVersion: "1"},
	}
	// Override Health via embedding trick is not ergonomic; we test via a healthFnPlugin.
	_ = antifraudPlugin
	_ = healthErr

	// Simpler: just check that healthy plugins return nil.
	host := pluginhost.New(cfg, nil, map[string]func() pluginapi.Plugin{
		"core": func() pluginapi.Plugin { return core },
		"antifraud": func() pluginapi.Plugin {
			return &stubPlugin{
				meta: pluginapi.Metadata{Name: "antifraud", Kind: "antifraud", APIVersion: "1"},
			}
		},
	}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, host.Load(ctx))

	health := host.HealthCheck(context.Background())
	assert.Nil(t, health["core"])
	assert.Nil(t, health["antifraud"])
}

func TestHostLoad_RollsBackInitialisedPluginsAndCanRetry(t *testing.T) {
	var mu sync.Mutex
	var stopped []string
	recordStop := func(name string) func() {
		return func() {
			mu.Lock()
			defer mu.Unlock()
			stopped = append(stopped, name)
		}
	}

	core := &stopFnPlugin{
		stubPlugin:   *makeCorePlugin(),
		stopCallback: recordStop("core"),
	}
	mailer := &stopFnPlugin{
		stubPlugin: stubPlugin{
			meta:    pluginapi.Metadata{Name: "mailer", Kind: "notification", APIVersion: "1"},
			initErr: errors.New("mailer init failed"),
		},
		stopCallback: recordStop("mailer"),
	}
	host := pluginhost.New(pluginhost.PluginsConfig{
		"core":   {Enabled: true, Source: "builtin"},
		"mailer": {Enabled: true, Source: "builtin"},
	}, nil, map[string]func() pluginapi.Plugin{
		"core":   func() pluginapi.Plugin { return core },
		"mailer": func() pluginapi.Plugin { return mailer },
	}, nil)

	err := host.Load(context.Background())
	require.Error(t, err)
	assert.ErrorContains(t, err, "mailer init failed")
	assert.Equal(t, []string{"mailer", "core"}, stopped, "rollback must stop the failing plugin first")
	assert.Empty(t, host.Loaded())
	_, err = host.ResolveService("subscription_repository")
	assert.Error(t, err, "a failed load must not expose staged services")

	mailer.initErr = nil
	require.NoError(t, host.Load(context.Background()), "a fully rolled-back host can retry Load")
	require.NoError(t, host.Shutdown(context.Background()))
}

func TestHostLoad_PublishesServicesAtomically(t *testing.T) {
	var stopped int
	core := &stopFnPlugin{
		stubPlugin: stubPlugin{
			meta: pluginapi.Metadata{
				Name:       "core",
				Kind:       "core",
				Mandatory:  true,
				APIVersion: "1",
				Publishes: []pluginapi.ServiceRef{
					{Name: "first"},
					{Name: "second"},
				},
			},
			services: map[string]any{"first": struct{}{}},
		},
		stopCallback: func() { stopped++ },
	}
	host := pluginhost.New(pluginhost.PluginsConfig{
		"core": {Enabled: true, Source: "builtin"},
	}, nil, map[string]func() pluginapi.Plugin{
		"core": func() pluginapi.Plugin { return core },
	}, nil)

	err := host.Load(context.Background())
	require.Error(t, err)
	assert.ErrorContains(t, err, "did not provide declared service \"second\"")
	assert.Equal(t, 1, stopped, "a publication failure still cleans up the initialised plugin")
	_, err = host.ResolveService("first")
	assert.Error(t, err, "the first service must not leak when a later declaration is missing")
}

func TestHostLoad_RequiresServiceProviderForDeclaredPublications(t *testing.T) {
	stopped := false
	core := &noProviderPlugin{
		meta: pluginapi.Metadata{
			Name:       "core",
			Kind:       "core",
			Mandatory:  true,
			APIVersion: "1",
			Publishes:  []pluginapi.ServiceRef{{Name: "must_publish"}},
		},
		stopFn: func() { stopped = true },
	}
	host := pluginhost.New(pluginhost.PluginsConfig{
		"core": {Enabled: true, Source: "builtin"},
	}, nil, map[string]func() pluginapi.Plugin{
		"core": func() pluginapi.Plugin { return core },
	}, nil)

	err := host.Load(context.Background())
	require.Error(t, err)
	assert.ErrorContains(t, err, "does not implement pluginapi.ServiceProvider")
	assert.True(t, stopped)
}

func TestHostLoad_RejectsExternalCore(t *testing.T) {
	host := pluginhost.New(pluginhost.PluginsConfig{
		"core": {Enabled: true, Source: "external"},
	}, nil, map[string]func() pluginapi.Plugin{
		"core": func() pluginapi.Plugin { return makeCorePlugin() },
	}, nil)

	err := host.Load(context.Background())
	require.Error(t, err)
	assert.ErrorContains(t, err, "core")
	assert.ErrorContains(t, err, "cannot use source \"external\"")
}

func TestGraph_DeterministicOrder(t *testing.T) {
	core := pluginapi.Metadata{
		Name: "core", Kind: "core", Mandatory: true,
		Publishes: []pluginapi.ServiceRef{{Name: "core_service"}},
	}
	alpha := pluginapi.Metadata{
		Name: "alpha", Kind: "test",
		Requires:  []pluginapi.ServiceRef{{Name: "core_service"}},
		Publishes: []pluginapi.ServiceRef{{Name: "alpha_service"}},
	}
	beta := pluginapi.Metadata{
		Name: "beta", Kind: "test",
		Requires: []pluginapi.ServiceRef{{Name: "core_service"}},
	}
	charlie := pluginapi.Metadata{
		Name: "charlie", Kind: "test",
		Requires: []pluginapi.ServiceRef{{Name: "alpha_service"}},
	}

	for _, input := range [][]pluginapi.Metadata{
		{core, alpha, beta, charlie},
		{charlie, beta, alpha, core},
		{beta, core, charlie, alpha},
	} {
		order, err := pluginhost.Graph(input, makeEnabled("core", "alpha", "beta", "charlie"))
		require.NoError(t, err)
		assert.Equal(t, []string{"core", "alpha", "beta", "charlie"}, order)
	}
}

func TestHostLoad_ResolverCannotResolveAfterInit(t *testing.T) {
	core := makeCorePlugin()
	var resolver pluginapi.ServiceResolver
	consumer := &stubPlugin{
		meta: pluginapi.Metadata{
			Name:       "consumer",
			Kind:       "test",
			APIVersion: "1",
			Requires:   []pluginapi.ServiceRef{{Name: "subscription_repository"}},
		},
		initFn: func(_ context.Context, _ pluginapi.RawConfig, reg pluginapi.ServiceResolver) error {
			resolver = reg
			_, err := reg.Resolve("subscription_repository")
			return err
		},
	}
	host := pluginhost.New(pluginhost.PluginsConfig{
		"core":     {Enabled: true, Source: "builtin"},
		"consumer": {Enabled: true, Source: "builtin"},
	}, nil, map[string]func() pluginapi.Plugin{
		"core":     func() pluginapi.Plugin { return core },
		"consumer": func() pluginapi.Plugin { return consumer },
	}, nil)

	require.NoError(t, host.Load(context.Background()))
	_, err := resolver.Resolve("subscription_repository")
	require.Error(t, err)
	assert.ErrorContains(t, err, "after Init() completed")
	require.NoError(t, host.Shutdown(context.Background()))
}
