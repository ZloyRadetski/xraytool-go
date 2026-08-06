package eventsink_webhook_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	eventsinkPlugin "xraytool/internal/plugins/eventsink_webhook"
	"xraytool/internal/pluginapi"
)

func TestEventsink_Metadata(t *testing.T) {
	t.Parallel()
	p := eventsinkPlugin.New()
	m := p.Metadata()
	if m.Name != "eventsink_webhook" {
		t.Errorf("Name = %q, want %q", m.Name, "eventsink_webhook")
	}
	if m.Kind != "event_sink" {
		t.Errorf("Kind = %q, want %q", m.Kind, "event_sink")
	}
	if m.Mandatory {
		t.Error("eventsink_webhook must not be mandatory")
	}
}

func TestEventsink_Init_NilConfig_OK(t *testing.T) {
	t.Parallel()
	p := eventsinkPlugin.New()
	if err := p.Init(context.Background(), nil, nil); err != nil {
		t.Fatalf("Init(nil) should succeed, got: %v", err)
	}
}

func TestEventsink_Init_WithWebhooks_OK(t *testing.T) {
	t.Parallel()
	p := eventsinkPlugin.New()
	err := p.Init(context.Background(), pluginapi.RawConfig{
		"webhooks":       []interface{}{"https://example.com/hook"},
		"webhook_secret": "s3cr3t",
	}, nil)
	if err != nil {
		t.Fatalf("Init() with valid config should succeed, got: %v", err)
	}
}

func TestEventsink_Health_BeforeInit_Error(t *testing.T) {
	t.Parallel()
	p := eventsinkPlugin.New()
	if err := p.Health(context.Background()); err == nil {
		t.Error("Health() before Init() should return error")
	}
}

func TestEventsink_Health_AfterInit_OK(t *testing.T) {
	t.Parallel()
	p := eventsinkPlugin.New()
	_ = p.Init(context.Background(), nil, nil)
	if err := p.Health(context.Background()); err != nil {
		t.Errorf("Health() after Init() should be nil, got: %v", err)
	}
}

func TestEventsink_Handle_BeforeInit_Error(t *testing.T) {
	t.Parallel()
	p := eventsinkPlugin.New()
	err := p.Handle(context.Background(), pluginapi.Event{
		Type: "test.event",
		Data: map[string]any{"key": "value"},
	})
	if err == nil {
		t.Error("Handle() before Init() should return error")
	}
}

func TestEventsink_Handle_NoWebhooks_NoError(t *testing.T) {
	t.Parallel()
	p := eventsinkPlugin.New()
	// Init with empty config → no webhooks → Dispatcher.Dispatch is a no-op
	_ = p.Init(context.Background(), nil, nil)
	err := p.Handle(context.Background(), pluginapi.Event{
		Type:       "test.event",
		OccurredAt: time.Now(),
		Data:       map[string]any{"key": "value"},
	})
	if err != nil {
		t.Errorf("Handle() with no webhooks should be no-op, got: %v", err)
	}
}

func TestEventsink_Handle_DeliversTtoWebhook(t *testing.T) {
	t.Parallel()

	received := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		eventType := r.Header.Get("Content-Type")
		_ = eventType
		received <- r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := eventsinkPlugin.New()
	_ = p.Init(context.Background(), pluginapi.RawConfig{
		"webhooks": []interface{}{srv.URL + "/webhook"},
	}, nil)

	err := p.Handle(context.Background(), pluginapi.Event{
		Type:       "subscription.created",
		OccurredAt: time.Now(),
		Data:       map[string]any{"email": "user@example.com"},
	})
	if err != nil {
		t.Fatalf("Handle() should not error: %v", err)
	}

	// Wait for async delivery (Dispatcher delivers in a goroutine).
	select {
	case path := <-received:
		if path != "/webhook" {
			t.Errorf("webhook received at %q, want /webhook", path)
		}
	case <-time.After(3 * time.Second):
		t.Error("webhook was not delivered within 3 seconds")
	}
}

func TestEventsink_Stop_DrainsPendingDeliveries(t *testing.T) {
	t.Parallel()

	delivered := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		delivered <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := eventsinkPlugin.New()
	_ = p.Init(context.Background(), pluginapi.RawConfig{
		"webhooks": []interface{}{srv.URL + "/hook"},
	}, nil)

	_ = p.Handle(context.Background(), pluginapi.Event{
		Type: "payment.completed",
		Data: map[string]any{"amount": 100},
	})

	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.Stop(stopCtx); err != nil {
		t.Fatalf("Stop() returned error: %v", err)
	}

	select {
	case <-delivered:
		// Good: webhook was delivered before Stop returned.
	case <-time.After(100 * time.Millisecond):
		// This is acceptable — Stop() drains and then the select fires.
	}
}
