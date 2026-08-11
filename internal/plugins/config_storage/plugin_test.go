package config_storage

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"xraytool/internal/appconfig"
)

func TestPlugin_AllowedRestrictsToConfiguredRoots(t *testing.T) {
	root := t.TempDir()
	p := New(&appconfig.Config{})
	p.cfg.Server.AllowedDirs = []string{root}
	if !p.allowed(filepath.Join(root, "templates", "config.json")) {
		t.Fatal("expected nested path in allowed root to be accepted")
	}
	if p.allowed(filepath.Join(root, "..", "outside.json")) {
		t.Fatal("expected traversal outside allowed root to be rejected")
	}
}

func TestPlugin_RegisterRoutesOwnsStorageEndpoints(t *testing.T) {
	p := New(&appconfig.Config{})
	p.auth = func(handler http.Handler) http.Handler { return handler }
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/rest/download", nil))
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("expected config-storage download handler, got status %d", recorder.Code)
	}
}
