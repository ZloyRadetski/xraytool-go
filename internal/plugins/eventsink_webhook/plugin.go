// Package eventsink_webhook implements the EventSink plugin that delivers
// domain events to configured HTTP webhook endpoints.
//
// Phase 1.1: this is a mechanical wrap of the existing internal/events package.
// The HTTP delivery logic (retries, HMAC signing, backoff) stays in
// events.Dispatcher — we only add Plugin lifecycle glue here.
//
// Metadata:
//
//	Name: "eventsink_webhook"
//	Kind: "event_sink"
//
// Published service names: none (EventSink plugins are consumers, not publishers).
// Required services: none (reads config section from RawConfig).
package eventsink_webhook

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"xraytool/internal/events"
	"xraytool/internal/pluginapi"
)

// pluginConfig is the typed config for this plugin's YAML section.
//
//	plugins:
//	  eventsink_webhook:
//	    enabled: true
//	    config:
//	      webhooks:
//	        - "https://example.com/webhook"
//	      webhook_secret: "s3cr3t"
type pluginConfig struct {
	Webhooks      []string
	WebhookSecret string
}

// Plugin wraps events.Dispatcher as a pluginapi.EventSink.
type Plugin struct {
	log        *slog.Logger
	dispatcher *events.Dispatcher
}

// New creates an uninitialised plugin.
func New() *Plugin { return &Plugin{} }

// ── pluginapi.Plugin ──────────────────────────────────────────────────────────

func (p *Plugin) Metadata() pluginapi.Metadata {
	return pluginapi.Metadata{
		Name:        "eventsink_webhook",
		Kind:        "event_sink",
		Version:     "1.0.0",
		APIVersion:  pluginapi.CurrentAPIVersion,
		Description: "HTTP webhook delivery for domain events with HMAC signing and retry.",
		Mandatory:   false,
		Publishes:   nil,
		Requires:    nil,
	}
}

func (p *Plugin) Init(_ context.Context, rawCfg pluginapi.RawConfig, reg pluginapi.ServiceResolver) error {
	p.log = slog.Default().With("plugin", "eventsink_webhook")

	cfg := parseConfig(rawCfg)

	p.dispatcher = events.NewDispatcher(&events.Config{
		Webhooks:      cfg.Webhooks,
		WebhookSecret: cfg.WebhookSecret,
	})

	if reg != nil {
		reg.Logger().Info("eventsink_webhook: initialised", "webhooks", len(cfg.Webhooks))
	}
	return nil
}

func (p *Plugin) Start(ctx context.Context) error {
	// The Dispatcher itself spawns goroutines on each Dispatch() call.
	// Nothing to start here — just wait for shutdown.
	<-ctx.Done()
	return nil
}

func (p *Plugin) Stop(ctx context.Context) error {
	if p.dispatcher == nil {
		return nil
	}
	// Drain all in-flight deliveries within the shutdown deadline.
	done := make(chan struct{})
	go func() {
		p.dispatcher.Shutdown()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("eventsink_webhook: shutdown timed out — some webhooks may not have been delivered")
	}
}

func (p *Plugin) Health(_ context.Context) error {
	if p.dispatcher == nil {
		return fmt.Errorf("eventsink_webhook: not initialised")
	}
	return nil
}

// ── pluginapi.EventSink ───────────────────────────────────────────────────────

// Handle delivers a pluginapi.Event to the configured webhooks.
// Maps pluginapi.Event fields to the events.Dispatcher format.
func (p *Plugin) Handle(_ context.Context, ev pluginapi.Event) error {
	if p.dispatcher == nil {
		return fmt.Errorf("eventsink_webhook: not initialised")
	}
	p.dispatcher.Dispatch(ev.Type, ev.Data, ev.UserMeta)
	return nil
}

// Dispatcher returns the underlying events.Dispatcher.
// Used by the kernel to wire it into existing services that still call
// Dispatcher.Dispatch() directly (server.Router, payment.Service).
// Will be removed in Phase 6 when all callers use EmitEvent() via ServiceResolver.
func (p *Plugin) Dispatcher() *events.Dispatcher { return p.dispatcher }

// ── helpers ───────────────────────────────────────────────────────────────────

func parseConfig(raw pluginapi.RawConfig) pluginConfig {
	var cfg pluginConfig
	if raw == nil {
		return cfg
	}
	if v, ok := raw["webhook_secret"].(string); ok {
		cfg.WebhookSecret = v
	}
	if v, ok := raw["webhooks"].([]interface{}); ok {
		for _, item := range v {
			if s, ok := item.(string); ok {
				cfg.Webhooks = append(cfg.Webhooks, s)
			}
		}
	}
	return cfg
}

// Compile-time interface checks.
var _ pluginapi.Plugin    = (*Plugin)(nil)
var _ pluginapi.EventSink = (*Plugin)(nil)

// Ensure time is used (for context deadline in Stop).
var _ = time.Second
