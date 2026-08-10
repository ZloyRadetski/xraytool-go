// Package subscription_autobalancer owns the v2 JSON subscription source
// format and its compilation to native Xray balancing profiles.
package subscription_autobalancer

import (
	"context"

	"xraytool/internal/pluginapi"
	"xraytool/internal/plugins/subscription_autobalancer/template"
)

type Plugin struct{}

func New() *Plugin { return &Plugin{} }

func (p *Plugin) Metadata() pluginapi.Metadata {
	return pluginapi.Metadata{
		Name:        "subscription_autobalancer",
		Kind:        "subscription",
		Version:     "1.0.0",
		APIVersion:  pluginapi.CurrentAPIVersion,
		Description: "Compiles v2 subscription templates and native Xray auto-balancers.",
		Publishes: []pluginapi.ServiceRef{
			{Name: pluginapi.ServiceSubscriptionTemplateProcessor},
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
	return map[string]any{pluginapi.ServiceSubscriptionTemplateProcessor: pluginapi.SubscriptionTemplateProcessor(p)}
}

// ProcessSubscriptionTemplate recognises only the v2 source format. The
// complete profile array is delivered to JSON clients, while ExportJSONConfig
// intentionally contains only endpoint profiles for VLESS and Clash renderers.
func (p *Plugin) ProcessSubscriptionTemplate(_ context.Context, jsonConfig string) (pluginapi.SubscriptionTemplateResult, error) {
	compiled, err := template.Compile(jsonConfig)
	if err != nil {
		return pluginapi.SubscriptionTemplateResult{}, err
	}
	if !compiled.IsV2 {
		return pluginapi.SubscriptionTemplateResult{}, nil
	}
	return pluginapi.SubscriptionTemplateResult{
		Handled:          true,
		JSONConfig:       compiled.JSON,
		ExportJSONConfig: compiled.ExportJSON,
		ProfileCount:     compiled.ProfileCount,
		BalancerCount:    compiled.BalancerCount,
	}, nil
}

var _ pluginapi.Plugin = (*Plugin)(nil)
var _ pluginapi.ServiceProvider = (*Plugin)(nil)
var _ pluginapi.SubscriptionTemplateProcessor = (*Plugin)(nil)
