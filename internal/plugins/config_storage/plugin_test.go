package config_storage

import (
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
