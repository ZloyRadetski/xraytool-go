// Package engine_xray implements the EngineProvider plugin for Xray-core.
//
// Phase 1.5: thin lifecycle wrapper over Adapter.
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
)

// pluginConfig holds parsed RawConfig for engine_xray.
type pluginConfig struct {
	GRPCAddr     string
	ConfigPath   string
	TemplatePath string
	// ServerAddress is the public address clients use to reach this Xray
	// instance. It is deliberately plugin-owned: an engine must not infer a
	// public endpoint from the kernel HTTP listener.
	ServerAddress string
	// Hy2ConfigYAML is the optional legacy Hysteria2 configuration. Some
	// deployments keep the salamander password there rather than in Xray's
	// inbound settings.
	Hy2ConfigYAML     string
	RealityRotation   bool
	RealityKeysPath   string
	BlacklistedAdmins []string
}

// Plugin wraps Adapter as a pluginapi.EngineProvider.
type Plugin struct {
	log     *slog.Logger
	engine  domain.Engine
	adapter *Adapter // present only when this plugin constructed the adapter
	cfg     pluginConfig
}

// New creates an uninitialised plugin.
func New() *Plugin { return &Plugin{} }

// NewFromAdapter wraps an already-constructed Adapter.
// Used by the kernel during Phase 1.5 transition so the engine is only
// built once — not duplicated between the old server.go path and the plugin.
func NewFromAdapter(a *Adapter) *Plugin {
	return &Plugin{
		log:     slog.Default().With("plugin", "engine_xray"),
		engine:  a,
		adapter: a,
	}
}

// NewFromEngine wraps an already-constructed domain engine. It is used by the
// kernel while the state-sync EventAwareEngine still owns mutation recording:
// the plugin exposes the very same engine instance instead of constructing a
// second Xray adapter with divergent state.
func NewFromEngine(engine domain.Engine) *Plugin {
	p := &Plugin{
		log:    slog.Default().With("plugin", "engine_xray"),
		engine: engine,
	}
	if adapter, ok := engine.(*Adapter); ok {
		p.adapter = adapter
	}
	return p
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
			{Name: pluginapi.ServiceClientConfigContributor},
			{Name: pluginapi.ServiceSubscriptionConfigProvider},
		},
	}
}

