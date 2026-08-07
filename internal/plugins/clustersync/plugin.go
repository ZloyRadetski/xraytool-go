// Package clustersync exposes the existing master-to-slave synchronisation
// implementation as an optional ClusterSyncProvider plugin.
//
// The transport and state-machine implementations deliberately remain in
// internal/slave and internal/statesync for now. This package owns only the
// plugin boundary and lifecycle: a single-node installation does not load it,
// hence does not start a sync worker or retain cluster provider state.
package clustersync

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"sync"
	"time"

	"xraytool/internal/domain"
	"xraytool/internal/pluginapi"

	"xraytool/internal/plugins/clustersync/statesync"

)

const (
	// ServiceClusterSyncProvider is the stable service-registry name published
	// by this plugin. Kept as an alias for plugin consumers while the canonical
	// cross-boundary constant lives in pluginapi.
	ServiceClusterSyncProvider = pluginapi.ServiceClusterSyncProvider

	defaultSyncInterval  = 3 * time.Minute
	defaultPurgeInterval = 24 * time.Hour
)

// Config is the typed configuration understood by the cluster-sync plugin.
//
// Example:
//
//	plugins:
//	  cluster_sync:
//	    enabled: true
//	    source: builtin
//	    config:
//	      sync_interval: "3m"
//	      sync_on_start: true
type Config struct {
	// SyncInterval controls how often the master reconciles its local engine
	// and replicates the state to slaves. It must be positive.
	SyncInterval time.Duration

	// SyncOnStart runs one reconciliation immediately when the plugin starts.
	SyncOnStart bool
}

// Runtime contains the kernel-owned adapters used by the legacy cluster
// implementation. The wrapper accepts a pre-built SyncService to preserve the
// current construction order while it is still needed by legacy HTTP routes.
// When SyncService is nil, the plugin constructs one from Registry, Engine and
// SlaveProvider.
//
// A SlaveProvider that itself needs a *statesync.Service (the current
// slave.NewStateSyncProvider implementation does) should be paired with that
// already-wired service through SyncService. This avoids creating two separate
// synchronisation state machines during the migration.
type Runtime struct {
	Registry      domain.Registry
	Engine        domain.Engine
	SyncService   *statesync.Service
	SlaveProvider domain.StateSyncSlaveProvider
	StatsProvider domain.ClusterStatsProvider
	// Propagator broadcasts point-in-time admin commands (newuser/rmuser) to
	// configured slaves. It is optional because scheduled synchronisation can
	// still operate without this legacy command transport.
	Propagator domain.EventPropagator
	Logger     *slog.Logger
}

// Plugin wraps statesync.Service and domain.ClusterStatsProvider as the
// pluginapi.ClusterSyncProvider extension point.
type Plugin struct {
	runtime Runtime

	mu               sync.RWMutex
	log              *slog.Logger
	cfg              Config
	syncService      *statesync.Service
	statsProvider    domain.ClusterStatsProvider
	propagator       domain.EventPropagator
	subscriptionRepo pluginapi.SubscriptionRepository
	initialized      bool
	running          bool
	runCancel        context.CancelFunc
	lastPurge        time.Time
}

// New creates an uninitialised plugin. It is useful for factories and tests;
// callers must inject Runtime before Init, using NewWithRuntime in production.
func New() *Plugin { return &Plugin{} }

// NewWithRuntime creates a plugin whose legacy dependencies were composed by
// the kernel before Host.Load.
func NewWithRuntime(runtime Runtime) *Plugin { return &Plugin{runtime: runtime} }

// Metadata describes the plugin boundary to the Host.
func (p *Plugin) Metadata() pluginapi.Metadata {
	return pluginapi.Metadata{
		Name:        "cluster_sync",
		Kind:        "cluster_sync",
		Version:     "1.0.0",
		APIVersion:  pluginapi.CurrentAPIVersion,
		Description: "Master-to-slave state replication and cluster traffic aggregation.",
		Mandatory:   false,
		Requires: []pluginapi.ServiceRef{
			{Name: "subscription_repository", Optional: false},
		},
		Publishes: []pluginapi.ServiceRef{
			{Name: ServiceClusterSyncProvider},
		},
	}
}

