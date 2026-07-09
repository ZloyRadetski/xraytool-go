package slave

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRegistryServers(t *testing.T) {
	// Test nil map
	reg := NewRegistry(nil, nil)
	servers, err := reg.Servers()
	if err != nil || len(servers) != 0 {
		t.Errorf("Expected empty map for nil, got %v, %v", servers, err)
	}

	// Test valid map
	reg = NewRegistry(map[string]Entry{"s1": {Domain: "s1.com"}}, nil)
	servers, err = reg.Servers()
	if err != nil || len(servers) != 1 {
		t.Errorf("Expected 1 server, got %v, err: %v", servers, err)
	}
}

func TestRegistryPropagateAndCallOne(t *testing.T) {
	ts1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"success","output":"s1_ok"}`)) //nolint:errcheck
	}))
	defer ts1.Close()

	ts2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`error`)) //nolint:errcheck
	}))
	defer ts2.Close()

	cfgData := map[string]Entry{
		"s1": {URL: ts1.URL},
		"s2": {URL: ts2.URL},
	}

	client := NewClient(1*time.Second, 1*time.Second, "remote")
	reg := NewRegistry(cfgData, client)

	// Test PropagateAll
	results := reg.PropagateAll("cmd", nil)
	if len(results) != 2 {
		t.Errorf("Expected 2 results, got %d", len(results))
	}
	resMap := make(map[string]PropagateResult)
	for _, r := range results {
		resMap[r.Server] = r
	}
	if resMap["s1"].Output != "s1_ok" || resMap["s1"].Err != nil {
		t.Errorf("Expected s1_ok and no error, got %v", resMap["s1"])
	}
	if resMap["s2"].Err == nil {
		t.Errorf("Expected error for s2")
	}

	// Test CallOne success
	out, err := reg.CallOne("s1", "cmd", nil)
	if err != nil || out != "s1_ok" {
		t.Errorf("CallOne s1 failed: %v, %v", out, err)
	}

	// Test CallOne failure
	_, err = reg.CallOne("s2", "cmd", nil)
	if err == nil {
		t.Errorf("CallOne s2 expected error")
	}

	// Test CallOne unknown
	_, err = reg.CallOne("unknown", "cmd", nil)
	if err == nil {
		t.Errorf("CallOne unknown expected error")
	}

	// Test with nil map
	regEmpty := NewRegistry(nil, client)
	if res := regEmpty.PropagateAll("cmd", nil); res != nil {
		t.Errorf("PropagateAll on empty map should return nil")
	}
	if _, err := regEmpty.CallOne("s1", "cmd", nil); err == nil {
		t.Errorf("CallOne on empty map should return error")
	}
}

func TestRegistryPropagateAll_ConcurrencySafe(t *testing.T) {
	ts1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"success","output":"ok"}`)) //nolint:errcheck
	}))
	defer ts1.Close()

	ts2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"success","output":"ok"}`)) //nolint:errcheck
	}))
	defer ts2.Close()

	cfgData := map[string]Entry{
		"s1": {URL: ts1.URL},
		"s2": {URL: ts2.URL},
	}

	client := NewClient(1*time.Second, 1*time.Second, "remote")
	reg := NewRegistry(cfgData, client)

	sharedParams := map[string]string{
		"some_key": "some_val",
	}

	// Should not panic from concurrent map writes
	results := reg.PropagateAll("cmd", sharedParams)
	if len(results) != 2 {
		t.Errorf("Expected 2 results, got %d", len(results))
	}
}
