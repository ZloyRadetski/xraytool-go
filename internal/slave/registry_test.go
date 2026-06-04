package slave

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseServers(t *testing.T) {
	// Test object format
	objJSON := []byte(`{
		"server1": {"domain": "s1.com"},
		"server2": {"domain": "s2.com"}
	}`)
	servers, err := parseServers(objJSON)
	if err != nil || len(servers) != 2 || servers["server1"].Domain != "s1.com" {
		t.Errorf("Failed to parse object format: %v, %v", err, servers)
	}

	// Test array format
	arrJSON := []byte(`[
		{"name": "s1", "domain": "s1.com"},
		{"id": "s2", "domain": "s2.com"},
		{"key": "s3", "domain": "s3.com"},
		{"server": "s4", "domain": "s4.com"},
		{"domain": "s5.com"},
		"invalid"
	]`)
	servers, err = parseServers(arrJSON)
	if err != nil || len(servers) != 5 {
		t.Errorf("Failed to parse array format: %v, %v", err, servers)
	}
	if servers["s1"].Domain != "s1.com" || servers["s2"].Domain != "s2.com" ||
		servers["s3"].Domain != "s3.com" || servers["s4"].Domain != "s4.com" ||
		servers["4"].Domain != "s5.com" {
		t.Errorf("Array format names parsed incorrectly: %v", servers)
	}

	// Test bad JSON
	badJSON := []byte(`{bad json`)
	_, err = parseServers(badJSON)
	if err == nil {
		t.Errorf("Expected error for bad JSON")
	}
}

func TestRegistryServers(t *testing.T) {
	dir := t.TempDir()
	
	// Test missing file
	reg := NewRegistry(filepath.Join(dir, "missing.json"), nil)
	servers, err := reg.Servers()
	if err != nil || servers != nil {
		t.Errorf("Expected nil, nil for missing file, got %v, %v", servers, err)
	}

	// Test valid file
	validFile := filepath.Join(dir, "servers.json")
	os.WriteFile(validFile, []byte(`{"s1": {"domain": "s1.com"}}`), 0644)
	reg = NewRegistry(validFile, nil)
	servers, err = reg.Servers()
	if err != nil || len(servers) != 1 {
		t.Errorf("Expected 1 server, got %v, err: %v", servers, err)
	}

	// Test unreadable file (we can test bad json to get unmarshal error inside parseServers, wait, parseServers error handled?)
	badFile := filepath.Join(dir, "bad.json")
	os.WriteFile(badFile, []byte(`bad`), 0644)
	reg = NewRegistry(badFile, nil)
	_, err = reg.Servers()
	if err == nil {
		t.Errorf("Expected error for bad JSON file")
	}
}

func TestRegistryPropagateAndCallOne(t *testing.T) {
	ts1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"success","output":"s1_ok"}`))
	}))
	defer ts1.Close()

	ts2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`error`))
	}))
	defer ts2.Close()

	dir := t.TempDir()
	cfg := filepath.Join(dir, "servers.json")
	cfgData := map[string]Entry{
		"s1": {URL: ts1.URL},
		"s2": {URL: ts2.URL},
	}
	cfgBytes, _ := json.Marshal(cfgData)
	os.WriteFile(cfg, cfgBytes, 0644)

	client := NewClient(1*time.Second, 1*time.Second, "remote")
	reg := NewRegistry(cfg, client)

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

	// Test with missing file
	regEmpty := NewRegistry(filepath.Join(dir, "missing.json"), client)
	if res := regEmpty.PropagateAll("cmd", nil); res != nil {
		t.Errorf("PropagateAll on missing file should return nil")
	}
	if _, err := regEmpty.CallOne("s1", "cmd", nil); err == nil {
		t.Errorf("CallOne on missing file should return error")
	}

	// Test empty servers file
	emptyCfg := filepath.Join(dir, "empty.json")
	os.WriteFile(emptyCfg, []byte(`{}`), 0644)
	regEmptyJSON := NewRegistry(emptyCfg, client)
	if res := regEmptyJSON.PropagateAll("cmd", nil); res != nil {
		t.Errorf("PropagateAll on empty file should return nil")
	}
}

func TestRegistryServersReadError(t *testing.T) {
	dir := t.TempDir()
	badPath := filepath.Join(dir, "is_dir")
	os.MkdirAll(badPath, 0755)

	client := NewClient(1*time.Second, 1*time.Second, "remote")
	reg := NewRegistry(badPath, client)

	// Test Servers error
	_, err := reg.Servers()
	if err == nil {
		t.Errorf("Expected error reading a directory, got nil")
	}

	// Test PropagateAll with Servers error
	if res := reg.PropagateAll("cmd", nil); res != nil {
		t.Errorf("PropagateAll should return nil on Servers error")
	}

	// Test CallOne with Servers error
	if _, err := reg.CallOne("s1", "cmd", nil); err == nil {
		t.Errorf("CallOne should return error on Servers error")
	}
}