func (p *Plugin) Init(_ context.Context, rawCfg pluginapi.RawConfig, reg pluginapi.ServiceResolver) error {
	p.log = slog.Default().With("plugin", "engine_xray")
	// Parse this even when the kernel supplied an already-constructed engine:
	// BuildClientLinks owns its Xray-specific configuration independently of
	// the domain.Engine lifecycle bridge.
	p.cfg = parseConfig(rawCfg)

	// If an engine was already provided by the kernel, skip construction.
	if p.engine != nil {
		if reg != nil {
			reg.Logger().Info("engine_xray: using pre-constructed domain engine")
		}
		return nil
	}

	p.adapter = NewAdapter(
		p.cfg.GRPCAddr,
		p.cfg.ConfigPath,
		p.cfg.TemplatePath,
		p.cfg.RealityRotation,
		p.cfg.RealityKeysPath,
		p.cfg.BlacklistedAdmins,
		slog.Default(),
	)
	p.engine = p.adapter

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
	if p.engine == nil {
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
	if p.engine == nil {
		return nil
	}
	return map[string]any{
		"engine.softban":                            p,
		"engine.logger_control":                     p,
		pluginapi.ServiceClientConfigContributor:    pluginapi.ClientConfigContributor(p),
		pluginapi.ServiceSubscriptionConfigProvider: pluginapi.SubscriptionConfigProvider(p),
	}
}

func (p *Plugin) AddUser(ctx context.Context, u pluginapi.VPNUserConfig) error {
	return p.engine.AddUser(ctx, toDomain(u))
}

func (p *Plugin) AddUsersBulk(ctx context.Context, users []pluginapi.VPNUserConfig) error {
	return p.engine.AddUsersBulk(ctx, toDomainSlice(users))
}

func (p *Plugin) RemoveUser(ctx context.Context, email string) error {
	return p.engine.RemoveUser(ctx, email)
}

func (p *Plugin) RemoveUsersBulk(ctx context.Context, emails []string) error {
	return p.engine.RemoveUsersBulk(ctx, emails)
}

func (p *Plugin) SetExpire(ctx context.Context, email string, expire string) error {
	return p.engine.SetExpire(ctx, email, expire)
}

func (p *Plugin) SetLimit(ctx context.Context, email string, limit float64) error {
	return p.engine.SetLimit(ctx, email, limit)
}

func (p *Plugin) RebuildInbound(ctx context.Context, tag string) error {
	return p.engine.RebuildInbound(ctx, tag)
}

func (p *Plugin) QueryStats(ctx context.Context) ([]pluginapi.TrafficStat, error) {
	stats, err := p.engine.QueryStats(ctx)
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
	return p.engine.BanUser(ctx, email)
}

func (p *Plugin) UnbanUser(ctx context.Context, email string) error {
	return p.engine.UnbanUser(ctx, email)
}

func (p *Plugin) RestartLogger(ctx context.Context) error {
	return p.engine.RestartLogger(ctx)
}

func (p *Plugin) ListUsers(ctx context.Context) ([]pluginapi.VPNUserConfig, error) {
	users, err := p.engine.ListUsers(ctx)
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
	result, err := p.engine.SyncUsers(ctx, toDomainSlice(dbUsers), removeOrphans)
	if err != nil {
		return nil, err
	}
	return &pluginapi.EngineSyncResult{Added: result.Added, Removed: result.Removed}, nil
}

// ── pluginapi.ClientConfigContributor ────────────────────────────────────────

// ── Adapter accessor (Phase 1.5 → Phase 3 bridge) ────────────────────────────

// Adapter returns the underlying Adapter.
// Used by server_kernel.go to pass the engine to code that still accepts
// domain.Engine directly (workers and router).
// Removed in Phase 3 when all callers use EngineProvider via ServiceRegistry.
func (p *Plugin) Adapter() *Adapter { return p.adapter }

// DomainEngine returns the adapter as domain.Engine.
// Convenience method so the kernel doesn't need to cast.
func (p *Plugin) DomainEngine() domain.Engine { return p.engine }

// ── type conversion helpers ───────────────────────────────────────────────────

func toDomain(u pluginapi.VPNUserConfig) domain.VPNUserConfig {
	return domain.VPNUserConfig{
		Email:                 u.Email,
		UUID:                  u.UUID,
		Auth:                  u.Auth,
		Subfile:               u.Subfile,
		Expire:                u.Expire,
		MaxDevices:            u.MaxDevices,
		Flow:                  u.Flow,
		Cipher:                u.Cipher,
		PlanEngineIDs:         append([]string(nil), u.PlanEngineIDs...),
		SubscriptionEngineIDs: append([]string(nil), u.SubscriptionEngineIDs...),
	}
}

func fromDomain(u domain.VPNUserConfig) pluginapi.VPNUserConfig {
	return pluginapi.VPNUserConfig{
		Email:                 u.Email,
		UUID:                  u.UUID,
		Auth:                  u.Auth,
		Subfile:               u.Subfile,
		Expire:                u.Expire,
		MaxDevices:            u.MaxDevices,
		Flow:                  u.Flow,
		Cipher:                u.Cipher,
		PlanEngineIDs:         append([]string(nil), u.PlanEngineIDs...),
		SubscriptionEngineIDs: append([]string(nil), u.SubscriptionEngineIDs...),
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
	for _, key := range []string{"server_address", "server", "host", "address"} {
		if v, ok := raw[key].(string); ok && v != "" {
			cfg.ServerAddress = v
			break
		}
	}
	if v, ok := raw["hy2_config_yaml"].(string); ok {
		cfg.Hy2ConfigYAML = v
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
var _ pluginapi.ClientConfigContributor = (*Plugin)(nil)
var _ pluginapi.ServiceProvider = (*Plugin)(nil)
