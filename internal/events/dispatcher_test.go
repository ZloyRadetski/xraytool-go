package events

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
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

func TestDispatcher_Dispatch_Async(t *testing.T) {
	var requestCount int32
	var mu sync.Mutex
	var receivedEvent Event
	var wg sync.WaitGroup
	wg.Add(2)

	handler := func(w http.ResponseWriter, r *http.Request) {
		defer wg.Done()
		atomic.AddInt32(&requestCount, 1)

		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("Expected Content-Type application/json, got %s", r.Header.Get("Content-Type"))
		}

		body, _ := io.ReadAll(r.Body)
		var evt Event
		if err := json.Unmarshal(body, &evt); err != nil {
			t.Errorf("Failed to parse event: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		receivedEvent = evt
		mu.Unlock()

		w.WriteHeader(http.StatusOK)
	}

	ts1 := httptest.NewServer(http.HandlerFunc(handler))
	defer ts1.Close()

	ts2 := httptest.NewServer(http.HandlerFunc(handler))
	defer ts2.Close()

	cfg := &appconfig.Config{Webhooks: []string{ts1.URL, ts2.URL}}
	d := NewDispatcher(cfg)

	data := map[string]interface{}{"info": "test_data"}
	metadata := map[string]interface{}{"user_id": "123"}

	// async publish
	d.Dispatch("async.event", data, metadata)

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("Timeout waiting for webhooks to be called")
	}

	if atomic.LoadInt32(&requestCount) != 2 {
		t.Errorf("Expected 2 requests, got %d", requestCount)
	}

	if receivedEvent.EventType != "async.event" {
		t.Errorf("Expected EventType 'async.event', got '%s'", receivedEvent.EventType)
	}
	if receivedEvent.EventID == "" {
		t.Errorf("Expected non-empty EventID")
	}
	if receivedEvent.Timestamp == "" {
		t.Errorf("Expected non-empty Timestamp")
	}
	if val, ok := receivedEvent.Data["info"]; !ok || val != "test_data" {
		t.Errorf("Expected data['info']='test_data', got %v", receivedEvent.Data)
	}
	if val, ok := receivedEvent.UserMetadata["user_id"]; !ok || val != "123" {
		t.Errorf("Expected UserMetadata['user_id']='123', got %v", receivedEvent.UserMetadata)
	}
}

func TestDispatcher_NilConfig(t *testing.T) {
	d := NewDispatcher(nil)
	d.Dispatch("test", nil, nil)
	d.DispatchSync("test", nil, nil)
	// Should not panic
}

func TestDispatcher_DeadWebhook_Fallback(t *testing.T) {
	// Provide a URL that will definitely fail to connect immediately
	cfg := &appconfig.Config{Webhooks: []string{"http://127.0.0.1:0"}}
	d := NewDispatcher(cfg)
	// Speed up the backoff for test
	d.client.Timeout = 1 * time.Millisecond
	d.Dispatch("dead.event", nil, nil)
	time.Sleep(10 * time.Millisecond)
	// No panic = pass
}
