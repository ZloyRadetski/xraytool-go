// Package traffic_file provides the backwards-compatible file-backed traffic
// accounting implementation. Deployments can replace it with a database or
// analytics-backed plugin without changing subscription delivery.
package traffic_file

import (
	"context"
	"fmt"
	json "github.com/goccy/go-json"

	"xraytool/internal/appconfig"
	"xraytool/internal/domain"
	"xraytool/internal/pluginapi"
)

const (
	ServiceTrafficProvider  = pluginapi.ServiceTrafficProvider
	ServiceQuotaProvider    = pluginapi.ServiceTrafficQuotaProvider
	ServiceSnapshotProvider = pluginapi.ServiceTrafficSnapshotProvider
)

type Plugin struct {
	cfg    *appconfig.Config
	engine domain.Engine
}

func New(cfg *appconfig.Config) *Plugin { return &Plugin{cfg: cfg} }

func (p *Plugin) Metadata() pluginapi.Metadata {
	return pluginapi.Metadata{
		Name:        "traffic_file",
		Kind:        "traffic",
		Version:     "1.0.0",
		APIVersion:  pluginapi.CurrentAPIVersion,
		Description: "File-backed cumulative traffic accounting with a no-op quota policy.",
		Requires: []pluginapi.ServiceRef{
			{Name: "domain_engine", Optional: true},
		},
		Publishes: []pluginapi.ServiceRef{
			{Name: ServiceTrafficProvider},
			{Name: ServiceQuotaProvider},
			{Name: ServiceSnapshotProvider},
		},
	}
}

func (p *Plugin) Init(_ context.Context, _ pluginapi.RawConfig, resolver pluginapi.ServiceResolver) error {
	// Traffic reads remain available even when an engine is not loaded (for
	// example in a standalone CLI). The live snapshot capability then returns
	// a clear error instead of coupling this plugin to a concrete engine.
	if resolver != nil {
		if value, err := resolver.Resolve("domain_engine"); err == nil {
			if engine, ok := value.(domain.Engine); ok {
				p.engine = engine
			}
		}
	}
	return nil
}
func (p *Plugin) Start(ctx context.Context) error { <-ctx.Done(); return nil }
func (p *Plugin) Stop(_ context.Context) error    { return nil }
func (p *Plugin) Health(_ context.Context) error  { return nil }

func (p *Plugin) PublishedServices() map[string]any {
	return map[string]any{
		ServiceTrafficProvider:  pluginapi.TrafficProvider(p),
		ServiceQuotaProvider:    pluginapi.TrafficQuotaProvider(p),
		ServiceSnapshotProvider: pluginapi.TrafficSnapshotProvider(p),
	}
}

// Usage checks inferred cluster totals first, then local statistics, preserving
// the precedence of the previous core implementation.
func (p *Plugin) Usage(_ context.Context, email string) (pluginapi.TrafficUsage, bool, error) {
	if p.cfg == nil {
		return pluginapi.TrafficUsage{}, false, nil
	}
	paths := []string{p.cfg.Paths.InferredStats, p.cfg.Paths.StatsState}
	for _, path := range paths {
		if path == "" {
			continue
		}
		state, err := Load(path, p.cfg.DetailedRetentionSeconds())
		if err != nil {
			return pluginapi.TrafficUsage{}, false, err
		}
		if user, ok := state.Users[email]; ok && user != nil {
			return pluginapi.TrafficUsage{
				UploadBytes: user.CumulativeUp, DownloadBytes: user.CumulativeDown,
			}, true, nil
		}
	}
	return pluginapi.TrafficUsage{}, false, nil
}

// GenerateClusterStats owns the legacy file-backed statistics workflow. The
// CLI delegates here instead of constructing backend internals itself, so
// another traffic plugin can replace the behaviour without changing command semantics.
func (p *Plugin) GenerateClusterStats(inferredMode bool, statePath string, engine domain.Engine, cluster domain.ClusterStatsProvider) ([]MergedUser, domain.SlaveReport, error) {
	if p == nil || p.cfg == nil {
		return nil, domain.SlaveReport{}, fmt.Errorf("traffic_file: app config is required")
	}
	service := NewService(Config{
		IsMaster:                 p.cfg.IsMaster(),
		StatsStatePath:           p.cfg.Paths.StatsState,
		InferredStatsPath:        p.cfg.Paths.InferredStats,
		DetailedRetentionSeconds: p.cfg.DetailedRetentionSeconds(),
	}, engine, cluster)
	return service.GenerateClusterStats(inferredMode, statePath)
}

// LocalTrafficSnapshot is the only cluster-facing view of this backend. It
// performs the same locked cumulative update as the CLI, so every consumer
// observes counter-reset-safe totals.
func (p *Plugin) LocalTrafficSnapshot(_ context.Context) ([]pluginapi.TrafficSnapshot, error) {
	if p == nil || p.cfg == nil {
		return nil, fmt.Errorf("traffic_file: app config is required")
	}
	if p.engine == nil {
		return nil, fmt.Errorf("traffic_file: domain_engine is unavailable")
	}
	users, err := NewService(Config{
		StatsStatePath:           p.cfg.Paths.StatsState,
		DetailedRetentionSeconds: p.cfg.DetailedRetentionSeconds(),
	}, p.engine, nil).GenerateLocalStats()
	if err != nil {
		return nil, err
	}
	snapshot := make([]pluginapi.TrafficSnapshot, 0, len(users))
	for _, user := range users {
		snapshot = append(snapshot, pluginapi.TrafficSnapshot{Email: user.Email, Usage: pluginapi.TrafficUsage{UploadBytes: user.Total.Up, DownloadBytes: user.Total.Down}})
	}
	return snapshot, nil
}

// CheckQuota keeps historical behaviour: traffic was reported but never used
// to block a subscription. Alternative quota plugins can publish the same
// service and enforce an installation-specific policy.
func (p *Plugin) CheckQuota(_ context.Context, _ pluginapi.Subscription, _ pluginapi.TrafficUsage) (pluginapi.TrafficQuotaDecision, error) {
	return pluginapi.TrafficQuotaDecision{}, nil
}

func (p *Plugin) CLICommands() []pluginapi.CLICommand {
	return []pluginapi.CLICommand{{
		Name: "usage", Use: "usage <email>", Short: "Show cumulative traffic for one user.",
	}}
}

func (p *Plugin) RunCLI(ctx context.Context, name string, args []string) (string, error) {
	if name != "usage" {
		return "", fmt.Errorf("traffic_file: unknown command %q", name)
	}
	if len(args) != 1 || args[0] == "" {
		return "", fmt.Errorf("traffic_file usage: usage <email>")
	}
	usage, found, err := p.Usage(ctx, args[0])
	if err != nil {
		return "", err
	}
	result, err := json.Marshal(map[string]any{
		"email": args[0], "found": found, "upload": usage.UploadBytes, "download": usage.DownloadBytes,
	})
	return string(result), err
}

var _ pluginapi.Plugin = (*Plugin)(nil)
var _ pluginapi.ServiceProvider = (*Plugin)(nil)
var _ pluginapi.TrafficProvider = (*Plugin)(nil)
var _ pluginapi.TrafficQuotaProvider = (*Plugin)(nil)
var _ pluginapi.TrafficSnapshotProvider = (*Plugin)(nil)
var _ pluginapi.CLIContributor = (*Plugin)(nil)
