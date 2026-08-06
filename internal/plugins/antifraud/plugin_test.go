package antifraud_plugin_test

import (
	"context"
	"testing"

	antifraudPlugin "xraytool/internal/plugins/antifraud"
	"xraytool/internal/pluginapi"
)

func TestAntifraud_Metadata(t *testing.T) {
	t.Parallel()
	p := antifraudPlugin.New()
	m := p.Metadata()
	if m.Name != "antifraud" {
		t.Errorf("Name = %q, want %q", m.Name, "antifraud")
	}
	if m.Kind != "antifraud" {
		t.Errorf("Kind = %q, want %q", m.Kind, "antifraud")
	}
	if m.Mandatory {
		t.Error("antifraud must not be mandatory")
	}
	if len(m.Requires) == 0 {
		t.Error("antifraud must declare at least one required service")
	}
}

func TestAntifraud_Init_DefaultConfig(t *testing.T) {
	t.Parallel()
	p := antifraudPlugin.New()
	if err := p.Init(context.Background(), nil, nil); err != nil {
		t.Fatalf("Init(nil config) should succeed with defaults, got: %v", err)
	}
	cfg := p.Config()
	if cfg == nil {
		t.Fatal("Config() should not be nil after Init()")
	}
	// Defaults
	if cfg.Enabled {
		t.Error("default enabled should be false")
	}
	if cfg.SuspiciousIPThreshold != 3 {
		t.Errorf("default max_ips should be 3, got %d", cfg.SuspiciousIPThreshold)
	}
}

func TestAntifraud_Init_ParsesConfig(t *testing.T) {
	t.Parallel()
	p := antifraudPlugin.New()
	err := p.Init(context.Background(), pluginapi.RawConfig{
		"enabled":  true,
		"dry_run":  true,
		"log_path": "/dev/shm/xray-access.log",
		"max_ips":  5,
	}, nil)
	if err != nil {
		t.Fatalf("Init() failed: %v", err)
	}
	cfg := p.Config()
	if !cfg.Enabled {
		t.Error("enabled should be true after parsing")
	}
	if !cfg.DryRun {
		t.Error("dry_run should be true after parsing")
	}
	if cfg.LogPath != "/dev/shm/xray-access.log" {
		t.Errorf("log_path = %q, want /dev/shm/xray-access.log", cfg.LogPath)
	}
	if cfg.SuspiciousIPThreshold != 5 {
		t.Errorf("max_ips = %d, want 5", cfg.SuspiciousIPThreshold)
	}
}

func TestAntifraud_IsBanned_WithoutModule_ReturnsFalse(t *testing.T) {
	t.Parallel()
	p := antifraudPlugin.New()
	_ = p.Init(context.Background(), nil, nil)
	// Module not initialised (InitWithDependencies not called) → always returns false.
	if p.IsBanned("any@example.com") {
		t.Error("IsBanned() without module should return false")
	}
}

func TestAntifraud_ForceUnban_WithoutModule_NoError(t *testing.T) {
	t.Parallel()
	p := antifraudPlugin.New()
	_ = p.Init(context.Background(), nil, nil)
	if err := p.ForceUnban(context.Background(), "any@example.com"); err != nil {
		t.Errorf("ForceUnban() without module should be a no-op, got: %v", err)
	}
}

func TestAntifraud_IngestEvents_WithoutModule_NoError(t *testing.T) {
	t.Parallel()
	p := antifraudPlugin.New()
	_ = p.Init(context.Background(), nil, nil)
	err := p.IngestEvents(context.Background(), "slave-1", []pluginapi.FraudEvent{
		{Email: "user@example.com", IP: "1.2.3.4"},
	})
	if err != nil {
		t.Errorf("IngestEvents() without module should be a no-op, got: %v", err)
	}
}

func TestAntifraud_Health_DisabledPlugin_NoError(t *testing.T) {
	t.Parallel()
	p := antifraudPlugin.New()
	// enabled=false → module nil is acceptable
	_ = p.Init(context.Background(), pluginapi.RawConfig{"enabled": false}, nil)
	if err := p.Health(context.Background()); err != nil {
		t.Errorf("Health() with disabled antifraud should return nil, got: %v", err)
	}
}

func TestAntifraud_Health_EnabledWithoutModule_Error(t *testing.T) {
	t.Parallel()
	p := antifraudPlugin.New()
	_ = p.Init(context.Background(), pluginapi.RawConfig{"enabled": true}, nil)
	// Module not initialised → Health should report error
	if err := p.Health(context.Background()); err == nil {
		t.Error("Health() with enabled antifraud but uninitialised module should return error")
	}
}

func TestAntifraud_SetBanSink_DoesNotPanic(t *testing.T) {
	t.Parallel()
	p := antifraudPlugin.New()
	// Should not panic even if sink is nil
	p.SetBanSink(nil)
}

func TestAntifraud_Snapshot_WithoutModule_EmptyMap(t *testing.T) {
	t.Parallel()
	p := antifraudPlugin.New()
	_ = p.Init(context.Background(), nil, nil)
	snap, err := p.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot() without module should not error, got: %v", err)
	}
	if snap == nil {
		t.Error("Snapshot() should return empty map, not nil")
	}
}
