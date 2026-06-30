package appconfig

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDefaultConfig(t *testing.T) {
	tmpFile := filepath.Join(t.TempDir(), "config.yaml")

	// Loading non-existent file should create it with defaults
	cfg, err := Load(tmpFile)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg == nil {
		t.Fatal("Expected config, got nil")
	}

	if cfg.Mode != "master" {
		t.Errorf("Expected mode 'master', got %s", cfg.Mode)
	}

	if cfg.Ports.APIServer != 8080 {
		t.Errorf("Expected APIServer port 8080, got %d", cfg.Ports.APIServer)
	}

	if cfg.Logging.Level != "info" {
		t.Errorf("Expected Logging.Level 'info', got %s", cfg.Logging.Level)
	}

	// Verify file was created
	if _, err := os.Stat(tmpFile); os.IsNotExist(err) {
		t.Error("Default config file was not created")
	}

	if len(cfg.Subscription.DummyConfigs.Expired) == 0 {
		t.Error("Expected DummyConfigs.Expired to be populated with defaults")
	}
}

func TestLoadCustomConfig(t *testing.T) {
	tmpFile := filepath.Join(t.TempDir(), "config.yaml")
	customYAML := `
mode: slave
ports:
  api_server: 9090
logging:
  level: debug
subscription:
  user_agent_whitelist: ["custom1", "custom2"]
  user_agent_no_checks: ["custom1"]
worker:
  expiry_interval: "2m"
  expiration_warnings: ["48h", "2h"]
`
	err := os.WriteFile(tmpFile, []byte(customYAML), 0644)
	if err != nil {
		t.Fatalf("Failed to write custom config: %v", err)
	}

	cfg, err := Load(tmpFile)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.Mode != "slave" {
		t.Errorf("Expected mode 'slave', got %s", cfg.Mode)
	}

	if cfg.Ports.APIServer != 9090 {
		t.Errorf("Expected APIServer port 9090, got %d", cfg.Ports.APIServer)
	}

	if cfg.Logging.Level != "debug" {
		t.Errorf("Expected Logging.Level 'debug', got %s", cfg.Logging.Level)
	}

	if len(cfg.Subscription.UserAgentWhitelist) != 2 || cfg.Subscription.UserAgentWhitelist[0] != "custom1" {
		t.Errorf("Expected UserAgentWhitelist 'custom1', got %v", cfg.Subscription.UserAgentWhitelist)
	}

	if len(cfg.Subscription.UserAgentNoChecks) != 1 || cfg.Subscription.UserAgentNoChecks[0] != "custom1" {
		t.Errorf("Expected UserAgentNoChecks 'custom1', got %v", cfg.Subscription.UserAgentNoChecks)
	}

	if cfg.Worker.ExpiryInterval != "2m" {
		t.Errorf("Expected Worker.ExpiryInterval '2m', got %v", cfg.Worker.ExpiryInterval)
	}

	if len(cfg.Worker.ExpirationWarnings) != 2 || cfg.Worker.ExpirationWarnings[0] != "48h" || cfg.Worker.ExpirationWarnings[1] != "2h" {
		t.Errorf("Expected Worker.ExpirationWarnings ['48h', '2h'], got %v", cfg.Worker.ExpirationWarnings)
	}
}

func TestIsMaster(t *testing.T) {
	cfg := &Config{Mode: "master"}
	if !cfg.IsMaster() {
		t.Error("IsMaster() returned false for mode 'master'")
	}

	cfg.Mode = "slave"
	if cfg.IsMaster() {
		t.Error("IsMaster() returned true for mode 'slave'")
	}
}

func TestLoadErrors(t *testing.T) {
	// 1. Unparseable JSON/YAML
	tmpFile := filepath.Join(t.TempDir(), "invalid.yaml")
	os.WriteFile(tmpFile, []byte("invalid: [yaml: {"), 0644)
	if _, err := Load(tmpFile); err == nil {
		t.Error("Expected error parsing invalid config")
	}

	// 2. MkdirAll error (parent is a file)
	parentFile := filepath.Join(t.TempDir(), "parent.txt")
	os.WriteFile(parentFile, []byte(""), 0644)
	if _, err := Load(filepath.Join(parentFile, "config.yaml")); err == nil {
		t.Error("Expected error creating config directory")
	}

	// 3. WriteFile error (empty path)
	if _, err := Load(""); err == nil {
		t.Error("Expected error creating default config")
	}

	// 4. ReadFile error not IsNotExist (e.g. invalid chars or directory)
	if _, err := Load(t.TempDir()); err == nil {
		t.Error("Expected error reading config when path is a directory")
	}
}

