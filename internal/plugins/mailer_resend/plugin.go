// Package mailer_resend implements the NotificationProvider plugin for email
// delivery via the Resend.com API.
//
// Phase 1.1: this is a mechanical wrap of the existing internal/mailer package.
// The business logic (HTML template, API call) is NOT duplicated here — we
// import and delegate to the existing mailer.ResendMailer directly.
//
// Metadata:
//
//	Name:     "mailer_resend"
//	Kind:     "notification"
//	Channels: ["email"]
//
// Published service names:
//
//	"notification_provider"
//
// Required services: none (reads its own config section from RawConfig).
package mailer_resend

import (
	"context"
	"fmt"
	"log/slog"

	"xraytool/internal/pluginapi"
)

// pluginConfig is the typed representation of the plugin's YAML config section.
//
//	plugins:
//	  mailer_resend:
//	    enabled: true
//	    config:
//	      resend_api_key: "re_..."
//	      from_email: "noreply@example.com"
type pluginConfig struct {
	ResendAPIKey string
	FromEmail    string
}

// Plugin wraps mailer.ResendMailer as a pluginapi.NotificationProvider.
type Plugin struct {
	log    *slog.Logger
	mailer *ResendMailer
}

// New creates an uninitialised plugin. Call via BuiltinRegistry factory.
func New() *Plugin { return &Plugin{} }

// ── pluginapi.Plugin ──────────────────────────────────────────────────────────

func (p *Plugin) Metadata() pluginapi.Metadata {
	return pluginapi.Metadata{
		Name:        "mailer_resend",
		Kind:        "notification",
		Version:     "1.0.0",
		APIVersion:  pluginapi.CurrentAPIVersion,
		Description: "Email notification delivery via the Resend.com API.",
		Mandatory:   false,
		Publishes:   []pluginapi.ServiceRef{{Name: "notification_provider"}},
		Requires:    nil,
	}
}

func (p *Plugin) Init(_ context.Context, rawCfg pluginapi.RawConfig, reg pluginapi.ServiceResolver) error {
	p.log = slog.Default().With("plugin", "mailer_resend")

	cfg, err := parseConfig(rawCfg)
	if err != nil {
		return fmt.Errorf("mailer_resend: config error: %w", err)
	}
	if cfg.ResendAPIKey == "" {
		return fmt.Errorf("mailer_resend: resend_api_key is required in plugin config")
	}
	if cfg.FromEmail == "" {
		return fmt.Errorf("mailer_resend: from_email is required in plugin config")
	}

	p.mailer = NewResendMailer(cfg.ResendAPIKey, cfg.FromEmail)

	if reg != nil {
		reg.Logger().Info("mailer_resend: initialised", "from", cfg.FromEmail)
	}
	return nil
}

func (p *Plugin) Start(ctx context.Context) error {
	// No background goroutines needed for a stateless HTTP client.
	<-ctx.Done()
	return nil
}

func (p *Plugin) Stop(_ context.Context) error {
	return nil
}

func (p *Plugin) Health(_ context.Context) error {
	if p.mailer == nil {
		return fmt.Errorf("mailer_resend: not initialised")
	}
	return nil
}

// ── pluginapi.NotificationProvider ───────────────────────────────────────────

func (p *Plugin) Channels() []string { return []string{"email"} }

// PublishedServices exposes this plugin as the notification provider selected
// by the Host. The router is migrated to resolve this service in Phase 6.
func (p *Plugin) PublishedServices() map[string]any {
	if p.mailer == nil {
		return nil
	}
	return map[string]any{"notification_provider": p}
}

// Send delivers a notification. Currently only the "otp_code" kind is supported.
// Future kinds (subscription_expiring, payment_received) can be added without
// changing the plugin contract.
func (p *Plugin) Send(_ context.Context, n pluginapi.Notification) error {
	if p.mailer == nil {
		return fmt.Errorf("mailer_resend: plugin not initialised")
	}
	if n.Channel != "email" {
		return fmt.Errorf("mailer_resend: unsupported channel %q", n.Channel)
	}
	switch n.Kind {
	case "otp_code":
		code, _ := n.Payload["code"].(string)
		if code == "" {
			return fmt.Errorf("mailer_resend: otp_code notification missing 'code' field")
		}
		return p.mailer.SendCode(n.To, code)
	default:
		return fmt.Errorf("mailer_resend: unsupported notification kind %q", n.Kind)
	}
}

// SendCode is a convenience bridge for handlers that still call mailer directly
// (via server.Router.mailer). Will be removed in Phase 6 when router resolves
// "notification_provider" from the ServiceRegistry instead.
func (p *Plugin) SendCode(to, code string) error {
	if p.mailer == nil {
		return fmt.Errorf("mailer_resend: plugin not initialised")
	}
	return p.mailer.SendCode(to, code)
}

// ── helpers ───────────────────────────────────────────────────────────────────

func parseConfig(raw pluginapi.RawConfig) (pluginConfig, error) {
	var cfg pluginConfig
	if raw == nil {
		return cfg, nil
	}
	if v, ok := raw["resend_api_key"].(string); ok {
		cfg.ResendAPIKey = v
	}
	if v, ok := raw["from_email"].(string); ok {
		cfg.FromEmail = v
	}
	return cfg, nil
}

// Compile-time interface checks.
var _ pluginapi.Plugin = (*Plugin)(nil)
var _ pluginapi.NotificationProvider = (*Plugin)(nil)
var _ pluginapi.ServiceProvider = (*Plugin)(nil)
