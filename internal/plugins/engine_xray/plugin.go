// Package engine_xray implements the EngineProvider plugin for Xray-core.
//
// Phase 1.5: thin lifecycle wrapper over vpn.Adapter.
// All Xray logic stays in internal/vpn — we add Plugin lifecycle + EngineProvider bridge.
//
// Published: "engine.softban", "engine.logger_control"
// Required: none (engines load before other plugins)
package engine_xray

import (
	"context"
	"fmt"
	"log/slog"

	"xraytool/internal/domain"
	"xraytool/internal/pluginapi"
	"xraytool/internal/vpn"
)

// pluginConfig holds parsed RawConfig for engine_xray.
type pluginConfig struct {
	GRPCAddr          string
	ConfigPath        string
	TemplatePath      string
	RealityRotation   bool
	RealityKeysPath   string
	BlacklistedAdmins []string
}

// Plugin wraps vpn.Adapter as a pluginapi.EngineProvider.
type Plugin struct {
	log     *slog.Logger
	adapter *vpn.Adapter
	cfg     pluginConfig
}

// New creates an uninitialised plugin.
func New() *Plugin { return &Plugin{} }

// NewFromAdapter wraps an already-constructed vpn.Adapter.
// Used by the kernel during Phase 1.5 transition so the engine is only
// built once — not duplicated between the old server.go path and the plugin.
func NewFromAdapter(a *vpn.Adapter) *Plugin {
	return &Plugin{
		log:     slog.Default().With("plugin", "engine_xray"),
		adapter: a,
	}
}

// ── pluginapi.Plugin ──────────────────────────────────────────────────────────

func (p *Plugin) Metadata() pluginapi.Metadata {
	return pluginapi.Metadata{
		Name:        "engine_xray",
		Kind:        "engine",
		Version:     "1.0.0",
		APIVersion:  pluginapi.CurrentAPIVersion,
		Description: "Xray-core VPN engine: gRPC user management, traffic stats, log rotation.",
		Mandatory:   false,
		Requires:    nil,
		Publishes: []pluginapi.ServiceRef{
			{Name: "engine.softban"},
			{Name: "engine.logger_control"},
		},
	}
}

func (p *Plugin) Init(_ context.Context, rawCfg pluginapi.RawConfig, reg pluginapi.ServiceResolver) error {
	p.log = slog.Default().With("plugin", "engine_xray")

	// If the adapter was already provided via NewFromAdapter(), skip construction.
	if p.adapter != nil {
		if reg != nil {
			reg.Logger().Info("engine_xray: using pre-constructed adapter")
		}
		return nil
	}

	p.cfg = parseConfig(rawCfg)
	p.adapter = vpn.NewAdapter(
		p.cfg.GRPCAddr,
		p.cfg.ConfigPath,
		p.cfg.TemplatePath,
		p.cfg.RealityRotation,
		p.cfg.RealityKeysPath,
		p.cfg.BlacklistedAdmins,
		slog.Default(),
	)

	if reg != nil {
		reg.Logger().Info("engine_xray: adapter created",
			"grpc_addr", p.cfg.GRPCAddr,
			"template", p.cfg.TemplatePath,
		)
	}
	return nil
}

// Start is a no-op — Xray runs as an external systemd service.
// Blocks until ctx is cancelled (pluginhost contract).
func (p *Plugin) Start(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

func (p *Plugin) Stop(_ context.Context) error { return nil }

func (p *Plugin) Health(_ context.Context) error {
	if p.adapter == nil {
		return fmt.Errorf("engine_xray: adapter not initialised")
	}
	return nil
}

// ── pluginapi.EngineProvider ──────────────────────────────────────────────────

func (p *Plugin) ID() string { return "xray" }

// PublishedServices exposes the engine capabilities that optional plugins can
// resolve during Init. Both names intentionally point at the same provider;
// they model two narrow capabilities of the underlying adapter.
func (p *Plugin) PublishedServices() map[string]any {
	if p.adapter == nil {
		return nil
	}
	return map[string]any{
		"engine.softban":        p,
		"engine.logger_control": p,
	}
}

func (p *Plugin) AddUser(ctx context.Context, u pluginapi.VPNUserConfig) error {
	return p.adapter.AddUser(ctx, toDomain(u))
}

func (p *Plugin) AddUsersBulk(ctx context.Context, users []pluginapi.VPNUserConfig) error {
	return p.adapter.AddUsersBulk(ctx, toDomainSlice(users))
}

func (p *Plugin) RemoveUser(ctx context.Context, email string) error {
	return p.adapter.RemoveUser(ctx, email)
}

func (p *Plugin) RemoveUsersBulk(ctx context.Context, emails []string) error {
	return p.adapter.RemoveUsersBulk(ctx, emails)
}

func (p *Plugin) SetExpire(ctx context.Context, email string, expire string) error {
	return p.adapter.SetExpire(ctx, email, expire)
}

func (p *Plugin) SetLimit(ctx context.Context, email string, limit float64) error {
	return p.adapter.SetLimit(ctx, email, limit)
}

func (p *Plugin) RebuildInbound(ctx context.Context, tag string) error {
	return p.adapter.RebuildInbound(ctx, tag)
}

func (p *Plugin) QueryStats(ctx context.Context) ([]pluginapi.TrafficStat, error) {
	stats, err := p.adapter.QueryStats(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]pluginapi.TrafficStat, len(stats))
	for i, s := range stats {
		out[i] = pluginapi.TrafficStat{Email: s.Email, Up: s.Up, Down: s.Down}
	}
	return out, nil
}

