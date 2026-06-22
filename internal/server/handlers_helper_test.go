package server_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"testing"

	"xraytool/internal/appconfig"
	"xraytool/internal/database"
	"xraytool/internal/server"
	"xraytool/internal/subscription"
)

func TestMain(m *testing.M) {
	// Initialize in-memory SQLite for all handler tests
	f, _ := os.CreateTemp("", "xraytool_test_*.db")
	f.Close()
	defer os.Remove(f.Name())

	if err := database.Init(database.Config{
		Driver:      "sqlite",
		SQLitePath:  f.Name(),
		AutoMigrate: true,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "db init failed: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func newTestRouter(t *testing.T) *server.Router {
	t.Helper()

	f, _ := os.CreateTemp("", "xrayconfig_*.json")
	f.WriteString(`{"inbounds":[]}`)
	f.Close()
	t.Cleanup(func() { os.Remove(f.Name()) })

	limitDBPath := f.Name() + ".limited.db"
	t.Cleanup(func() { os.Remove(limitDBPath) })

	cfg := &appconfig.Config{
		Server:        appconfig.ServerConf{Domain: "test.example.com"},
		Webhooks:      []string{}, // no webhooks in tests
		PlategaSecret: "test-platega-secret",
		Paths: appconfig.PathsConf{
			XrayConfig: f.Name(),
			LimitedDB:  limitDBPath,
		},
	}
	cm := subscription.NewCacheManager(cfg)
	return server.New(cfg, "test-api-key", cm)
}

func do(r *server.Router, method, path, body string, apiKey string) *httptest.ResponseRecorder {
	var bodyReader *bytes.Reader
	if body != "" {
		bodyReader = bytes.NewReader([]byte(body))
	} else {
		bodyReader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, bodyReader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func doAuth(r *server.Router, method, path, body string) *httptest.ResponseRecorder {
	return do(r, method, path, body, "test-api-key")
}

func jsonBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("invalid JSON response: %s", rec.Body.String())
	}
	return m
}