// Init validates the plugin configuration, resolves its explicit core
// dependency, and binds the kernel-owned cluster adapters.
func (p *Plugin) Init(_ context.Context, rawCfg pluginapi.RawConfig, reg pluginapi.ServiceResolver) error {
	cfg, err := parseConfig(rawCfg)
	if err != nil {
		return fmt.Errorf("cluster_sync: config error: %w", err)
	}

	var subscriptionRepo pluginapi.SubscriptionRepository
	if reg != nil {
		service, err := reg.Resolve("subscription_repository")
		if err != nil {
			return fmt.Errorf("cluster_sync: resolve subscription_repository: %w", err)
		}
		var ok bool
		subscriptionRepo, ok = service.(pluginapi.SubscriptionRepository)
		if !ok || isNil(subscriptionRepo) {
			return fmt.Errorf("cluster_sync: subscription_repository has unexpected type %T", service)
		}
	}

	if isNil(p.runtime.Registry) {
		return fmt.Errorf("cluster_sync: runtime Registry is required")
	}
	if isNil(p.runtime.Engine) {
		return fmt.Errorf("cluster_sync: runtime Engine is required")
	}
	if isNil(p.runtime.SlaveProvider) {
		return fmt.Errorf("cluster_sync: runtime SlaveProvider is required")
	}
	if isNil(p.runtime.StatsProvider) {
		return fmt.Errorf("cluster_sync: runtime StatsProvider is required")
	}

	log := p.runtime.Logger
	if log == nil {
		log = slog.Default()
	}
	log = log.With("plugin", "cluster_sync")

	syncService := p.runtime.SyncService
	if syncService == nil {
		syncService = statesync.NewService(p.runtime.Registry, p.runtime.Engine, p.runtime.SlaveProvider, log)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.running {
		return fmt.Errorf("cluster_sync: cannot initialise while running")
	}
	p.log = log
	p.cfg = cfg
	p.syncService = syncService
	p.statsProvider = p.runtime.StatsProvider
	p.propagator = p.runtime.Propagator
	p.subscriptionRepo = subscriptionRepo
	p.lastPurge = time.Time{}
	p.initialized = true

	if reg != nil {
		reg.Logger().Info("cluster_sync: initialised", "sync_interval", cfg.SyncInterval, "sync_on_start", cfg.SyncOnStart)
	}
	return nil
}

// Start runs the cluster reconciliation worker until the host cancels its
// context. Failures of an individual sync are logged and do not stop the
// plugin; the next interval provides the retry.
func (p *Plugin) Start(ctx context.Context) error {
	p.mu.Lock()
	if !p.initialized || p.syncService == nil || isNil(p.statsProvider) {
		p.mu.Unlock()
		return fmt.Errorf("cluster_sync: plugin is not initialised")
	}
	if p.running {
		p.mu.Unlock()
		return fmt.Errorf("cluster_sync: plugin is already running")
	}
	runCtx, cancel := context.WithCancel(ctx)
	p.running = true
	p.runCancel = cancel
	cfg := p.cfg
	p.mu.Unlock()

	defer func() {
		cancel()
		p.mu.Lock()
		p.running = false
		p.runCancel = nil
		p.mu.Unlock()
	}()

	if cfg.SyncOnStart {
		p.syncOnce(runCtx)
	}

	ticker := time.NewTicker(cfg.SyncInterval)
	defer ticker.Stop()
	for {
		select {
		case <-runCtx.Done():
			return nil
		case <-ticker.C:
			p.syncOnce(runCtx)
		}
	}
}

// Stop explicitly cancels a running worker. Host.Shutdown also cancels the
// Start context before calling Stop; this method makes direct lifecycle use
// follow the same contract.
func (p *Plugin) Stop(_ context.Context) error {
	p.mu.RLock()
	cancel := p.runCancel
	p.mu.RUnlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

// Health reports whether all required runtime adapters were bound during Init.
// It deliberately does not perform network I/O: live slave reachability is
// represented by the report returned from CollectSlaveTotals/SyncAllSlaves.
func (p *Plugin) Health(_ context.Context) error {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if !p.initialized || p.syncService == nil || isNil(p.statsProvider) {
		return fmt.Errorf("cluster_sync: not initialised")
	}
	if p.cfg.SyncInterval <= 0 {
		return fmt.Errorf("cluster_sync: invalid sync interval %s", p.cfg.SyncInterval)
	}
	return nil
}

// PublishedServices exposes this plugin through the generic service registry.
func (p *Plugin) PublishedServices() map[string]any {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if !p.initialized {
		return nil
	}
	return map[string]any{ServiceClusterSyncProvider: p}
}

// SyncAllSlaves replicates the current master state and maps legacy domain
// values to the plugin API contract.
func (p *Plugin) SyncAllSlaves(ctx context.Context, dryRun bool, forceFull bool) ([]pluginapi.SyncResult, error) {
	p.mu.RLock()
	syncService := p.syncService
	p.mu.RUnlock()
	if syncService == nil {
		return nil, fmt.Errorf("cluster_sync: plugin is not initialised")
	}

	results, err := syncService.SyncAllSlaves(ctx, dryRun, forceFull)
	if err != nil {
		return nil, err
	}
	return toPluginSyncResults(results), nil
}

// CollectSlaveTotals aggregates the traffic reported by all reachable slave
// nodes and maps legacy domain values to the plugin API contract.
func (p *Plugin) CollectSlaveTotals() ([]pluginapi.SlaveUserTotal, pluginapi.SlaveReport) {
	p.mu.RLock()
	provider := p.statsProvider
	p.mu.RUnlock()
	if isNil(provider) {
		return nil, pluginapi.SlaveReport{}
	}

	totals, report := provider.CollectSlaveTotals()
	out := make([]pluginapi.SlaveUserTotal, len(totals))
	for i, total := range totals {
		out[i] = pluginapi.SlaveUserTotal{Email: total.Email, Slave: total.Slave}
	}
	return out, pluginapi.SlaveReport{
		Enabled:       report.Enabled,
		TotalServers:  report.TotalServers,
		OKServers:     report.OKServers,
		FailedServers: report.FailedServers,
	}
}

// BuildSnapshot exposes the master's active user snapshot through the plugin
// API. HTTP handlers consume this interface instead of internal/statesync.
func (p *Plugin) BuildSnapshot(ctx context.Context) ([]pluginapi.VPNUserConfig, error) {
	p.mu.RLock()
	syncService := p.syncService
	p.mu.RUnlock()
	if syncService == nil {
		return nil, fmt.Errorf("cluster_sync: plugin is not initialised")
	}

	users, err := syncService.BuildSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]pluginapi.VPNUserConfig, len(users))
	for i, user := range users {
		out[i] = pluginapi.VPNUserConfig{
			Email:                 user.Email,
			UUID:                  user.UUID,
			Auth:                  user.Auth,
			Subfile:               user.Subfile,
			Expire:                user.Expire,
			MaxDevices:            user.MaxDevices,
			Flow:                  user.Flow,
			Cipher:                user.Cipher,
			PlanEngineIDs:         append([]string(nil), user.PlanEngineIDs...),
			SubscriptionEngineIDs: append([]string(nil), user.SubscriptionEngineIDs...),
		}
	}
	return out, nil
}