func (p *Plugin) BanUser(ctx context.Context, email string) error {
	return p.adapter.BanUser(ctx, email)
}

func (p *Plugin) UnbanUser(ctx context.Context, email string) error {
	return p.adapter.UnbanUser(ctx, email)
}

func (p *Plugin) RestartLogger(ctx context.Context) error {
	return p.adapter.RestartLogger(ctx)
}

func (p *Plugin) ListUsers(ctx context.Context) ([]pluginapi.VPNUserConfig, error) {
	users, err := p.adapter.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]pluginapi.VPNUserConfig, len(users))
	for i, u := range users {
		out[i] = fromDomain(u)
	}
	return out, nil
}

func (p *Plugin) SyncUsers(ctx context.Context, dbUsers []pluginapi.VPNUserConfig, removeOrphans bool) (*pluginapi.EngineSyncResult, error) {
	result, err := p.adapter.SyncUsers(ctx, toDomainSlice(dbUsers), removeOrphans)
	if err != nil {
		return nil, err
	}
	return &pluginapi.EngineSyncResult{Added: result.Added, Removed: result.Removed}, nil
}

// ── pluginapi.ClientConfigContributor ────────────────────────────────────────

// BuildClientLinks returns share links for the user.
// Phase 1.5: stub — returns nil until Phase 2.6.4 when subscription.go
// is refactored to use the plugin's links instead of direct vpn.RawConfig parsing.
func (p *Plugin) BuildClientLinks(_ context.Context, _ pluginapi.VPNUserConfig) ([]pluginapi.ClientLink, error) {
	return nil, nil
}

// ── Adapter accessor (Phase 1.5 → Phase 3 bridge) ────────────────────────────

// Adapter returns the underlying vpn.Adapter.
// Used by server_kernel.go to pass the engine to code that still accepts
// domain.Engine directly (workers, statesync, router).
// Removed in Phase 3 when all callers use EngineProvider via ServiceRegistry.
func (p *Plugin) Adapter() *vpn.Adapter { return p.adapter }

// DomainEngine returns the adapter as domain.Engine.
// Convenience method so the kernel doesn't need to cast.
func (p *Plugin) DomainEngine() domain.Engine { return p.adapter }

// ── type conversion helpers ───────────────────────────────────────────────────

func toDomain(u pluginapi.VPNUserConfig) domain.VPNUserConfig {
	return domain.VPNUserConfig{
		Email:      u.Email,
		UUID:       u.UUID,
		Auth:       u.Auth,
		Subfile:    u.Subfile,
		Expire:     u.Expire,
		MaxDevices: u.MaxDevices,
		Flow:       u.Flow,
		Cipher:     u.Cipher,
	}
}

func fromDomain(u domain.VPNUserConfig) pluginapi.VPNUserConfig {
	return pluginapi.VPNUserConfig{
		Email:      u.Email,
		UUID:       u.UUID,
		Auth:       u.Auth,
		Subfile:    u.Subfile,
		Expire:     u.Expire,
		MaxDevices: u.MaxDevices,
		Flow:       u.Flow,
		Cipher:     u.Cipher,
	}
}

func toDomainSlice(users []pluginapi.VPNUserConfig) []domain.VPNUserConfig {
	out := make([]domain.VPNUserConfig, len(users))
	for i, u := range users {
		out[i] = toDomain(u)
	}
	return out
}

func parseConfig(raw pluginapi.RawConfig) pluginConfig {
	var cfg pluginConfig
	if raw == nil {
		return cfg
	}
	if v, ok := raw["grpc_addr"].(string); ok {
		cfg.GRPCAddr = v
	}
	if v, ok := raw["config_path"].(string); ok {
		cfg.ConfigPath = v
	}
	if v, ok := raw["template_path"].(string); ok {
		cfg.TemplatePath = v
	}
	if v, ok := raw["reality_rotation"].(bool); ok {
		cfg.RealityRotation = v
	}
	if v, ok := raw["reality_keys_path"].(string); ok {
		cfg.RealityKeysPath = v
	}
	if v, ok := raw["blacklisted_admins"].([]interface{}); ok {
		for _, item := range v {
			if s, ok := item.(string); ok {
				cfg.BlacklistedAdmins = append(cfg.BlacklistedAdmins, s)
			}
		}
	}
	return cfg
}

// Compile-time interface checks.
var _ pluginapi.Plugin = (*Plugin)(nil)
var _ pluginapi.EngineProvider = (*Plugin)(nil)
var _ pluginapi.ServiceProvider = (*Plugin)(nil)
