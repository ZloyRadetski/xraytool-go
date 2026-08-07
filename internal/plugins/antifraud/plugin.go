// Package antifraud_plugin implements the AntifraudProvider plugin.
//
// Phase 1.1: mechanical wrap of internal/antifraud. Business logic stays in
// internal/antifraud — this plugin adds Plugin lifecycle glue (Init/Start/Stop/Health)
// and bridges the pluginapi.AntifraudProvider contract to the existing module.
//
// Metadata:
//
//	Name: "antifraud"
//	Kind: "antifraud"
//
// Required services: "domain_registry"
// Published services: "antifraud_provider"
//
// Two-phase init (same pattern as core plugin — Phase 1 tech-debt):
//
//	Init() → reads config from RawConfig, records the parsed cfg.
//	InitWithDependencies() → constructs antifraud.Module with runtime deps
//	                          (called by kernel after engine is available).
//
// The slave fraud-reporter adapter is still constructed by the kernel because
// it depends on slave.Client which requires network config. Will move into
// this plugin in Phase 2 when slave becomes its own plugin.
package antifraud_plugin

import (
	"context"
	"fmt"
	"log/slog"

	"xraytool/internal/domain"
	"xraytool/internal/pluginapi"
)

// Plugin wraps antifraud.Module as a pluginapi.AntifraudProvider.
type Plugin struct {
	log     *slog.Logger
	module  *Module
	cfg     *Config
	runtime Runtime

	// banSink is set by the host via SetBanSink before Start().
	// For Phase 1 we don't use the push model (the module still maintains its
	// own in-memory banStore). The sink is stored for Phase 2 migration.
	banSink pluginapi.BanUpdateSink
}

// Runtime contains the kernel-owned adapters needed by the existing
// anti-fraud module. It is injected before Host.Load so Init can construct the
// module before Start and publish a ready provider to dependent code.
type Runtime struct {
	Registry   domain.Registry
	Banner     domain.SoftBanner
	LoggerCtl  domain.LoggerController
	Propagator domain.EventPropagator
	Reporter   domain.FraudEventReporter
}

// New creates an uninitialised plugin.
func New() *Plugin { return &Plugin{} }

// NewWithRuntime creates a plugin ready for normal Host.Load lifecycle.
func NewWithRuntime(runtime Runtime) *Plugin { return &Plugin{runtime: runtime} }

// ── pluginapi.Plugin ──────────────────────────────────────────────────────────

func (p *Plugin) Metadata() pluginapi.Metadata {
	return pluginapi.Metadata{
		Name:        "antifraud",
		Kind:        "antifraud",
		Version:     "1.0.0",
		APIVersion:  pluginapi.CurrentAPIVersion,
		Description: "Multi-IP anti-fraud: log tailing, IP tracking, soft-ban management.",
		Mandatory:   false,
		Requires: []pluginapi.ServiceRef{
			{Name: "domain_registry", Optional: false},
		},
		Publishes: []pluginapi.ServiceRef{
			{Name: "antifraud_provider"},
		},
	}
}

func (p *Plugin) Init(_ context.Context, rawCfg pluginapi.RawConfig, reg pluginapi.ServiceResolver) error {
	p.log = slog.Default().With("plugin", "antifraud")
	p.cfg = parseConfig(rawCfg)

	registry := p.runtime.Registry
	if registry == nil && reg != nil {
		resolved, err := reg.Resolve("domain_registry")
		if err != nil {
			return fmt.Errorf("antifraud: resolve domain_registry: %w", err)
		}
		var ok bool
		registry, ok = resolved.(domain.Registry)
		if !ok {
			return fmt.Errorf("antifraud: domain_registry has unexpected type %T", resolved)
		}
	}
	if registry != nil && p.runtime.Banner != nil && p.runtime.LoggerCtl != nil {
		p.module = NewModule(
			p.cfg,
			registry,
			p.runtime.Banner,
			p.runtime.LoggerCtl,
			p.runtime.Propagator,
			p.runtime.Reporter,
			slog.Default(),
		)
		if p.banSink != nil {
			p.module.SetBanSink(p.banSink)
		}
	}
	if reg != nil {
		reg.Logger().Info("antifraud: config parsed",
			"enabled", p.cfg.Enabled,
			"dry_run", p.cfg.DryRun,
			"log_path", p.cfg.LogPath,
		)
	}
	return nil
}

// InitWithDependencies is called by the kernel after Init(), injecting the
// runtime dependencies that come from the loaded engine and core plugin.
// Phase 1 tech-debt: in Phase 2 these will be Resolved from ServiceRegistry.
func (p *Plugin) InitWithDependencies(
	registry domain.Registry,
	banner domain.SoftBanner,
	loggerCtl domain.LoggerController,
	propagator domain.EventPropagator,
	reporter domain.FraudEventReporter, // nil if no slave fraud reporting
) error {
	if registry == nil {
		return fmt.Errorf("antifraud plugin: registry must not be nil")
	}
	p.module = NewModule(
		p.cfg,
		registry,
		banner,
		loggerCtl,
		propagator,
		reporter,
		slog.Default(),
	)
	if p.banSink != nil {
		p.module.SetBanSink(p.banSink)
	}
	return nil
}

