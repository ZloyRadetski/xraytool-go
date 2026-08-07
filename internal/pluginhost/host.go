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
	"time"

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

	// Exec and Args are required for source:"external". Exec is passed to
	// exec.Command without a shell.
	Exec string   `yaml:"exec"`
	Args []string `yaml:"args"`
	// LogPath optionally overrides the persistent external process log file.
	// Empty uses ExternalLogPath(name).
	LogPath string `yaml:"log_path"`

	// RestartPolicy limits restarts of an external plugin process. A zero
	// MaxRestarts disables restarts for this entry.
	RestartPolicy RestartPolicy `yaml:"restart_policy"`

	// Config is the raw configuration map passed to Plugin.Init() as RawConfig.
	Config pluginapi.RawConfig `yaml:"config"`
}

// RestartPolicy controls recovery of an external plugin process.
type RestartPolicy struct {
	MaxRestarts int           `yaml:"max_restarts"`
	Backoff     time.Duration `yaml:"backoff"`
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
	if strings.TrimSpace(name) == "" {
		return errors.New("service name must not be empty")
	}
	if isNilService(svc) {
		return fmt.Errorf("service %q must not be nil", name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.services == nil {
		r.services = make(map[string]any)
	}
	if _, exists := r.services[name]; exists {
		return fmt.Errorf("service %q is already published", name)
	}
	r.services[name] = svc
	return nil
}

// publishBatch atomically makes a fully validated group of services visible.
// It is deliberately internal: publication is owned by Host and must happen
// only after a plugin's Init completed successfully.
func (r *ServiceRegistry) publishBatch(services map[string]any) error {
	if len(services) == 0 {
		return nil
	}

	names := make([]string, 0, len(services))
	for name, service := range services {
		if strings.TrimSpace(name) == "" {
			return errors.New("service name must not be empty")
		}
		if isNilService(service) {
			return fmt.Errorf("service %q must not be nil", name)
		}
		names = append(names, name)
	}
	sort.Strings(names)

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.services == nil {
		r.services = make(map[string]any)
	}
	for _, name := range names {
		if _, exists := r.services[name]; exists {
			return fmt.Errorf("service %q is already published", name)
		}
	}
	for _, name := range names {
		r.services[name] = services[name]
	}
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
	// Config maps are inherently unordered. Sort a copy up front so both the
	// resulting order and validation errors remain stable across process runs.
	metas := make([]pluginapi.Metadata, 0, len(plugins))
	for _, metadata := range plugins {
		if enabled[metadata.Name] {
			metas = append(metas, metadata)
		}
	}
	sort.Slice(metas, func(i, j int) bool {
		return metas[i].Name < metas[j].Name
	})

	published := make(map[string]string, len(metas)) // service name -> plugin name
	byName := make(map[string]pluginapi.Metadata, len(metas))
	mandatoryName := ""

	for _, metadata := range metas {
		if strings.TrimSpace(metadata.Name) == "" {
			return nil, errors.New("enabled plugin has an empty metadata name")
		}
		if _, exists := byName[metadata.Name]; exists {
			return nil, fmt.Errorf("plugin metadata contains duplicate name %q", metadata.Name)
		}
		byName[metadata.Name] = metadata

		if metadata.Mandatory {
			if mandatoryName != "" {
				return nil, errors.New("plugin dependency graph contains more than one mandatory plugin")
			}
			if len(metadata.Requires) > 0 {
				return nil, fmt.Errorf("mandatory plugin %q must not require services", metadata.Name)
			}
			mandatoryName = metadata.Name
		}

		declaredPublications := make(map[string]struct{}, len(metadata.Publishes))
		for _, publication := range metadata.Publishes {
			if strings.TrimSpace(publication.Name) == "" {
				return nil, fmt.Errorf("plugin %q declares an empty published service name", metadata.Name)
			}
			if _, exists := declaredPublications[publication.Name]; exists {
				return nil, fmt.Errorf("plugin %q declares published service %q more than once", metadata.Name, publication.Name)
			}
			declaredPublications[publication.Name] = struct{}{}
			if owner, exists := published[publication.Name]; exists {
				return nil, fmt.Errorf(
					"service %q is published by both plugins %q and %q",
					publication.Name, owner, metadata.Name,
				)
			}
			published[publication.Name] = metadata.Name
		}
	}

	// Kahn's algorithm. Edges point from a publisher to a consumer so the
	// consumer's in-degree is exactly the number of services it must wait for.
	inDegree := make(map[string]int, len(metas))
	reverseEdges := make(map[string][]string, len(metas))
	for _, metadata := range metas {
		inDegree[metadata.Name] = 0
		declaredRequirements := make(map[string]struct{}, len(metadata.Requires))
		for _, requirement := range metadata.Requires {
			if strings.TrimSpace(requirement.Name) == "" {
				return nil, fmt.Errorf("plugin %q declares an empty required service name", metadata.Name)
			}
			if _, exists := declaredRequirements[requirement.Name]; exists {
				return nil, fmt.Errorf("plugin %q declares required service %q more than once", metadata.Name, requirement.Name)
			}
			declaredRequirements[requirement.Name] = struct{}{}

			publisher, exists := published[requirement.Name]
			if !exists {
				if requirement.Optional {
					continue
				}
				return nil, fmt.Errorf(
					"plugin %q requires service %q which is not published by any enabled plugin "+
						"(either the publishing plugin is disabled or missing)",
					metadata.Name, requirement.Name,
				)
			}
			if publisher == metadata.Name {
				return nil, fmt.Errorf("plugin %q cannot require its own published service %q", metadata.Name, requirement.Name)
			}
			reverseEdges[publisher] = append(reverseEdges[publisher], metadata.Name)
			inDegree[metadata.Name]++
		}
	}

	for publisher := range reverseEdges {
		sort.Strings(reverseEdges[publisher])
	}

	ready := make([]string, 0, len(metas))
	for _, metadata := range metas {
		if inDegree[metadata.Name] == 0 {
			ready = append(ready, metadata.Name)
		}
	}
	sort.Strings(ready)
	if mandatoryName != "" {
		// A mandatory plugin has no dependencies (validated above), so it is
		// always ready and must be selected first regardless of its name.
		for i, name := range ready {
			if name == mandatoryName {
				ready = append([]string{mandatoryName}, append(ready[:i], ready[i+1:]...)...)
				break
			}
		}
	}

	order := make([]string, 0, len(metas))
	for len(ready) > 0 {
		current := ready[0]
		ready = ready[1:]
		order = append(order, current)

		for _, consumer := range reverseEdges[current] {
			inDegree[consumer]--
			if inDegree[consumer] == 0 {
				ready = append(ready, consumer)
			}
		}
		// Keep a total order among all currently runnable plugins, not only the
		// initial queue. That makes the output deterministic for diamonds too.
		sort.Strings(ready)
	}

	if len(order) != len(metas) {
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
	if err != nil || wanted < 1 {
		return false
	}
	supported, err := strconv.Atoi(pluginapi.CurrentAPIVersion)
	return err == nil && supported >= 1 && wanted <= supported
}

func publishDeclaredServices(
	registry *ServiceRegistry,
	pluginName string,
	meta pluginapi.Metadata,
	plugin pluginapi.Plugin,
) error {
	provider, ok := plugin.(pluginapi.ServiceProvider)
	if !ok {
		if len(meta.Publishes) == 0 {
			return nil
		}
		return fmt.Errorf(
			"plugin %q declares published services but does not implement pluginapi.ServiceProvider",
			pluginName,
		)
	}

	services := provider.PublishedServices()
	declared := make(map[string]any, len(meta.Publishes))
	for _, publication := range meta.Publishes {
		declared[publication.Name] = nil
		service, ok := services[publication.Name]
		if !ok {
			return fmt.Errorf("plugin %q did not provide declared service %q", pluginName, publication.Name)
		}
		if isNilService(service) {
			return fmt.Errorf("plugin %q provided nil for declared service %q", pluginName, publication.Name)
		}
		declared[publication.Name] = service
	}

	providedNames := make([]string, 0, len(services))
	for name := range services {
		providedNames = append(providedNames, name)
	}
	sort.Strings(providedNames)
	for _, name := range providedNames {
		if _, ok := declared[name]; !ok {
			return fmt.Errorf("plugin %q provided undeclared service %q", pluginName, name)
		}
	}
	if err := registry.publishBatch(declared); err != nil {
		return fmt.Errorf("plugin %q publish declared services: %w", pluginName, err)
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

	mu     sync.RWMutex
	active bool
}

func (s *scopedResolver) Resolve(name string) (any, error) {
	s.mu.RLock()
	active := s.active
	s.mu.RUnlock()
	if !active {
		return nil, fmt.Errorf("plugin %q tried to resolve %q after Init() completed", s.pluginName, name)
	}
	if !s.declared[name] {
		return nil, fmt.Errorf(
			"plugin %q tried to resolve %q which is not declared in its Metadata().Requires; "+
				"add it to Requires to make the dependency explicit and validatable",
			s.pluginName, name,
		)
	}
	return s.registry.resolve(name)
}

// closeResolveWindow makes the resolver unusable for dependency resolution once
// Init returns. Keeping a resolver reference must not turn it into a runtime
// service locator and thereby bypass the graph validated by Host.Load.
func (s *scopedResolver) closeResolveWindow() {
	s.mu.Lock()
	s.active = false
	s.mu.Unlock()
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

type hostState uint8

const (
	hostNew hostState = iota
	hostLoading
	hostRunning
	hostStopping
	hostStopped
)

// Host is the Plugin Host. It owns the Service Registry, loads plugins in
// dependency order, runs their goroutines, and shuts them down gracefully.
type Host struct {
	cfg      PluginsConfig
	registry *ServiceRegistry
	log      *slog.Logger

	// staticRegistry holds all compiled-in plugin factories, populated by
	// internal/pluginhost/registry_builtin.go (generated / hand-maintained).
	staticRegistry map[string]func() pluginapi.Plugin

	// pluginDBFactory creates a scoped handle for an enabled plugin. It is
	// supplied by the composition root because the kernel, not plugins, owns
	// the connection pool.
	pluginDBFactory PluginDBFactory

	// loaded is the ordered slice of successfully initialised plugins.
	loaded []loadedPlugin

	// emitFn is set by the kernel to fan out events to all EventSink plugins.
	// It is used by scopedResolver.EmitEvent.
	emitFn func(eventType string, data map[string]any, userMeta map[string]any)

	// runCancel cancels the context passed to every Plugin.Start. Stop alone is
	// not sufficient because Start is allowed to block until its context ends.
	runCancel context.CancelFunc

	// lifecycleMu serialises Load and Shutdown without holding mu while calling
	// untrusted plugin code. Plugins can therefore use regular Host accessors
	// from their lifecycle methods without deadlocking the host.
	lifecycleMu sync.Mutex

	// starts tracks every Start goroutine so Shutdown can wait until they have
	// actually returned after cancellation.
	starts sync.WaitGroup

	// banCache is a kernel-owned read model populated by AntifraudProvider
	// push updates. Subscription requests must never issue RPCs to an external
	// antifraud plugin on their hot path.
	banCache *LocalBanCache

	mu    sync.RWMutex
	state hostState
}

// New creates a Host with the given configuration and optional event emitter.
// staticPlugins maps plugin names to factory functions that return a fresh
// pluginapi.Plugin — it is populated by registry_builtin.go.
func New(
	cfg PluginsConfig,
	log *slog.Logger,
	staticPlugins map[string]func() pluginapi.Plugin,
	emitFn func(string, map[string]any, map[string]any),
	options ...HostOption,
) *Host {
	if log == nil {
		log = slog.Default()
	}
	host := &Host{
		cfg:            clonePluginsConfig(cfg),
		registry:       newServiceRegistry(),
		log:            log,
		staticRegistry: cloneStaticRegistry(staticPlugins),
		emitFn:         emitFn,
		banCache:       NewLocalBanCache(),
	}
	for _, option := range options {
		if option != nil {
			option(host)
		}
	}
	return host
}

func clonePluginsConfig(cfg PluginsConfig) PluginsConfig {
	if cfg == nil {
		return nil
	}
	copy := make(PluginsConfig, len(cfg))
	for name, entry := range cfg {
		entry.Config = cloneRawConfig(entry.Config)
		entry.Args = append([]string(nil), entry.Args...)
		copy[name] = entry
	}
	return copy
}

func cloneRawConfig(cfg pluginapi.RawConfig) pluginapi.RawConfig {
	if cfg == nil {
		return nil
	}
	copy := make(pluginapi.RawConfig, len(cfg))
	for key, value := range cfg {
		copy[key] = value
	}
	return copy
}

func cloneStaticRegistry(registry map[string]func() pluginapi.Plugin) map[string]func() pluginapi.Plugin {
	if registry == nil {
		return nil
	}
	copy := make(map[string]func() pluginapi.Plugin, len(registry))
	for name, factory := range registry {
		copy[name] = factory
	}
	return copy
}

func cloneMetadata(metadata pluginapi.Metadata) pluginapi.Metadata {
	metadata.Publishes = append([]pluginapi.ServiceRef(nil), metadata.Publishes...)
	metadata.Requires = append([]pluginapi.ServiceRef(nil), metadata.Requires...)
	return metadata
}

// Load validates the dependency graph, then initialises and starts all enabled
// plugins in topological order. It returns an error (without starting any plugin)
// if:
//   - The core plugin is disabled or missing.
//   - A non-optional required service has no publisher among the enabled plugins.
//   - The dependency graph contains a cycle.
//   - Any plugin's Init() returns an error.
func (h *Host) Load(ctx context.Context) (err error) {
	if ctx == nil {
		return errors.New("pluginhost: Load context must not be nil")
	}

	h.lifecycleMu.Lock()
	defer h.lifecycleMu.Unlock()

	h.mu.Lock()
	locked := true
	defer func() {
		if locked {
			h.mu.Unlock()
		}
	}()

	switch h.state {
	case hostNew:
		h.state = hostLoading
	case hostRunning:
		return errors.New("Host.Load() called more than once")
	case hostStopped:
		return errors.New("Host.Load() cannot be called after Shutdown()")
	default:
		return errors.New("Host.Load() is already in progress")
	}

	// The registry used during Init is private until every plugin has passed
	// Init and declared all of its services. This prevents external callers from
	// observing a partially initialised graph.
	stagingRegistry := newServiceRegistry()
	initialised := make([]loadedPlugin, 0, len(h.cfg))
	preflighted := make([]preflightPlugin, 0, len(h.cfg))
	defer func() {
		if err == nil {
			return
		}
		// Never run arbitrary plugin Stop methods while holding the Host lock:
		// a plugin may legitimately call a Host accessor during cleanup.
		h.mu.Unlock()
		locked = false
		cleanupErr := h.rollbackInitialised(ctx, initialised)
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), externalPluginRPCTimeout)
		for i := len(preflighted) - 1; i >= 0; i-- {
			preflighted[i].AbortPreflight(cleanupCtx)
		}
		cleanupCancel()
		h.mu.Lock()
		locked = true
		h.loaded = nil
		h.registry = newServiceRegistry()
		h.runCancel = nil
		h.state = hostNew
		if cleanupErr != nil {
			err = errors.Join(err, cleanupErr)
		}
	}()

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("plugin host load cancelled before initialisation: %w", err)
	}
	if entry, configured := h.cfg["core"]; configured && !entry.Enabled {
		return errors.New("plugin \"core\" is mandatory and cannot be disabled")
	}

	// Collect metadata for all enabled plugins to build the dependency graph.
	type candidate struct {
		name    string
		entry   PluginEntry
		factory func() pluginapi.Plugin
		plugin  pluginapi.Plugin
	}

	candidates := make([]candidate, 0, len(h.cfg))
	names := make([]string, 0, len(h.cfg))
	for name := range h.cfg {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		entry := h.cfg[name]
		if !entry.Enabled {
			h.log.Info("[pluginhost] plugin disabled — skipping", "plugin", name)
			continue
		}
		if strings.TrimSpace(name) == "" {
			return errors.New("enabled plugin configuration has an empty name")
		}
		switch entry.Source {
		case "builtin":
			factory, ok := h.staticRegistry[name]
			if !ok {
				return fmt.Errorf("plugin %q is enabled but not found in the builtin registry; "+
					"check your build tags or add it to registry_builtin.go", name)
			}
			if factory == nil {
				return fmt.Errorf("plugin %q has a nil builtin factory", name)
			}
			candidates = append(candidates, candidate{name: name, entry: entry, factory: factory})
		case "external":
			if name == "core" {
				return errors.New("plugin \"core\" is mandatory and cannot use source \"external\"")
			}
			candidates = append(candidates, candidate{
				name: name, entry: entry, plugin: newExternalPlugin(name, entry, h.log),
			})
		default:
			return fmt.Errorf("plugin %q has unsupported source %q; supported sources are %q and %q",
				name, entry.Source, "builtin", "external")
		}
	}

	// Instantiate each candidate to read its Metadata.
	instances := make(map[string]pluginapi.Plugin, len(candidates))
	metadataByName := make(map[string]pluginapi.Metadata, len(candidates))
	metas := make([]pluginapi.Metadata, 0, len(candidates))
	enabled := make(map[string]bool, len(candidates))

	for _, c := range candidates {
		p := c.plugin
		if p == nil {
			p = c.factory()
		}
		if isNilService(p) {
			return fmt.Errorf("plugin factory registered as %q returned nil", c.name)
		}
		if external, ok := p.(preflightPlugin); ok {
			h.mu.Unlock()
			locked = false
			preflightErr := external.Preflight(ctx)
			h.mu.Lock()
			locked = true
			if preflightErr != nil {
				return fmt.Errorf("external plugin %q preflight failed: %w", c.name, preflightErr)
			}
			preflighted = append(preflighted, external)
		}
		m := cloneMetadata(p.Metadata())
		if m.Name != c.name {
			return fmt.Errorf("plugin factory registered as %q returned Metadata.Name=%q — they must match",
				c.name, m.Name)
		}
		if strings.TrimSpace(m.Kind) == "" {
			return fmt.Errorf("plugin %q has an empty Metadata.Kind", m.Name)
		}
		if m.Mandatory && m.Name != "core" {
			return fmt.Errorf("plugin %q is mandatory, but only plugin \"core\" may be mandatory", m.Name)
		}
		if m.Name == "core" && !m.Mandatory {
			return errors.New("plugin \"core\" must set Metadata.Mandatory=true")
		}
		instances[m.Name] = p
		metadataByName[m.Name] = m
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
	loaded := make([]loadedPlugin, 0, len(order))
	for _, name := range order {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("plugin host load cancelled before initialising %q: %w", name, err)
		}
		p := instances[name]
		m := metadataByName[name]
		entry := h.cfg[name]

		// Build the set of declared Requires for the scoped resolver.
		declared := make(map[string]bool, len(m.Requires))
		for _, req := range m.Requires {
			declared[req.Name] = true
		}

		db := h.databaseHandle(name)
		if h.pluginDBFactory != nil && db == nil {
			return fmt.Errorf("plugin %q database handle factory returned nil", name)
		}
		if db != nil {
			h.log.Info("[pluginhost] applying plugin migrations", "plugin", name, "namespace", db.PluginName())
			h.mu.Unlock()
			locked = false
			migrationErr := runPluginMigrations(ctx, name, p, db)
			h.mu.Lock()
			locked = true
			if migrationErr != nil {
				return migrationErr
			}
		}

		resolver := &scopedResolver{
			pluginName: name,
			declared:   declared,
			registry:   stagingRegistry,
			db:         db,
			log:        &slogLogger{log: h.log.With("plugin", name)},
			emitFn:     h.emitFn,
			active:     true,
		}

		h.log.Info("[pluginhost] initialising plugin", "plugin", name, "version", m.Version)
		h.mu.Unlock()
		locked = false
		initErr := p.Init(ctx, cloneRawConfig(entry.Config), resolver)
		h.mu.Lock()
		locked = true
		resolver.closeResolveWindow()
		initialised = append(initialised, loadedPlugin{meta: m, plugin: p})
		if initErr != nil {
			return fmt.Errorf("plugin %q Init() failed: %w", name, initErr)
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("plugin host load cancelled after initialising %q: %w", name, err)
		}

		h.mu.Unlock()
		locked = false
		publishErr := publishDeclaredServices(stagingRegistry, name, m, p)
		h.mu.Lock()
		locked = true
		if publishErr != nil {
			return publishErr
		}

		loaded = append(loaded, loadedPlugin{meta: m, plugin: p})
		h.log.Info("[pluginhost] plugin initialised", "plugin", name)
	}

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("plugin host load cancelled before starting plugins: %w", err)
	}

	// Install the local ban cache before any plugin is started. An internal
	// provider can then publish recovered bans during Start, while an external
	// provider gets the same push-only channel without exposing an RPC on the
	// subscription hot path.
	for _, lp := range loaded {
		provider, ok := lp.plugin.(pluginapi.AntifraudProvider)
		if !ok {
			continue
		}
		h.mu.Unlock()
		locked = false
		provider.SetBanSink(h.banCache)
		h.mu.Lock()
		locked = true
	}

	// Start all plugins (each in its own goroutine). The Host owns a child
	// context so Shutdown can stop plugins even if the caller's Load context is
	// still alive (the normal case for a long-running server).
	runCtx, runCancel := context.WithCancel(ctx)
	h.registry = stagingRegistry
	h.loaded = loaded
	h.runCancel = runCancel
	h.starts.Add(len(loaded))
	h.state = hostRunning
	for _, lp := range loaded {
		go func(lp loadedPlugin) {
			defer h.starts.Done()
			h.log.Info("[pluginhost] starting plugin", "plugin", lp.meta.Name)
			if err := lp.plugin.Start(runCtx); err != nil && !errors.Is(err, context.Canceled) {
				h.log.Error("[pluginhost] plugin Start() returned an error",
					"plugin", lp.meta.Name, "error", err)
			}
			h.log.Info("[pluginhost] plugin stopped", "plugin", lp.meta.Name)
		}(lp)
	}

	h.log.Info("[pluginhost] all plugins loaded and started", "count", len(loaded))
	return nil
}

// rollbackInitialised stops every plugin whose Init was entered, including the
// one that returned an error. Init is allowed to allocate resources before it
// detects a configuration or dependency problem, so excluding that last plugin
// would leak those resources.
func (h *Host) rollbackInitialised(ctx context.Context, initialised []loadedPlugin) error {
	cleanupCtx := context.WithoutCancel(ctx)
	var errs []error
	for i := len(initialised) - 1; i >= 0; i-- {
		lp := initialised[i]
		h.log.Info("[pluginhost] rolling back plugin", "plugin", lp.meta.Name)
		if err := lp.plugin.Stop(cleanupCtx); err != nil {
			h.log.Error("[pluginhost] plugin rollback Stop() error", "plugin", lp.meta.Name, "error", err)
			errs = append(errs, fmt.Errorf("plugin %q rollback Stop: %w", lp.meta.Name, err))
		}
	}
	return errors.Join(errs...)
}

// ResolveService retrieves a published service by name. For use in tests and
// internal kernel code that needs to access a service outside a plugin's Init().
func (h *Host) ResolveService(name string) (any, error) {
	h.mu.RLock()
	registry := h.registry
	h.mu.RUnlock()
	return registry.resolve(name)
}

// Shutdown stops all loaded plugins in reverse load order (last-loaded first,
// core plugin last). Each plugin is given the context's deadline to finish.
func (h *Host) Shutdown(ctx context.Context) error {
	if ctx == nil {
		return errors.New("pluginhost: Shutdown context must not be nil")
	}

	h.lifecycleMu.Lock()
	defer h.lifecycleMu.Unlock()

	h.mu.Lock()
	switch h.state {
	case hostNew, hostStopped:
		h.mu.Unlock()
		return nil
	case hostRunning:
		// Continue below.
	default:
		h.mu.Unlock()
		return errors.New("Host.Shutdown() called while plugin host is not running")
	}

	loaded := append([]loadedPlugin(nil), h.loaded...)
	runCancel := h.runCancel
	h.runCancel = nil
	h.state = hostStopping
	h.mu.Unlock()

	if runCancel != nil {
		runCancel()
	}

	err := h.stopPlugins(ctx, loaded, "stopping")
	if waitErr := h.waitForStarts(ctx); waitErr != nil {
		err = errors.Join(err, waitErr)
	}

	h.mu.Lock()
	h.loaded = nil
	h.registry = newServiceRegistry()
	h.state = hostStopped
	h.mu.Unlock()
	return err
}

func (h *Host) stopPlugins(ctx context.Context, plugins []loadedPlugin, action string) error {
	var errs []error
	for i := len(plugins) - 1; i >= 0; i-- {
		lp := plugins[i]
		h.log.Info("[pluginhost] "+action+" plugin", "plugin", lp.meta.Name)
		if err := lp.plugin.Stop(ctx); err != nil {
			h.log.Error("[pluginhost] plugin Stop() error", "plugin", lp.meta.Name, "error", err)
			errs = append(errs, fmt.Errorf("plugin %q Stop: %w", lp.meta.Name, err))
		}
	}
	return errors.Join(errs...)
}

func (h *Host) waitForStarts(ctx context.Context) error {
	if ctx.Err() != nil {
		return fmt.Errorf("wait for plugin Start routines: %w", ctx.Err())
	}
	done := make(chan struct{})
	go func() {
		h.starts.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("wait for plugin Start routines: %w", ctx.Err())
	}
}

// HealthCheck returns a map of plugin name → health error (nil means healthy).
// Called by the health monitor and `xraytool plugin list`.
func (h *Host) HealthCheck(ctx context.Context) map[string]error {
	h.mu.RLock()
	loaded := append([]loadedPlugin(nil), h.loaded...)
	h.mu.RUnlock()

	result := make(map[string]error, len(loaded))
	for _, lp := range loaded {
		healthErr := lp.plugin.Health(ctx)
		if healthErr != nil {
			if restartable, ok := lp.plugin.(restartablePlugin); ok && restartable.IsExternal() {
				h.log.Warn("[pluginhost] external plugin health check failed; attempting restart",
					"plugin", lp.meta.Name, "error", healthErr)
				if restartErr := restartable.Restart(ctx); restartErr != nil {
					healthErr = errors.Join(healthErr, restartErr)
				} else {
					healthErr = nil
				}
			}
			if healthErr != nil && h.emitFn != nil {
				h.emitFn("plugin.crashed", map[string]any{
					"plugin": lp.meta.Name,
					"error":  healthErr.Error(),
				}, nil)
			}
		}
		result[lp.meta.Name] = healthErr
	}
	return result
}

// Restart restarts one external plugin using its configured restart policy.
// Builtin plugins intentionally cannot be restarted as subprocesses: their
// lifecycle is owned by the process and should be restarted with the host.
func (h *Host) Restart(ctx context.Context, name string) error {
	if ctx == nil {
		return errors.New("pluginhost: Restart context must not be nil")
	}
	h.mu.RLock()
	state := h.state
	var target pluginapi.Plugin
	for _, lp := range h.loaded {
		if lp.meta.Name == name {
			target = lp.plugin
			break
		}
	}
	h.mu.RUnlock()
	if state != hostRunning {
		return errors.New("pluginhost: Restart requires a running host")
	}
	if target == nil {
		return fmt.Errorf("pluginhost: plugin %q is not loaded", name)
	}
	restartable, ok := target.(restartablePlugin)
	if !ok || !restartable.IsExternal() {
		return fmt.Errorf("pluginhost: plugin %q is not an external plugin and cannot be restarted", name)
	}
	return restartable.Restart(ctx)
}

// ExternalLogs returns a bounded tail of stderr lines for a loaded external
// plugin. Builtin plugins do not have a subprocess log stream and return a
// descriptive error instead. A non-positive maxLines returns the entire tail.
func (h *Host) ExternalLogs(name string, maxLines int) ([]string, error) {
	h.mu.RLock()
	var target pluginapi.Plugin
	for _, lp := range h.loaded {
		if lp.meta.Name == name {
			target = lp.plugin
			break
		}
	}
	h.mu.RUnlock()
	if target == nil {
		return nil, fmt.Errorf("pluginhost: plugin %q is not loaded", name)
	}
	logs, ok := target.(externalLogProvider)
	if !ok {
		return nil, fmt.Errorf("pluginhost: plugin %q is not an external plugin", name)
	}
	return logs.Logs(maxLines), nil
}

// Loaded returns a snapshot of the currently loaded plugins' metadata.
func (h *Host) Loaded() []pluginapi.Metadata {
	h.mu.RLock()
	defer h.mu.RUnlock()
	metas := make([]pluginapi.Metadata, len(h.loaded))
	for i, lp := range h.loaded {
		metas[i] = cloneMetadata(lp.meta)
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
	h.mu.RLock()
	registry := h.registry
	h.mu.RUnlock()
	svc, err := registry.resolve("antifraud_provider")
	if err != nil {
		return nil, false
	}
	af, ok := svc.(pluginapi.AntifraudProvider)
	return af, ok
}

// BanCache returns the local, push-populated anti-fraud read cache. Callers
// use it for synchronous subscription checks instead of calling a provider
// across a process boundary.
func (h *Host) BanCache() *LocalBanCache {
	h.mu.RLock()
	cache := h.banCache
	h.mu.RUnlock()
	return cache
}

// PaymentProviders returns all loaded payment providers keyed by MethodID.
func (h *Host) PaymentProviders() map[string]pluginapi.PaymentProvider {
	h.mu.RLock()
	defer h.mu.RUnlock()

	result := make(map[string]pluginapi.PaymentProvider)
	for _, lp := range h.loaded {
		if external, ok := lp.plugin.(*externalPlugin); ok && !external.publishesPaymentProvider() {
			continue
		}
		if pp, ok := lp.plugin.(pluginapi.PaymentProvider); ok {
			if methodID := strings.TrimSpace(pp.MethodID()); methodID != "" {
				result[methodID] = pp
			}
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
		if external, ok := lp.plugin.(*externalPlugin); ok && !external.publishes("event_sink") {
			continue
		}
		if es, ok := lp.plugin.(pluginapi.EventSink); ok {
			sinks = append(sinks, es)
		}
	}
	return sinks
}

// PaymentProviderConsumers returns all loaded PaymentProviderConsumer plugins.
func (h *Host) PaymentProviderConsumers() []pluginapi.PaymentProviderConsumer {
	h.mu.RLock()
	defer h.mu.RUnlock()

	var consumers []pluginapi.PaymentProviderConsumer
	for _, lp := range h.loaded {
		if c, ok := lp.plugin.(pluginapi.PaymentProviderConsumer); ok {
			consumers = append(consumers, c)
		}
	}
	return consumers
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