func TestLoadEmptyConfigDefaults(t *testing.T) {
	tmpFile := filepath.Join(t.TempDir(), "empty.yaml")
	// Explicitly set empty/zero values so the fallbacks trigger
	customYAML := `
server:
  domain: "test.com"
database:
  driver: "sqlite"
paths:
  xray_config: ""
  stats_state: ""
  inferred_stats: ""
  json_subscription_template: ""
  routing_template: ""
  routing_ru_template: ""
  hy2_config_yaml: ""
  geoip_dat: ""
  geosite_dat: ""
xray:
  api_addr: ""
stats:
  bucket_seconds: 0
  detailed_retention_days: 0
slave_api:
  connect_timeout: 0s
  request_timeout: 0s
  remote_path: ""
ports:
  api_server: 0
logging:
  level: ""
  format: ""
subscription:
  user_agent_whitelist: []
  user_agent_no_checks: []
`
	os.WriteFile(tmpFile, []byte(customYAML), 0644)
	cfg, err := Load(tmpFile)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	defs := defaults()
	if cfg.Paths.XrayConfig != defs.Paths.XrayConfig ||
		cfg.Stats.BucketSeconds != defs.Stats.BucketSeconds ||
		cfg.SlaveAPI.ConnectTimeout != defs.SlaveAPI.ConnectTimeout ||
		cfg.Ports.APIServer != defs.Ports.APIServer ||
		cfg.Logging.Level != defs.Logging.Level ||
		cfg.Paths.InferredStats != defs.Paths.InferredStats ||
		cfg.Paths.RoutingTemplate != defs.Paths.RoutingTemplate ||
		cfg.Paths.RoutingRUTemplate != defs.Paths.RoutingRUTemplate ||
		cfg.Paths.Hy2ConfigYAML != defs.Paths.Hy2ConfigYAML ||
		cfg.Paths.GeoIPDat != defs.Paths.GeoIPDat ||
		cfg.Paths.GeositeDat != defs.Paths.GeositeDat ||
		cfg.Xray.APIAddr != defs.Xray.APIAddr ||
		cfg.Stats.DetailedRetentionDays != defs.Stats.DetailedRetentionDays ||
		cfg.SlaveAPI.RequestTimeout != defs.SlaveAPI.RequestTimeout ||
		cfg.SlaveAPI.RemotePath != defs.SlaveAPI.RemotePath ||
		cfg.Logging.Format != defs.Logging.Format ||
		len(cfg.Subscription.UserAgentWhitelist) != len(defs.Subscription.UserAgentWhitelist) ||
		len(cfg.Subscription.UserAgentNoChecks) != len(defs.Subscription.UserAgentNoChecks) {
		t.Error("Empty config did not correctly apply all defaults")
	}
}

func TestLegacyKeysMigration(t *testing.T) {
	tmpFile := filepath.Join(t.TempDir(), "legacy.yaml")
	customYAML := `
paths:
  json_subscription_template: ""
  subscription_template: "/legacy/sub.txt"
  subscription_vless_template: "/legacy/vless.txt"
`
	os.WriteFile(tmpFile, []byte(customYAML), 0644)
	cfg, err := Load(tmpFile)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.Paths.JSONSubscriptionTemplate != "/legacy/sub.txt" {
		t.Errorf("Expected legacy sub template to migrate, got %s", cfg.Paths.JSONSubscriptionTemplate)
	}
	if cfg.Paths.VlessSubscriptionTemplate != "/legacy/vless.txt" {
		t.Errorf("Expected legacy vless template to migrate, got %s", cfg.Paths.VlessSubscriptionTemplate)
	}
}

func TestDetailedRetentionSeconds(t *testing.T) {
	cfg := &Config{Stats: StatsConf{DetailedRetentionDays: 2}}
	if cfg.DetailedRetentionSeconds() != 2*24*60*60 {
		t.Errorf("Expected 172800, got %d", cfg.DetailedRetentionSeconds())
	}
}
