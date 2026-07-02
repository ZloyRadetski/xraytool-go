package events

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"xraytool/internal/generate"
	"xraytool/internal/logger"
)

// Event represents a structured webhook event.
type Event struct {
	EventID      string                 `json:"event_id"`
	EventType    string                 `json:"event_type"`
	Timestamp    string                 `json:"timestamp"`
	Data         map[string]interface{} `json:"data"`
	UserMetadata map[string]interface{} `json:"user_metadata,omitempty"`
}

// Dispatcher manages sending events to registered webhooks.
//
// Background goroutines spawned by Dispatch are tracked in wg so that
// Shutdown can block until all in-flight webhook deliveries complete.
// This prevents silent data loss on graceful server shutdown (SIGTERM).
type Dispatcher struct {
	webhooks []string
	client   *http.Client
	secret   string
	wg       sync.WaitGroup
}

// Config holds settings for the event dispatcher.
type Config struct {
	Webhooks      []string
	WebhookSecret string
}

// NewDispatcher creates a new event dispatcher with the given config.
func NewDispatcher(cfg *Config) *Dispatcher {
	if cfg == nil {
		return &Dispatcher{
			webhooks: []string{},
			client: &http.Client{
				Timeout: 10 * time.Second,
			},
		}
	}
	return &Dispatcher{
		webhooks: cfg.Webhooks,
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
		secret: cfg.WebhookSecret,
	}
}

// Dispatch sends an event with the given type, data, and metadata to all
// registered webhooks asynchronously. The goroutine is tracked in the
// internal WaitGroup so Shutdown() can drain all in-flight deliveries.
func (d *Dispatcher) Dispatch(eventType string, data map[string]interface{}, userMetadata map[string]interface{}) {
	if len(d.webhooks) == 0 {
		return
	}

	event := Event{
		EventID:      "evt_" + generate.Secret(16),
		EventType:    eventType,
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
		Data:         data,
		UserMetadata: userMetadata,
	}

	logger.Infof("[EVENT_DISPATCHER] Dispatching event %s (type: %s) to %d webhooks asynchronously", event.EventID, event.EventType, len(d.webhooks))

	d.broadcast(event)
}

// DispatchSync sends an event synchronously, blocking until all webhooks are sent (or fail).
// This is critical for short-lived CLI commands like `xraytool sub`.
func (d *Dispatcher) DispatchSync(eventType string, data map[string]interface{}, userMetadata map[string]interface{}) {
	if len(d.webhooks) == 0 {
		return
	}

	event := Event{
		EventID:      "evt_" + generate.Secret(16),
		EventType:    eventType,
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
		Data:         data,
		UserMetadata: userMetadata,
	}

	logger.Infof("[EVENT_DISPATCHER] Dispatching event %s (type: %s) to %d webhooks synchronously", event.EventID, event.EventType, len(d.webhooks))
	payload, err := json.Marshal(event)
	if err != nil {
		logger.Errorf("[EVENT_DISPATCHER] Error marshalling event %s: %v", event.EventID, err)
		return
	}

	var wg sync.WaitGroup
	for _, webhookURL := range d.webhooks {
		wg.Add(1)
		go func(url string) {
			defer wg.Done()
			d.sendWithRetry(context.Background(), url, event.EventID, payload)
		}(webhookURL)
	}
	wg.Wait()
}

// Shutdown waits for all in-flight background webhook deliveries to complete.
// Call this during graceful server shutdown before exiting the process.
func (d *Dispatcher) Shutdown() {
	d.wg.Wait()
}

func (d *Dispatcher) broadcast(event Event) {
	payload, err := json.Marshal(event)
	if err != nil {
		logger.Errorf("[EVENT_DISPATCHER] Error marshalling event %s: %v", event.EventID, err)
		return
	}

	for _, webhookURL := range d.webhooks {
		d.wg.Add(1)
		go func(url string) {
			defer d.wg.Done()
			d.sendWithRetry(context.Background(), url, event.EventID, payload)
		}(webhookURL)
	}
}

func (d *Dispatcher) sendWithRetry(ctx context.Context, url, eventID string, payload []byte) {
	backoffs := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second}
	maxAttempts := len(backoffs) + 1

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		req, err := http.NewRequest("POST", url, bytes.NewReader(payload))
		if err != nil {
			logger.Errorf("[EVENT_DISPATCHER] Error creating request for event %s to %s: %v", eventID, url, err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		if d.secret != "" {
			mac := hmac.New(sha256.New, []byte(d.secret))
			mac.Write(payload)
			req.Header.Set("X-Webhook-Signature", hex.EncodeToString(mac.Sum(nil)))
		}

		resp, err := d.client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				logger.Infof("[EVENT_DISPATCHER] Event %s successfully sent to %s (attempt %d/%d)", eventID, url, attempt, maxAttempts)
				return
			}
			logger.Warnf("[EVENT_DISPATCHER] Webhook %s returned status %d for event %s (attempt %d/%d)", url, resp.StatusCode, eventID, attempt, maxAttempts)
		} else {
			logger.Warnf("[EVENT_DISPATCHER] Error sending event %s to %s (attempt %d/%d): %v", eventID, url, attempt, maxAttempts, err)
		}

		if attempt < maxAttempts {
			sleepTime := backoffs[attempt-1]
			logger.Infof("[EVENT_DISPATCHER] Retrying event %s to %s in %v...", eventID, url, sleepTime)
			select {
			case <-time.After(sleepTime):
			case <-ctx.Done():
				return
			}
		}
	}

	logger.Errorf("[EVENT_DISPATCHER] Failed to send event %s to %s after %d attempts", eventID, url, maxAttempts)
}