// MasterState exposes the master's synchronisation cursor through the plugin
// API, keeping HTTP routing independent of the statesync package.
func (p *Plugin) MasterState(ctx context.Context) (pluginapi.SyncState, error) {
	p.mu.RLock()
	syncService := p.syncService
	p.mu.RUnlock()
	if syncService == nil {
		return pluginapi.SyncState{}, fmt.Errorf("cluster_sync: plugin is not initialised")
	}

	state, err := syncService.MasterState(ctx)
	if err != nil {
		return pluginapi.SyncState{}, err
	}
	return pluginapi.SyncState{
		LastEventID: state.LastEventID,
		StateHash:   state.StateHash,
		UpdatedAt:   state.UpdatedAt,
	}, nil
}

// PropagateCommand delegates short-lived admin commands to the cluster
// transport supplied by the runtime. Router callers run this asynchronously;
// this method itself remains synchronous so errors can be logged centrally.
func (p *Plugin) PropagateCommand(_ context.Context, command string, params map[string]string) error {
	p.mu.RLock()
	propagator := p.propagator
	initialized := p.initialized
	p.mu.RUnlock()
	if !initialized {
		return fmt.Errorf("cluster_sync: plugin is not initialised")
	}
	if isNil(propagator) {
		return fmt.Errorf("cluster_sync: command propagation is unavailable")
	}
	if command == "" {
		return fmt.Errorf("cluster_sync: command must not be empty")
	}
	propagator.PropagateAll(command, params)
	return nil
}

