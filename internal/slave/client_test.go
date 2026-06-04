package slave

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestEntryEndpoint(t *testing.T) {
	tests := []struct {
		entry    Entry
		remote   string
		expected string
	}{
		{
			entry:    Entry{URL: "https://example.com/api/"},
			remote:   "/remote",
			expected: "https://example.com/api",
		},
		{
			entry:    Entry{Domain: "example.com", Scheme: "http"},
			remote:   "xraytool-remote",
			expected: "http://example.com/xraytool-remote",
		},
		{
			entry:    Entry{IP: "1.2.3.4", Port: 8080},
			remote:   "remote",
			expected: "https://1.2.3.4:8080/remote", // default scheme is https
		},
		{
			entry:    Entry{Host: "slave1.local", Path: "/custom-path"},
			remote:   "remote",
			expected: "https://slave1.local/custom-path",
		},
	}

	for _, tt := range tests {
		actual := tt.entry.Endpoint(tt.remote)
		if actual != tt.expected {
			t.Errorf("Endpoint failed. Expected %q, got %q", tt.expected, actual)
		}
	}
}

func TestUnwrapBody(t *testing.T) {
	tests := []struct {
		body     string
		expected string
		err      bool
	}{
		{body: `{"status":"success","output":"raw_data"}`, expected: "raw_data", err: false},
		{body: `{"status":"error","message":"failed"}`, expected: "", err: true},
		{body: `{"ok":false,"msg":"bad"}`, expected: "", err: true},
		{body: `plain text response`, expected: "plain text response", err: false},
		{body: `{"some":"data"}`, expected: `{"some":"data"}`, err: false},
	}

	for _, tt := range tests {
		actual, err := unwrapBody(tt.body)
		if tt.err && err == nil {
			t.Errorf("unwrapBody(%s) expected error, got nil", tt.body)
		}
		if !tt.err && err != nil {
			t.Errorf("unwrapBody(%s) unexpected error: %v", tt.body, err)
		}
		if actual != tt.expected {
			t.Errorf("unwrapBody(%s) expected %q, got %q", tt.body, tt.expected, actual)
		}
	}
}

func TestClientCall(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		var req map[string]string
		json.NewDecoder(r.Body).Decode(&req)

		if r.URL.Path == "/sync" {
			if req["action"] == "test" {
				w.Write([]byte(`{"status":"success","output":"test_ok"}`))
				return
			}
		}
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer ts.Close()

	client := NewClient(1*time.Second, 1*time.Second, "remote")

	entry := Entry{
		URL:    ts.URL,
		APIKey: "secret",
	}

	out, err := client.Call(entry, "sync", map[string]string{"action": "test"})
	if err != nil {
		t.Fatalf("Call error: %v", err)
	}
	if out != "test_ok" {
		t.Errorf("Expected test_ok, got %s", out)
	}

	// Test auth failure
	badEntry := Entry{
		URL:    ts.URL,
		APIKey: "wrong",
	}
	_, err = client.Call(badEntry, "sync", map[string]string{"action": "test"})
	if err == nil {
		t.Errorf("Expected error for wrong API key")
	}
}
