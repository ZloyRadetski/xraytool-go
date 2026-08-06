// Package pluginhost implements the Plugin Host and Service Registry.
//
// The Plugin Host (Host) is the kernel component responsible for:
//  1. Reading plugins configuration.
//  2. Validating the dependency graph (Requires/Publishes) before starting any plugin.
//  3. Loading plugins in topological order (core plugin always first).
//  4. Running plugin lifecycle: Init → Start (goroutine) → Stop.
//  5. Health monitoring.
//
// The Service Registry (ServiceRegistry) is the runtime store of published
// services. During Load(), each plugin's Init() receives a ServiceResolver that
// is scoped to only the services that plugin declared in Metadata().Requires.
//
// IMPORTANT: This package is the strangler-fig replacement for the imperative
// composition in cmd/server.go. Phase 0.5 builds and tests it in isolation;
// the old cmd/server.go continues to work unchanged until Phase 1 is complete.
package pluginhost

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"

	"xraytool/internal/pluginapi"
)

// ─────────────────────────────────────────────────────────────────────────────
// PluginsConfig — the parsed plugins: section from xraytool.yml
// ─────────────────────────────────────────────────────────────────────────────

// PluginEntry is the per-plugin configuration block in plugins.yaml.
type PluginEntry struct {
	// Enabled controls whether the host calls Init() on this plugin.
	// If false, the plugin is completely ignored (no goroutines, no DB migrations).
	// Mandatory plugins (core) cannot be disabled; Host.Load() returns an error if tried.
	Enabled bool `yaml:"enabled"`

	// Source tells the host where to find the plugin.
	// "builtin" — compiled into the binary (the only mode currently supported).
	// Future: "external" will use go-plugin/gRPC (Phase 4).
	Source string `yaml:"source"`

	// Config is the raw configuration map passed to Plugin.Init() as RawConfig.
	Config pluginapi.RawConfig `yaml:"config"`
}

// PluginsConfig is the top-level plugins: map from xraytool.yml.
// Keys are plugin names (matching Metadata.Name).
type PluginsConfig map[string]PluginEntry

// ─────────────────────────────────────────────────────────────────────────────
// ServiceRegistry
// ─────────────────────────────────────────────────────────────────────────────

// ServiceRegistry stores services published by loaded plugins and resolves
// dependency requests from other plugins.
//
// Publish/Resolve lifecycle:
//  1. Before any Init(): Host calls Graph() to validate that all required
//     non-optional dependencies are satisfiable.
//  2. During Load(), for each plugin (in topological order):
//     a. Plugin.Init() is called; the plugin calls Resolve() via its ServiceResolver.
//     b. After successful Init(), Host calls Publish() for each name in Metadata.Publishes.
//
// Key design constraint (plan §2.5.1, rule 2):
// A plugin may only Resolve() a service it declared in Metadata().Requires.
// Resolving an undeclared service is an error — this keeps the dependency graph
// explicit, machine-readable and verifiable offline.
type ServiceRegistry struct {
	mu       sync.RWMutex
	services map[string]any
}

// newServiceRegistry creates an empty registry.
func newServiceRegistry() *ServiceRegistry {
	return &ServiceRegistry{services: make(map[string]any)}
}

