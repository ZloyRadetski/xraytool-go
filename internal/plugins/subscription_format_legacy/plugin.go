// Package subscription_format_legacy renders the formats that historically
// lived in the core subscription handler. Keeping this implementation as a
// plugin lets new format providers (for example sing-box or client portals)
// replace it without taking ownership of subscription transactions.
package subscription_format_legacy

import (
	"context"
	"strings"

	"xraytool/internal/pluginapi"
	"xraytool/internal/plugins/subscription_format_legacy/convert"
)

type Plugin struct{}

func New() *Plugin { return &Plugin{} }

func (p *Plugin) Metadata() pluginapi.Metadata {
	return pluginapi.Metadata{
		Name:        "subscription_format_legacy",
		Kind:        "subscription_format",
		Version:     "1.0.0",
		APIVersion:  pluginapi.CurrentAPIVersion,
		Description: "Legacy VLESS text and Clash YAML subscription renderer.",
		Publishes: []pluginapi.ServiceRef{
			{Name: pluginapi.ServiceSubscriptionFormatProvider},
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
	return map[string]any{pluginapi.ServiceSubscriptionFormatProvider: pluginapi.SubscriptionFormatProvider(p)}
}

func (p *Plugin) RenderSubscription(_ context.Context, request pluginapi.SubscriptionFormatRequest) (pluginapi.SubscriptionFormatResult, error) {
	shareText := linksText(request.Links)
	switch request.Format {
	case "vless":
		if shareText == "" {
			var err error
			shareText, err = convert.XrayJSONToShareText(request.JSONConfig)
			if err != nil {
				return pluginapi.SubscriptionFormatResult{}, err
			}
		}
		return pluginapi.SubscriptionFormatResult{
			Handled:            true,
			Body:               shareText,
			ContentDisposition: `attachment; filename="configs.txt"`,
			ContentType:        "text/plain; charset=utf-8",
		}, nil
	case "clash":
		if shareText != "" {
			clashYAML, err := convert.ShareTextToClashYAML(shareText)
			if err == nil {
				return clashResult(clashYAML), nil
			}
		}
		clashYAML, err := convert.XrayJSONToClashYAML(request.JSONConfig)
		if err != nil {
			return pluginapi.SubscriptionFormatResult{}, err
		}
		return clashResult(clashYAML), nil
	default:
		return pluginapi.SubscriptionFormatResult{}, nil
	}
}

func clashResult(body string) pluginapi.SubscriptionFormatResult {
	return pluginapi.SubscriptionFormatResult{
		Handled:            true,
		Body:               body,
		ContentDisposition: `attachment; filename="config.yaml"`,
		ContentType:        "text/yaml; charset=utf-8",
	}
}

func linksText(links []pluginapi.ClientLink) string {
	values := make([]string, 0, len(links))
	for _, link := range links {
		if uri := strings.TrimSpace(link.URI); uri != "" {
			values = append(values, uri)
		}
	}
	return strings.Join(values, "\n")
}

var _ pluginapi.Plugin = (*Plugin)(nil)
var _ pluginapi.ServiceProvider = (*Plugin)(nil)
var _ pluginapi.SubscriptionFormatProvider = (*Plugin)(nil)
