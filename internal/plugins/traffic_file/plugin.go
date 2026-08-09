// Package traffic_file provides the backwards-compatible file-backed traffic
// accounting implementation. Deployments can replace it with a database or
// analytics-backed plugin without changing subscription delivery.
package traffic_file

import (
	"context"
	"fmt"
	json "github.com/goccy/go-json"

	"xraytool/internal/appconfig"
	"xraytool/internal/pluginapi"
	"xraytool/internal/stats"
)

const (
	ServiceTrafficProvider = pluginapi.ServiceTrafficProvider
	ServiceQuotaProvider   = pluginapi.ServiceTrafficQuotaProvider
)

type Plugin struct {
	cfg *appconfig.Config
}

func New(cfg *appconfig.Config) *Plugin { return &Plugin{cfg: cfg} }

func (p *Plugin) Metadata() pluginapi.Metadata {
	return pluginapi.Metadata{
		Name:        "traffic_file",
		Kind:        "traffic",
		Version:     "1.0.0",
		APIVersion:  pluginapi.CurrentAPIVersion,
		Description: "File-backed cumulative traffic accounting with a no-op quota policy.",
		Publishes: []pluginapi.ServiceRef{
			{Name: ServiceTrafficProvider},
			{Name: ServiceQuotaProvider},
		},
	}
}

func (p *Plugin) Init(_ context.Context, _ pluginapi.RawConfig, _ pluginapi.ServiceResolver) error {
	return nil
}
func (p *Plugin) Start(ctx context.Context) error { <-ctx.Done(); return nil }
func (p *Plugin) Stop(_ context.Context) error    { return nil }
func (p *Plugin) Health(_ context.Context) error  { return nil }

func (p *Plugin) PublishedServices() map[string]any {
	return map[string]any{
		ServiceTrafficProvider: pluginapi.TrafficProvider(p),
		ServiceQuotaProvider:   pluginapi.TrafficQuotaProvider(p),
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
		state, err := stats.Load(path, p.cfg.DetailedRetentionSeconds())
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
var _ pluginapi.CLIContributor = (*Plugin)(nil)