// Config returns the parsed plugin configuration. It is primarily useful to
// the composition root while legacy worker wiring is being removed.
func (p *Plugin) Config() Config {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.cfg
}

func (p *Plugin) syncOnce(ctx context.Context) {
	p.mu.RLock()
	syncService := p.syncService
	log := p.log
	lastPurge := p.lastPurge
	p.mu.RUnlock()
	if syncService == nil {
		return
	}
	if log == nil {
		log = slog.Default().With("plugin", "cluster_sync")
	}

	changed, err := syncService.SelfHealMasterUUIDs(ctx)
	if err != nil {
		log.Error("cluster_sync: self-healing master users failed", "err", err)
	} else if changed {
		log.Info("cluster_sync: self-healing master users completed")
	}

	results, err := syncService.SyncAllSlaves(ctx, false, false)
	if err != nil {
		log.Error("cluster_sync: syncing slaves failed", "err", err)
	} else if results != nil {
		for _, result := range results {
			if result.Success {
				log.Info("cluster_sync: slave synchronised", "server", result.ServerName)
			} else {
				log.Error("cluster_sync: slave synchronisation failed", "server", result.ServerName, "err", result.Error)
			}
		}
	}

	if time.Since(lastPurge) < defaultPurgeInterval {
		return
	}
	syncService.PurgeOldEvents(ctx)
	p.mu.Lock()
	p.lastPurge = time.Now()
	p.mu.Unlock()
}

func parseConfig(raw pluginapi.RawConfig) (Config, error) {
	cfg := Config{
		SyncInterval: defaultSyncInterval,
		SyncOnStart:  true,
	}
	if raw == nil {
		return cfg, nil
	}
	if value, exists := raw["sync_interval"]; exists {
		switch value := value.(type) {
		case string:
			if value == "" {
				return Config{}, fmt.Errorf("sync_interval must not be empty")
			}
			interval, err := time.ParseDuration(value)
			if err != nil {
				return Config{}, fmt.Errorf("invalid sync_interval %q: %w", value, err)
			}
			cfg.SyncInterval = interval
		case time.Duration:
			cfg.SyncInterval = value
		default:
			return Config{}, fmt.Errorf("sync_interval must be a duration string, got %T", value)
		}
	}
	if value, exists := raw["sync_on_start"]; exists {
		enabled, ok := value.(bool)
		if !ok {
			return Config{}, fmt.Errorf("sync_on_start must be a boolean, got %T", value)
		}
		cfg.SyncOnStart = enabled
	}
	if cfg.SyncInterval <= 0 {
		return Config{}, fmt.Errorf("sync_interval must be positive")
	}
	return cfg, nil
}

func toPluginSyncResults(results []domain.SyncResult) []pluginapi.SyncResult {
	out := make([]pluginapi.SyncResult, len(results))
	for i, result := range results {
		out[i] = pluginapi.SyncResult{
			ServerName: result.ServerName,
			Success:    result.Success,
			Error:      result.Error,
		}
	}
	return out
}

func isNil(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

var _ pluginapi.Plugin = (*Plugin)(nil)
var _ pluginapi.ClusterSyncProvider = (*Plugin)(nil)
var _ pluginapi.ClusterSyncHTTPProvider = (*Plugin)(nil)
var _ pluginapi.ClusterCommandPropagator = (*Plugin)(nil)
var _ pluginapi.ServiceProvider = (*Plugin)(nil)
