package events

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
	"xraytool/internal/appconfig"
	"xraytool/internal/logger"
)

func init() {
	// Initialize a logger to discard or prevent nil pointer if accessed
	cfg := &appconfig.Config{
		Logging: appconfig.LoggingConf{Level: "error", Format: "console"},
	}
	logger.Init(cfg)
}

func TestDispatcherSync(t *testing.T) {
	var requestCount int32

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requestCount, 1)

		body, _ := io.ReadAll(r.Body)
		var event Event
		if err := json.Unmarshal(body, &event); err != nil {
			t.Errorf("Failed to parse event: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		if event.EventType != "test_event" {
			t.Errorf("Expected event_type test_event, got %s", event.EventType)
		}

		if val, ok := event.Data["key"]; !ok || val != "value" {
			t.Errorf("Expected data[key]=value, got %v", event.Data)
		}

		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	cfg := &appconfig.Config{
		Webhooks: []string{ts.URL},
	}

	dispatcher := NewDispatcher(cfg)

	data := map[string]interface{}{"key": "value"}
	dispatcher.DispatchSync("test_event", data, nil)

	if atomic.LoadInt32(&requestCount) != 1 {
		t.Errorf("Expected 1 request, got %d", requestCount)
	}
}

func TestDispatcherRetry(t *testing.T) {
	var requestCount int32

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := atomic.AddInt32(&requestCount, 1)
		if count == 1 {
			// Fail first time
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		// Succeed second time
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	cfg := &appconfig.Config{
		Webhooks: []string{ts.URL},
	}

	dispatcher := NewDispatcher(cfg)

	// Override client to have short timeout and avoid long test delays
	// Note: We can't easily override backoff slice in the sendWithRetry without refactoring,
	// but we can test it anyway (it will just take 2 seconds).
	dispatcher.client.Timeout = 1 * time.Second

	data := map[string]interface{}{"retry": true}
	dispatcher.DispatchSync("retry_event", data, nil)

	if atomic.LoadInt32(&requestCount) != 2 {
		t.Errorf("Expected 2 requests due to retry, got %d", requestCount)
	}
}