// PublishedServices exposes the ready provider to the Host. The provider is
// safe to publish before Start because all stateful dependencies are created
// during Init when Runtime is supplied.
func (p *Plugin) PublishedServices() map[string]any {
	if p.module == nil {
		return nil
	}
	return map[string]any{"antifraud_provider": p}
}

// Start runs the antifraud module goroutines. Blocks until ctx is cancelled.
func (p *Plugin) Start(ctx context.Context) error {
	if p.module == nil {
		p.log.Warn("antifraud: Start() called without initialised module — disabled")
		<-ctx.Done()
		return nil
	}
	p.module.Run(ctx)
	return nil
}

func (p *Plugin) Stop(_ context.Context) error { return nil }
func (p *Plugin) Health(_ context.Context) error {
	if p.module == nil && p.cfg != nil && p.cfg.Enabled {
		return fmt.Errorf("antifraud: module not initialised (InitWithDependencies not called)")
	}
	return nil
}

// ── pluginapi.AntifraudProvider ───────────────────────────────────────────────

// SetBanSink registers the kernel's local ban cache as the target for push updates.
// Phase 1: stored but not used — module still uses its own banStore.
// Phase 2: module will call sink.PushBanUpdate / sink.PushUnban instead of banStore.
func (p *Plugin) SetBanSink(sink pluginapi.BanUpdateSink) {
	p.banSink = sink
	if p.module != nil {
		p.module.SetBanSink(sink)
	}
}

// IsBanned reports whether the given email is currently soft-banned.
// Reads from in-memory banStore — no I/O.
func (p *Plugin) IsBanned(email string) bool {
	if p.module == nil {
		return false
	}
	return p.module.IsBanned(email)
}

// ForceUnban lifts an active ban immediately (admin action).
func (p *Plugin) ForceUnban(_ context.Context, email string) error {
	if p.module == nil {
		return nil
	}
	p.module.ForceUnban(email)
	return nil
}

// Snapshot returns the current state of the ban/IP-tracking store for diagnostics.
func (p *Plugin) Snapshot(_ context.Context) (map[string]any, error) {
	if p.module == nil {
		return map[string]any{}, nil
	}
	snap := p.module.GetSnapshot()
	return map[string]any{
		"state":         snap.State,
		"active_slaves": snap.ActiveSlaves,
	}, nil
}

// IngestEvents processes a batch of suspicious-IP events from slave nodes.
// Bridges pluginapi.FraudEvent → domain.FraudEvent.
func (p *Plugin) IngestEvents(_ context.Context, sourceID string, events []pluginapi.FraudEvent) error {
	if p.module == nil {
		return nil
	}
	domainEvts := make([]domain.FraudEvent, len(events))
	for i, e := range events {
		domainEvts[i] = domain.FraudEvent{Email: e.Email, IP: e.IP}
	}
	p.module.IngestEvents(sourceID, domainEvts)
	return nil
}

// ── Convenience accessors (used by kernel in Phase 1) ────────────────────────

// Module returns the underlying antifraud.Module.
// Used by kernel to wire WithAntiFraud hooks into server.Router.
// Will be removed in Phase 3.
func (p *Plugin) Module() *Module { return p.module }

// Config returns the parsed config (for kernel to check cfg.Enabled etc.).
func (p *Plugin) Config() *Config { return p.cfg }

// ── helpers ───────────────────────────────────────────────────────────────────

func parseConfig(raw pluginapi.RawConfig) *Config {
	cfg := &Config{
		Enabled:               false,
		DryRun:                false,
		SuspiciousIPThreshold: 3,
		IPLimitTTL:            "3m",
		BanDuration:           "24h",
		LogRotationSizeMB:     50,
		LogRotationMaxAge:     "5m",
		APIKey:                "xraytool_default_antifraud_salt_secret",
	}
	if raw == nil {
		return cfg
	}
	if v, ok := raw["enabled"].(bool); ok {
		cfg.Enabled = v
	}
	if v, ok := raw["dry_run"].(bool); ok {
		cfg.DryRun = v
	}
	if v, ok := raw["log_path"].(string); ok {
		cfg.LogPath = v
	}
	if v, ok := raw["max_ips"].(int); ok {
		cfg.SuspiciousIPThreshold = v
	}
	if v, ok := raw["ip_limit_ttl"].(string); ok {
		cfg.IPLimitTTL = v
	}
	if v, ok := raw["ban_duration"].(string); ok {
		cfg.BanDuration = v
	}
	if v, ok := raw["log_rotation_size_mb"].(int); ok {
		cfg.LogRotationSizeMB = v
	}
	if v, ok := raw["log_rotation_max_age"].(string); ok {
		cfg.LogRotationMaxAge = v
	}
	if v, ok := raw["report_to_master"].(bool); ok {
		cfg.ReportToMaster = v
	}
	if v, ok := raw["salt_secret"].(string); ok && v != "" {
		cfg.APIKey = v
	}
	if v, ok := raw["is_master"].(bool); ok {
		cfg.IsMaster = v
	}
	return cfg
}

// Compile-time interface checks.
var _ pluginapi.Plugin = (*Plugin)(nil)
var _ pluginapi.AntifraudProvider = (*Plugin)(nil)
var _ pluginapi.ServiceProvider = (*Plugin)(nil)
