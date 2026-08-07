package slave

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestClientCallErrors(t *testing.T) {
	client := NewClient(1*time.Second, 1*time.Second, "remote")

	// 1. Endpoint error
	emptyEntry := Entry{}
	_, err := client.Call(emptyEntry, "cmd", nil)
	if err == nil || !strings.Contains(err.Error(), "cannot determine endpoint") {
		t.Errorf("Expected endpoint error, got %v", err)
	}

	// 2. http.NewRequest error - invalid URL character
	badURLEntry := Entry{URL: "http://192.168.1.1\x7f/api"}
	_, err = client.Call(badURLEntry, "cmd", nil)
	if err == nil || !strings.Contains(err.Error(), "building request") {
		t.Errorf("Expected building request error, got %v", err)
	}

	// 3. httpClient.Do error (dial error / timeout)
	timeoutEntry := Entry{URL: "http://255.255.255.255:9999"}
	clientFast := NewClient(1*time.Millisecond, 1*time.Millisecond, "remote")
	_, err = clientFast.Call(timeoutEntry, "cmd", nil)
	if err == nil || !strings.Contains(err.Error(), "request to") {
		t.Errorf("Expected request error, got %v", err)
	}

	// 4. Bad status code without body
	ts500 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts500.Close()
	_, err = client.Call(Entry{URL: ts500.URL}, "cmd", nil)
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("Expected HTTP 500 error, got %v", err)
	}

	// 5. Bad status code with body
	ts500Body := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal error")) //nolint:errcheck
	}))
	defer ts500Body.Close()
	_, err = client.Call(Entry{URL: ts500Body.URL}, "cmd", nil)
	if err == nil || !strings.Contains(err.Error(), "internal error") {
		t.Errorf("Expected HTTP 500 error with body, got %v", err)
	}

	// 6. Insecure client path
	tsInsecure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`ok`)) //nolint:errcheck
	}))
	defer tsInsecure.Close()
	_, err = client.Call(Entry{URL: tsInsecure.URL, AllowInsecure: true}, "cmd", nil)
	if err != nil {
		t.Errorf("Expected insecure request to succeed, got %v", err)
	}
	_, err = client.Call(Entry{URL: tsInsecure.URL, Insecure: true}, "cmd", nil)
	if err != nil {
		t.Errorf("Expected insecure request to succeed, got %v", err)
	}
}

func TestUnwrapBodyCoverage(t *testing.T) {
	// 1. empty body
	res, err := unwrapBody("")
	if err != nil || res != "" {
		t.Errorf("Expected empty string, got %v, %v", res, err)
	}

	// 2. wrong type for status
	res, err = unwrapBody(`{"status":123}`)
	if err != nil || res != `{"status":123}` {
		t.Errorf("Expected plain response for wrong status type, got %v, %v", res, err)
	}

	// 3. wrong type for ok
	res, err = unwrapBody(`{"ok":"not_a_bool"}`)
	if err != nil || res != `{"ok":"not_a_bool"}` {
		t.Errorf("Expected plain response for wrong ok type, got %v, %v", res, err)
	}

	// 4. wrong type for output
	res, err = unwrapBody(`{"output":123}`)
	if err != nil || res != `{"output":123}` {
		t.Errorf("Expected plain response for wrong output type, got %v, %v", res, err)
	}
}

func TestApplyAuthHeaders(t *testing.T) {
	req, _ := http.NewRequest("POST", "http://test", nil)

	e := Entry{
		Token:      "tok_123",
		Bearer:     "bear_123",
		AuthHeader: "X-Custom: custom_val",
	}
	applyAuthHeaders(req, e)

	if req.Header.Get("X-API-Key") != "tok_123" {
		t.Errorf("Expected X-API-Key to be tok_123")
	}
	if req.Header.Get("Authorization") != "Bearer bear_123" {
		t.Errorf("Expected Authorization to be Bearer bear_123")
	}
	if req.Header.Get("X-Custom") != "custom_val" {
		t.Errorf("Expected X-Custom to be custom_val")
	}

	// Test malformed / empty headers
	req2, _ := http.NewRequest("POST", "http://test", nil)
	e2 := Entry{
		AuthHeader:    "null",
		Authorization: "InvalidHeaderNoColon",
	}
	applyAuthHeaders(req2, e2)
	if len(req2.Header) > 0 {
		t.Errorf("Expected no headers applied, got %v", req2.Header)
	}
}

func TestPortString(t *testing.T) {
	tests := []struct {
		val interface{}
		exp string
	}{
		{nil, ""},
		{"0", ""},
		{0, ""},
		{"", ""},
		{"<nil>", ""},
		{8080, "8080"},
		{"9090", "9090"},
		{int64(443), "443"},
	}
	for _, tt := range tests {
		if got := portString(tt.val); got != tt.exp {
			t.Errorf("portString(%v) expected %q, got %q", tt.val, tt.exp, got)
		}
	}
}

func TestEntryEndpointEmpty(t *testing.T) {
	e := Entry{}
	if got := e.Endpoint("cmd"); got != "" {
		t.Errorf("Expected empty endpoint, got %q", got)
	}
}

//nolint:unused
type errReader struct{}

//nolint:unused
//nolint:unused
//nolint:unused
//nolint:unused
func (errReader) Read(p []byte) (n int, err error) { //nolint:unused
	return 0, errors.New("mock read error")
}

func TestClientCallReadError(t *testing.T) {
	// A server that terminates the connection abruptly or we force a read error.
	// We can inject it using a custom RoundTripper, or we just write a broken response?
	// It's easier to use a httptest server that closes connection:
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		// connection hijacked and closed to simulate read error
		hj, ok := w.(http.Hijacker)
		if ok {
			conn, _, _ := hj.Hijack()
			conn.Close()
		}
	}))
	defer ts.Close()

	client := NewClient(1*time.Second, 1*time.Second, "remote")
	_, err := client.Call(Entry{URL: ts.URL}, "cmd", nil)
	if err == nil || !strings.Contains(err.Error(), "reading response") {
		t.Errorf("Expected read error, got %v", err)
	}
}