// Publish registers a service under name. Returns an error if name is already taken.
func (r *ServiceRegistry) Publish(name string, svc any) error {
	if name == "" {
		return errors.New("service name must not be empty")
	}
	if isNilService(svc) {
		return fmt.Errorf("service %q must not be nil", name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.services[name]; exists {
		return fmt.Errorf("service %q is already published", name)
	}
	r.services[name] = svc
	return nil
}

func isNilService(svc any) bool {
	if svc == nil {
		return true
	}
	v := reflect.ValueOf(svc)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

// resolve is the internal resolver used by scopedResolver.
// It intentionally does not check the declared-Requires list; that check is in
// scopedResolver.Resolve so that we can unit-test the registry independently.
func (r *ServiceRegistry) resolve(name string) (any, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	svc, ok := r.services[name]
	if !ok {
		return nil, fmt.Errorf("service %q is not published by any loaded plugin", name)
	}
	return svc, nil
}

// Graph performs a topological sort of the plugins using Metadata.Requires /
// Metadata.Publishes and returns the ordered plugin names. It validates that
// every non-optional required service has at least one publisher in the enabled
// set. Returns an error with a human-readable description of the first
// unsatisfied dependency found.
//
// The core plugin (Mandatory:true) is always placed first regardless of
// declaration order.
func Graph(plugins []pluginapi.Metadata, enabled map[string]bool) ([]string, error) {
	// Build the set of services that will be published once all enabled plugins load.
	published := make(map[string]string) // serviceName → pluginName
	seenPlugins := make(map[string]struct{}, len(plugins))
	mandatoryCount := 0
	for _, m := range plugins {
		if !enabled[m.Name] {
			continue
		}
		if m.Name == "" {
			return nil, errors.New("enabled plugin has an empty metadata name")
		}
		if _, exists := seenPlugins[m.Name]; exists {
			return nil, fmt.Errorf("plugin metadata contains duplicate name %q", m.Name)
		}
		seenPlugins[m.Name] = struct{}{}
		if m.Mandatory {
			mandatoryCount++
		}
		for _, pub := range m.Publishes {
			if pub.Name == "" {
				return nil, fmt.Errorf("plugin %q declares an empty published service name", m.Name)
			}
			if owner, exists := published[pub.Name]; exists {
				return nil, fmt.Errorf(
					"service %q is published by both plugins %q and %q",
					pub.Name, owner, m.Name,
				)
			}
			published[pub.Name] = m.Name
		}
	}
	if mandatoryCount > 1 {
		return nil, errors.New("plugin dependency graph contains more than one mandatory plugin")
	}

	// Check that every non-optional required service is satisfiable.
	for _, m := range plugins {
		if !enabled[m.Name] {
			continue
		}
		for _, req := range m.Requires {
			if req.Optional {
				continue
			}
			if _, ok := published[req.Name]; !ok {
				return nil, fmt.Errorf(
					"plugin %q requires service %q which is not published by any enabled plugin "+
						"(either the publishing plugin is disabled or missing)",
					m.Name, req.Name,
				)
			}
		}
	}

	// Topological sort using Kahn's algorithm.
	// Build adjacency: for each plugin, edges to plugins that require its published services.
	nameToMeta := make(map[string]pluginapi.Metadata, len(plugins))
	inDegree := make(map[string]int, len(plugins))
	deps := make(map[string][]string) // plugin → slice of plugins it depends on (must come before)

	for _, m := range plugins {
		if !enabled[m.Name] {
			continue
		}
		nameToMeta[m.Name] = m
		if _, ok := inDegree[m.Name]; !ok {
			inDegree[m.Name] = 0
		}
	}

	for _, m := range plugins {
		if !enabled[m.Name] {
			continue
		}
		for _, req := range m.Requires {
			publisher, ok := published[req.Name]
			if !ok {
				continue // optional and unsatisfied — skip
			}
			if publisher == m.Name {
				continue // self-publish is allowed but not a dep edge
			}
			// m depends on publisher → publisher must come first
			deps[m.Name] = append(deps[m.Name], publisher)
			inDegree[publisher] = inDegree[publisher] // ensure entry exists
		}
	}

	// Compute in-degree per node based on who depends on whom.
	// (in-degree = number of plugins that must be loaded BEFORE this one)
	inDegreeCalc := make(map[string]int, len(nameToMeta))
	reverseEdges := make(map[string][]string) // publisher → consumers that depend on it
	for consumer, publishers := range deps {
		for _, pub := range publishers {
			reverseEdges[pub] = append(reverseEdges[pub], consumer)
			inDegreeCalc[consumer]++
		}
	}

	// Kahn's BFS
	queue := make([]string, 0, len(nameToMeta))
	names := make([]string, 0, len(nameToMeta))
	for name := range nameToMeta {
		names = append(names, name)
	}
	sort.Strings(names)
	// Always seed with mandatory plugins first (core).
	for _, name := range names {
		m := nameToMeta[name]
		if m.Mandatory {
			queue = append(queue, name)
		}
	}
	for _, name := range names {
		if inDegreeCalc[name] == 0 && !nameToMeta[name].Mandatory {
			queue = append(queue, name)
		}
	}

	order := make([]string, 0, len(nameToMeta))
	visited := make(map[string]bool, len(nameToMeta))

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if visited[cur] {
			continue
		}
		visited[cur] = true
		order = append(order, cur)

		consumers := reverseEdges[cur]
		sort.Strings(consumers)
		for _, consumer := range consumers {
			inDegreeCalc[consumer]--
			if inDegreeCalc[consumer] == 0 {
				queue = append(queue, consumer)
			}
		}
	}

	if len(order) != len(nameToMeta) {
		return nil, errors.New("plugin dependency graph contains a cycle — cannot determine load order")
	}

	return order, nil
}

func apiVersionSupported(version string) bool {
	if version == "" {
		return false
	}
	major, _, _ := strings.Cut(version, ".")
	wanted, err := strconv.Atoi(major)
	if err != nil {
		return false
	}
	supported, err := strconv.Atoi(pluginapi.CurrentAPIVersion)
	return err == nil && wanted <= supported
}

func publishDeclaredServices(
	registry *ServiceRegistry,
	pluginName string,
	meta pluginapi.Metadata,
	plugin pluginapi.Plugin,
) error {
	provider, ok := plugin.(pluginapi.ServiceProvider)
	if !ok {
		// The in-progress Phase 1 wrappers still use two-phase initialisation.
		// Keep them loadable while they are migrated to ServiceProvider; they
		// cannot, however, satisfy a runtime Resolve until that migration lands.
		return nil
	}

	services := provider.PublishedServices()
	declared := make(map[string]struct{}, len(meta.Publishes))
	for _, publication := range meta.Publishes {
		declared[publication.Name] = struct{}{}
		service, ok := services[publication.Name]
		if !ok {
			return fmt.Errorf("plugin %q did not provide declared service %q", pluginName, publication.Name)
		}
		if err := registry.Publish(publication.Name, service); err != nil {
			return fmt.Errorf("plugin %q publish service %q: %w", pluginName, publication.Name, err)
		}
	}
	for name := range services {
		if _, ok := declared[name]; !ok {
			return fmt.Errorf("plugin %q provided undeclared service %q", pluginName, name)
		}
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// scopedResolver — ServiceResolver implementation handed to each plugin's Init()
// ─────────────────────────────────────────────────────────────────────────────

type scopedResolver struct {
	pluginName string
	declared   map[string]bool // names from Metadata.Requires
	registry   *ServiceRegistry
	db         pluginapi.PluginDBHandle
	log        pluginapi.Logger
	emitFn     func(eventType string, data map[string]any, userMeta map[string]any)
}

func (s *scopedResolver) Resolve(name string) (any, error) {
	if !s.declared[name] {
		return nil, fmt.Errorf(
			"plugin %q tried to resolve %q which is not declared in its Metadata().Requires; "+
				"add it to Requires to make the dependency explicit and validatable",
			s.pluginName, name,
		)
	}
	return s.registry.resolve(name)
}

func (s *scopedResolver) Logger() pluginapi.Logger { return s.log }

func (s *scopedResolver) EmitEvent(eventType string, data map[string]any, userMeta map[string]any) {
	if s.emitFn != nil {
		s.emitFn(eventType, data, userMeta)
	}
}

func (s *scopedResolver) DB() pluginapi.PluginDBHandle { return s.db }

// ─────────────────────────────────────────────────────────────────────────────
// slogLogger — thin pluginapi.Logger adapter over log/slog
// ─────────────────────────────────────────────────────────────────────────────

type slogLogger struct {
	log *slog.Logger
}

func (l *slogLogger) Debug(msg string, args ...any) { l.log.Debug(msg, args...) }
func (l *slogLogger) Info(msg string, args ...any)  { l.log.Info(msg, args...) }
func (l *slogLogger) Warn(msg string, args ...any)  { l.log.Warn(msg, args...) }
func (l *slogLogger) Error(msg string, args ...any) { l.log.Error(msg, args...) }

// ─────────────────────────────────────────────────────────────────────────────
// Host
// ─────────────────────────────────────────────────────────────────────────────

// loadedPlugin combines a plugin instance with its metadata for runtime use.
type loadedPlugin struct {
	meta   pluginapi.Metadata
	plugin pluginapi.Plugin
}

// Host is the Plugin Host. It owns the Service Registry, loads plugins in
// dependency order, runs their goroutines, and shuts them down gracefully.
//
// Phase 0.5 supports ONLY internal (compiled-in, source:"builtin") plugins.
// External (go-plugin/gRPC) support is added in Phase 4.
type Host struct {
	cfg      PluginsConfig
	registry *ServiceRegistry
	log      *slog.Logger

	// staticRegistry holds all compiled-in plugin factories, populated by
	// internal/pluginhost/registry_builtin.go (generated / hand-maintained).
	staticRegistry map[string]func() pluginapi.Plugin

	// loaded is the ordered slice of successfully initialised plugins.
	loaded []loadedPlugin

	// emitFn is set by the kernel to fan out events to all EventSink plugins.
	// It is used by scopedResolver.EmitEvent.
	emitFn func(eventType string, data map[string]any, userMeta map[string]any)

	// runCancel cancels the context passed to every Plugin.Start. Stop alone is
	// not sufficient because Start is allowed to block until its context ends.
	runCancel context.CancelFunc

	mu sync.RWMutex
}

// New creates a Host with the given configuration and optional event emitter.
// staticPlugins maps plugin names to factory functions that return a fresh
// pluginapi.Plugin — it is populated by registry_builtin.go.
func New(
	cfg PluginsConfig,
	log *slog.Logger,
	staticPlugins map[string]func() pluginapi.Plugin,
	emitFn func(string, map[string]any, map[string]any),
) *Host {
	if log == nil {
		log = slog.Default()
	}
	return &Host{
		cfg:            cfg,
		registry:       newServiceRegistry(),
		log:            log,
		staticRegistry: staticPlugins,
		emitFn:         emitFn,
	}
}

// Load validates the dependency graph, then initialises and starts all enabled
// plugins in topological order. It returns an error (without starting any plugin)
// if:
//   - The core plugin is disabled or missing.
//   - A non-optional required service has no publisher among the enabled plugins.
//   - The dependency graph contains a cycle.
//   - Any plugin's Init() returns an error.
func (h *Host) Load(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if len(h.loaded) > 0 {
		return errors.New("Host.Load() called more than once")
	}

	// Collect metadata for all enabled plugins to build the dependency graph.
	type candidate struct {
		name    string
		entry   PluginEntry
		factory func() pluginapi.Plugin
	}

	candidates := make([]candidate, 0, len(h.cfg))
	for name, entry := range h.cfg {
		if !entry.Enabled {
			h.log.Info("[pluginhost] plugin disabled — skipping", "plugin", name)
			continue
		}
		factory, ok := h.staticRegistry[name]
		if !ok {
			return fmt.Errorf("plugin %q is enabled but not found in the builtin registry; "+
				"check your build tags or add it to registry_builtin.go", name)
		}
		candidates = append(candidates, candidate{name: name, entry: entry, factory: factory})
	}

	// Instantiate each candidate to read its Metadata.
	instances := make(map[string]pluginapi.Plugin, len(candidates))
	metas := make([]pluginapi.Metadata, 0, len(candidates))
	enabled := make(map[string]bool, len(candidates))

	for _, c := range candidates {
		p := c.factory()
		m := p.Metadata()
		if m.Name != c.name {
			return fmt.Errorf("plugin factory registered as %q returned Metadata.Name=%q — they must match",
				c.name, m.Name)
		}
		instances[m.Name] = p
		metas = append(metas, m)
		enabled[m.Name] = true
		if !apiVersionSupported(m.APIVersion) {
			return fmt.Errorf(
				"plugin %q requires unsupported plugin API version %q (host supports %q)",
				m.Name, m.APIVersion, pluginapi.CurrentAPIVersion,
			)
		}

		// Enforce mandatory check early.
		if m.Mandatory && !c.entry.Enabled {
			return fmt.Errorf("plugin %q is mandatory (Metadata.Mandatory=true) and cannot be disabled", m.Name)
		}
	}

	// Check that the core (mandatory) plugin is present.
	foundMandatory := false
	for _, m := range metas {
		if m.Mandatory {
			foundMandatory = true
			break
		}
	}
	if !foundMandatory {
		return errors.New("no mandatory plugin found; the core plugin must be present and enabled — " +
			"the kernel cannot serve any request without it")
	}

	// Validate graph and compute load order.
	order, err := Graph(metas, enabled)
	if err != nil {
		return fmt.Errorf("plugin dependency graph error: %w", err)
	}

	h.log.Info("[pluginhost] load order determined", "order", order)

	// Init plugins in order.
	for _, name := range order {
		p := instances[name]
		m := p.Metadata()
		entry := h.cfg[name]

		// Build the set of declared Requires for the scoped resolver.
		declared := make(map[string]bool, len(m.Requires))
		for _, req := range m.Requires {
			declared[req.Name] = true
		}

		resolver := &scopedResolver{
			pluginName: name,
			declared:   declared,
			registry:   h.registry,
			db:         nil, // Phase 1: real PluginDBHandle wired here
			log:        &slogLogger{log: h.log.With("plugin", name)},
			emitFn:     h.emitFn,
		}

		h.log.Info("[pluginhost] initialising plugin", "plugin", name, "version", m.Version)
		if err := p.Init(ctx, entry.Config, resolver); err != nil {
			return fmt.Errorf("plugin %q Init() failed: %w", name, err)
		}

		if err := publishDeclaredServices(h.registry, name, m, p); err != nil {
			return err
		}

		h.loaded = append(h.loaded, loadedPlugin{meta: m, plugin: p})
		h.log.Info("[pluginhost] plugin initialised", "plugin", name)
	}

	// Start all plugins (each in its own goroutine). The Host owns a child
	// context so Shutdown can stop plugins even if the caller's Load context is
	// still alive (the normal case for a long-running server).
	runCtx, runCancel := context.WithCancel(ctx)
	h.runCancel = runCancel
	for _, lp := range h.loaded {
		go func(lp loadedPlugin) {
			h.log.Info("[pluginhost] starting plugin", "plugin", lp.meta.Name)
			if err := lp.plugin.Start(runCtx); err != nil && !errors.Is(err, context.Canceled) {
				h.log.Error("[pluginhost] plugin Start() returned an error",
					"plugin", lp.meta.Name, "error", err)
			}
			h.log.Info("[pluginhost] plugin stopped", "plugin", lp.meta.Name)
		}(lp)
	}

	h.log.Info("[pluginhost] all plugins loaded and started", "count", len(h.loaded))
	return nil
}

// PublishService is a helper that allows a plugin's Init() implementation to
// publish a service to the registry without holding a reference to the registry
// directly. It is called by plugins as:
//
//	reg.(*pluginhost.ScopedPublisher).Publish("my_service", myImpl)
//
// In practice, plugins receive a thin wrapper that exposes Publish alongside Resolve.
// This method on Host is used during testing.
func (h *Host) PublishService(name string, svc any) error {
	return h.registry.Publish(name, svc)
}

// ResolveService retrieves a published service by name. For use in tests and
// internal kernel code that needs to access a service outside a plugin's Init().
func (h *Host) ResolveService(name string) (any, error) {
	return h.registry.resolve(name)
}

// Shutdown stops all loaded plugins in reverse load order (last-loaded first,
// core plugin last). Each plugin is given the context's deadline to finish.
func (h *Host) Shutdown(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.runCancel != nil {
		h.runCancel()
		h.runCancel = nil
	}

	var errs []error
	for i := len(h.loaded) - 1; i >= 0; i-- {
		lp := h.loaded[i]
		h.log.Info("[pluginhost] stopping plugin", "plugin", lp.meta.Name)
		if err := lp.plugin.Stop(ctx); err != nil {
			h.log.Error("[pluginhost] plugin Stop() error", "plugin", lp.meta.Name, "error", err)
			errs = append(errs, fmt.Errorf("plugin %q Stop: %w", lp.meta.Name, err))
		}
	}
	h.loaded = nil
	return errors.Join(errs...)
}

// HealthCheck returns a map of plugin name → health error (nil means healthy).
// Called by the health monitor and `xraytool plugin list`.
func (h *Host) HealthCheck(ctx context.Context) map[string]error {
	h.mu.RLock()
	defer h.mu.RUnlock()

	result := make(map[string]error, len(h.loaded))
	for _, lp := range h.loaded {
		result[lp.meta.Name] = lp.plugin.Health(ctx)
	}
	return result
}

// Loaded returns a snapshot of the currently loaded plugins' metadata.
func (h *Host) Loaded() []pluginapi.Metadata {
	h.mu.RLock()
	defer h.mu.RUnlock()
	metas := make([]pluginapi.Metadata, len(h.loaded))
	for i, lp := range h.loaded {
		metas[i] = lp.meta
	}
	return metas
}

// typed accessors — convenience methods so that kernel code does not have to
// type-assert through ResolveService every time.

// Core returns the mandatory CoreProvider. Panics if core was not loaded or
// does not implement the declared core contract; either condition is a plugin
// wiring error rather than a runtime condition a caller can recover from.
func (h *Host) Core() pluginapi.CoreProvider {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, lp := range h.loaded {
		if !lp.meta.Mandatory {
			continue
		}
		core, ok := lp.plugin.(pluginapi.CoreProvider)
		if !ok {
			panic("pluginhost: mandatory plugin does not implement pluginapi.CoreProvider")
		}
		return core
	}
	panic("pluginhost: Core() called but the mandatory core plugin is not loaded")
}

// Antifraud returns the AntifraudProvider and a bool indicating whether one is loaded.
func (h *Host) Antifraud() (pluginapi.AntifraudProvider, bool) {
	svc, err := h.registry.resolve("antifraud_provider")
	if err != nil {
		return nil, false
	}
	af, ok := svc.(pluginapi.AntifraudProvider)
	return af, ok
}

// PaymentProviders returns all loaded payment providers keyed by MethodID.
func (h *Host) PaymentProviders() map[string]pluginapi.PaymentProvider {
	h.mu.RLock()
	defer h.mu.RUnlock()

	result := make(map[string]pluginapi.PaymentProvider)
	for _, lp := range h.loaded {
		if pp, ok := lp.plugin.(pluginapi.PaymentProvider); ok {
			result[pp.MethodID()] = pp
		}
	}
	return result
}

// EventSinks returns all loaded EventSink plugins.
func (h *Host) EventSinks() []pluginapi.EventSink {
	h.mu.RLock()
	defer h.mu.RUnlock()

	var sinks []pluginapi.EventSink
	for _, lp := range h.loaded {
		if es, ok := lp.plugin.(pluginapi.EventSink); ok {
			sinks = append(sinks, es)
		}
	}
	return sinks
}

// EngineProviders returns all loaded EngineProvider plugins.
func (h *Host) EngineProviders() []pluginapi.EngineProvider {
	h.mu.RLock()
	defer h.mu.RUnlock()

	var engines []pluginapi.EngineProvider
	for _, lp := range h.loaded {
		if ep, ok := lp.plugin.(pluginapi.EngineProvider); ok {
			engines = append(engines, ep)
		}
	}
	return engines
}

// PluginByName returns the loaded plugin instance for the given name, or nil if
// the plugin was not loaded. The caller must type-assert the result to the
// concrete plugin type.
//
// Example:
//
//	raw := host.PluginByName("core")
//	if raw == nil { /* handle */ }
//	core := raw.(*corePlugin.Plugin)
func (h *Host) PluginByName(name string) pluginapi.Plugin {
	h.mu.RLock()
	defer h.mu.RUnlock()

	for _, lp := range h.loaded {
		if lp.meta.Name == name {
			return lp.plugin
		}
	}
	return nil
}
