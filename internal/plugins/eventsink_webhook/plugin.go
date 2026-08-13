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
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	json "github.com/goccy/go-json"

	"xraytool/internal/generate"
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
	log     *slog.Logger
	cfg     pluginConfig
	client  *http.Client
	wg      sync.WaitGroup
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
	p.cfg = cfg
	p.client = &http.Client{
		Timeout: 10 * time.Second,
	}

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
	done := make(chan struct{})
	go func() {
		p.wg.Wait()
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
	if p.client == nil {
		return fmt.Errorf("eventsink_webhook: not initialised")
	}
	return nil
}

// ── pluginapi.EventSink ───────────────────────────────────────────────────────

func (p *Plugin) Handle(ctx context.Context, ev pluginapi.Event) error {
	if p.client == nil {
		return fmt.Errorf("eventsink_webhook: not initialised")
	}
	if len(p.cfg.Webhooks) == 0 {
		return nil
	}

	eventID := "evt_" + generate.Secret(16)
	
	payloadObj := map[string]any{
		"event_id":      eventID,
		"event_type":    ev.Type,
		"timestamp":     time.Now().UTC().Format(time.RFC3339),
		"data":          ev.Data,
		"user_metadata": ev.UserMeta,
	}

	payload, err := json.Marshal(payloadObj)
	if err != nil {
		p.log.Error("error marshalling event", "event_id", eventID, "error", err)
		return err
	}

	for _, webhookURL := range p.cfg.Webhooks {
		p.wg.Add(1)
		go func(url string) {
			defer p.wg.Done()
			p.sendWithRetry(ctx, url, eventID, payload)
		}(webhookURL)
	}

	return nil
}

func (p *Plugin) sendWithRetry(ctx context.Context, url, eventID string, payload []byte) {
	backoffs := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second}
	maxAttempts := len(backoffs) + 1

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
		if err != nil {
			p.log.Error("error creating request", "event_id", eventID, "url", url, "error", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		if p.cfg.WebhookSecret != "" {
			mac := hmac.New(sha256.New, []byte(p.cfg.WebhookSecret))
			mac.Write(payload)
			req.Header.Set("X-Webhook-Signature", hex.EncodeToString(mac.Sum(nil)))
		}

		resp, err := p.client.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				p.log.Info("event successfully sent", "event_id", eventID, "url", url, "attempt", attempt)
				return
			}
			p.log.Warn("webhook returned non-200 status", "url", url, "status", resp.StatusCode, "event_id", eventID, "attempt", attempt)
		} else {
			p.log.Warn("error sending event", "event_id", eventID, "url", url, "attempt", attempt, "error", err)
		}

		if attempt < maxAttempts {
			sleepTime := backoffs[attempt-1]
			p.log.Info("retrying event", "event_id", eventID, "url", url, "sleep_time", sleepTime)
			select {
			case <-time.After(sleepTime):
			case <-ctx.Done():
				return
			}
		}
	}

	p.log.Error("failed to send event after max attempts", "event_id", eventID, "url", url, "max_attempts", maxAttempts)
}

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
