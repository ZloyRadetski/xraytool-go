package appconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

	core, ok := cfg.Plugins["core"]
	if !ok || !core.Enabled || core.Source != "builtin" {
		t.Errorf("Expected mandatory built-in core plugin, got %#v", core)
	}
	if cfg.Engines.RoutingMode != "broadcast" {
		t.Errorf("Expected default engine routing mode broadcast, got %q", cfg.Engines.RoutingMode)
	}
	xray, ok := cfg.Engines.Entries["xray"]
	if !ok || !xray.Enabled || xray.Source != "builtin" {
		t.Errorf("Expected enabled built-in xray engine, got %#v", xray)
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

func TestLoadAllowsNoneLoggingLevel(t *testing.T) {
	tmpFile := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(tmpFile, []byte("logging:\n  level: none\n"), 0644); err != nil {
		t.Fatalf("WriteFile() error: %v", err)
	}

	cfg, err := Load(tmpFile)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Logging.Level != "none" {
		t.Fatalf("Logging.Level = %q, want none", cfg.Logging.Level)
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
	_ = os.WriteFile(tmpFile, []byte("invalid: [yaml: {"), 0644)
	if _, err := Load(tmpFile); err == nil {
		t.Error("Expected error parsing invalid config")
	}

	// 2. MkdirAll error (parent is a file)
	parentFile := filepath.Join(t.TempDir(), "parent.txt")
	_ = os.WriteFile(parentFile, []byte(""), 0644)
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
ports:
  api_server: 0
logging:
  level: ""
  format: ""
subscription:
  user_agent_whitelist: []
  user_agent_no_checks: []
`
	_ = os.WriteFile(tmpFile, []byte(customYAML), 0644)
	cfg, err := Load(tmpFile)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	defs := defaults()
	if cfg.Paths.XrayConfig != defs.Paths.XrayConfig ||
		cfg.Stats.BucketSeconds != defs.Stats.BucketSeconds ||
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
	_ = os.WriteFile(tmpFile, []byte(customYAML), 0644)
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

func TestLoadRejectsRemovedHTTPSyncConfiguration(t *testing.T) {
	tmpFile := filepath.Join(t.TempDir(), "removed-cluster-config.yaml")
	if err := os.WriteFile(tmpFile, []byte("master_api:\n  url: https://example.test/api/v1/internal/xray/sync\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(tmpFile)
	if err == nil || !strings.Contains(err.Error(), "master_api was removed") {
		t.Fatalf("expected removed master_api error, got %v", err)
	}
}

func TestDetailedRetentionSeconds(t *testing.T) {
	cfg := &Config{Stats: StatsConf{DetailedRetentionDays: 2}}
	if cfg.DetailedRetentionSeconds() != 2*24*60*60 {
		t.Errorf("Expected 172800, got %d", cfg.DetailedRetentionSeconds())
	}
}

func TestLoadPluginsAndEnginesConfig(t *testing.T) {
	tmpFile := filepath.Join(t.TempDir(), "plugins.yaml")
	customYAML := `
mode: slave
plugins:
  core:
    enabled: true
    source: builtin
  antifraud:
    enabled: true
    source: builtin
    config:
      max_ips: 7
  payment_yookassa:
    enabled: true
    source: external
    exec: /opt/xraytool/plugins/xraytool-plugin-yookassa
    args: ["--socket-dir=/run/xraytool/plugins"]
    restart_policy:
      max_restarts: 5
      backoff: 2s
    config:
      shop_id: "123456"
engines:
  routing_mode: by-plan
  xray:
    enabled: false
    source: builtin
  singbox:
    enabled: true
    source: external:/opt/xraytool/plugins/xraytool-engine-singbox
    config:
      api_addr: "127.0.0.1:19090"
`
	if err := os.WriteFile(tmpFile, []byte(customYAML), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(tmpFile)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	antifraud := cfg.Plugins["antifraud"]
	if !antifraud.Enabled || antifraud.Source != "builtin" {
		t.Errorf("Unexpected antifraud config: %#v", antifraud)
	}
	if got := antifraud.Config["max_ips"]; got != 7 {
		t.Errorf("Expected plugins.antifraud.config.max_ips=7, got %#v", got)
	}
	if got := antifraud.Config["ip_limit_ttl"]; got != "3m" {
		t.Errorf("Expected omitted antifraud config to retain legacy default, got %#v", got)
	}

	payment := cfg.Plugins["payment_yookassa"]
	if payment.Exec != "/opt/xraytool/plugins/xraytool-plugin-yookassa" || len(payment.Args) != 1 {
		t.Errorf("External plugin fields were not decoded: %#v", payment)
	}
	if payment.RestartPolicy.MaxRestarts != 5 || payment.RestartPolicy.Backoff != 2*time.Second {
		t.Errorf("Restart policy was not decoded: %#v", payment.RestartPolicy)
	}

	if cfg.Engines.RoutingMode != "by-plan" {
		t.Errorf("Expected by-plan routing, got %q", cfg.Engines.RoutingMode)
	}
	if cfg.Engines.Entries["xray"].Enabled {
		t.Error("Explicit engines.xray.enabled=false was overwritten by a default")
	}
	singbox := cfg.Engines.Entries["singbox"]
	if !singbox.Enabled || singbox.Source != "external:/opt/xraytool/plugins/xraytool-engine-singbox" {
		t.Errorf("Unexpected singbox config: %#v", singbox)
	}
}

func TestLoadPluginWithoutConfigUsesEmptyObject(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plugins-empty-config.yaml")
	if err := os.WriteFile(path, []byte(`
plugins:
  pricing_default:
    enabled: true
    source: builtin
`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	pricing := cfg.Plugins["pricing_default"]
	if pricing.Config == nil {
		t.Fatal("expected pricing_default.config to be an empty object, not null")
	}
	if len(pricing.Config) != 0 {
		t.Fatalf("expected empty pricing_default.config, got %#v", pricing.Config)
	}
}

func TestLoadLegacyConfigMaterializesPluginAndEngineDefaults(t *testing.T) {
	tmpFile := filepath.Join(t.TempDir(), "legacy.yaml")
	legacyYAML := `
mode: slave
engine:
  type: xray
xray:
  api_addr: "127.0.0.9:10085"
server:
  ip: "198.51.100.10"
paths:
  xray_config: "/srv/xray/config.json"
  xray_template: "/srv/xray/template.json"
  hy2_config_yaml: "/srv/hysteria/config.yaml"
reality:
  rotation_enabled: true
  keys_filepath: "/srv/xray/reality.keys"
blacklisted_admins: ["legacy-admin"]
anti_fraud:
  enabled: true
  dry_run: false
  max_ips: 9
mailer:
  enabled: true
  resend_api_key: "re_test"
  from_email: "noreply@example.test"
webhook_secret: "legacy-webhook-secret"
webhooks: ["https://example.test/hook"]
`
	if err := os.WriteFile(tmpFile, []byte(legacyYAML), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(tmpFile)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if !cfg.Plugins["core"].Enabled {
		t.Error("Legacy config did not materialize mandatory core plugin")
	}
	antifraud := cfg.Plugins["antifraud"]
	if !antifraud.Enabled || antifraud.Config["max_ips"] != 9 || antifraud.Config["dry_run"] != false {
		t.Errorf("Legacy anti_fraud was not translated: %#v", antifraud)
	}
	mailer := cfg.Plugins["mailer_resend"]
	if !mailer.Enabled || mailer.Config["resend_api_key"] != "re_test" {
		t.Errorf("Legacy mailer was not translated: %#v", mailer)
	}
	webhooks := cfg.Plugins["eventsink_webhook"]
	if !webhooks.Enabled || webhooks.Config["webhook_secret"] != "legacy-webhook-secret" {
		t.Errorf("Legacy webhooks were not translated: %#v", webhooks)
	}

	xray := cfg.Engines.Entries["xray"]
	if !xray.Enabled || xray.Source != "builtin" {
		t.Errorf("Legacy config did not materialize xray engine: %#v", xray)
	}
	if xray.Config["grpc_addr"] != "127.0.0.9:10085" ||
		xray.Config["config_path"] != "/srv/xray/config.json" ||
		xray.Config["template_path"] != "/srv/xray/template.json" ||
		xray.Config["server_address"] != "198.51.100.10" ||
		xray.Config["hy2_config_yaml"] != "/srv/hysteria/config.yaml" ||
		xray.Config["reality_rotation"] != true ||
		xray.Config["reality_keys_path"] != "/srv/xray/reality.keys" {
		t.Errorf("Legacy Xray values were not translated: %#v", xray.Config)
	}
}

func TestExplicitPluginAndEngineEntriesUseLegacyValuesForOmittedConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := `
mode: slave
xray:
  api_addr: "127.0.0.7:10085"
paths:
  xray_config: "/custom/config.json"
anti_fraud:
  enabled: true
  max_ips: 11
plugins:
  antifraud:
    enabled: true
    source: builtin
engines:
  xray:
    enabled: true
    source: builtin
`
	if err := os.WriteFile(path, []byte(yaml), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if got := cfg.Plugins["antifraud"].Config["max_ips"]; got != 11 {
		t.Errorf("Expected legacy antifraud max_ips=11, got %#v", got)
	}
	xray := cfg.Engines.Entries["xray"]
	if got := xray.Config["grpc_addr"]; got != "127.0.0.7:10085" {
		t.Errorf("Expected legacy xray api address, got %#v", got)
	}
	if got := xray.Config["config_path"]; got != "/custom/config.json" {
		t.Errorf("Expected legacy xray config path, got %#v", got)
	}
}

func TestXrayEngineConfigOverridesLegacyAdapterSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := `
mode: slave
xray:
  api_addr: "127.0.0.1:10085"
paths:
  xray_config: "/legacy/config.json"
  xray_template: "/legacy/template.json"
  hy2_config_yaml: "/legacy/hy2.yaml"
reality:
  rotation_enabled: false
  keys_filepath: "/legacy/reality.keys"
blacklisted_admins: ["legacy-admin"]
engines:
  xray:
    enabled: true
    source: builtin
    config:
      grpc_addr: "127.0.0.8:10085"
      config_path: "/engine/config.json"
      template_path: "/engine/template.json"
      hy2_config_yaml: "/engine/hy2.yaml"
      reality_rotation: true
      reality_keys_path: "/engine/reality.keys"
      blacklisted_admins: ["engine-admin"]
`
	if err := os.WriteFile(path, []byte(yaml), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Xray.APIAddr != "127.0.0.8:10085" || cfg.Paths.XrayConfig != "/engine/config.json" || cfg.Paths.XrayTemplate != "/engine/template.json" {
		t.Fatalf("engine adapter settings were not applied: xray=%q config=%q template=%q", cfg.Xray.APIAddr, cfg.Paths.XrayConfig, cfg.Paths.XrayTemplate)
	}
	if cfg.Paths.Hy2ConfigYAML != "/engine/hy2.yaml" || !cfg.Reality.RotationEnabled || cfg.Reality.KeysFilepath != "/engine/reality.keys" {
		t.Fatalf("engine auxiliary settings were not applied: hy2=%q rotation=%v keys=%q", cfg.Paths.Hy2ConfigYAML, cfg.Reality.RotationEnabled, cfg.Reality.KeysFilepath)
	}
	if len(cfg.BlacklistedAdmins) != 1 || cfg.BlacklistedAdmins[0] != "engine-admin" {
		t.Fatalf("blacklisted admins = %#v, want engine override", cfg.BlacklistedAdmins)
	}
}

func TestPluginAndEngineConfigValidation(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name: "core cannot be disabled",
			yaml: `
mode: slave
plugins:
  core:
    enabled: false
    source: builtin
`,
			wantErr: "plugins.core is mandatory",
		},
		{
			name: "at least one engine required",
			yaml: `
mode: slave
engines:
  xray:
    enabled: false
    source: builtin
`,
			wantErr: "at least one engine",
		},
		{
			name: "enabled external plugin requires executable",
			yaml: `
mode: slave
plugins:
  payment_yookassa:
    enabled: true
    source: external
`,
			wantErr: "plugins.payment_yookassa.exec is required",
		},
		{
			name: "routing mode is constrained",
			yaml: `
mode: slave
engines:
  routing_mode: random
`,
			wantErr: "engines.routing_mode must be one of",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tt.yaml), 0600); err != nil {
				t.Fatalf("write config: %v", err)
			}
			_, err := Load(path)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Load() error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestBuiltinPluginConfigSchemasAreValidatedBeforeHostStartup(t *testing.T) {
	tests := []struct {
		name    string
		plugin  string
		config  string
		wantErr string
	}{
		{
			name:    "wrong antifraud type",
			plugin:  "antifraud",
			config:  `max_ips: "seven"`,
			wantErr: "$.max_ips: expected integer",
		},
		{
			name:    "unknown pricing key",
			plugin:  "pricing_default",
			config:  "unexpected: true",
			wantErr: "additional property is not allowed",
		},
		{
			name:    "platega requires secret",
			plugin:  "payment_platega",
			config:  "merchant_id: merchant",
			wantErr: "$.secret: length must be at least 1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			yaml := "mode: slave\nplugins:\n  " + tt.plugin + ":\n    enabled: true\n    source: builtin\n    config:\n      " + strings.ReplaceAll(tt.config, "\n", "\n      ") + "\n"
			if err := os.WriteFile(path, []byte(yaml), 0600); err != nil {
				t.Fatalf("write config: %v", err)
			}
			_, err := Load(path)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Load() error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestExternalPluginConfigSchemaIsResolvedRelativeToManifest(t *testing.T) {
	dir := t.TempDir()
	bundle := filepath.Join(dir, "plugins", "payment-demo")
	schemaDir := filepath.Join(bundle, "schemas")
	if err := os.MkdirAll(schemaDir, 0755); err != nil {
		t.Fatalf("create schema directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(schemaDir, "config.schema.json"), []byte(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["api_key"],
  "properties": {"api_key": {"type": "string", "minLength": 1}}
}`), 0600); err != nil {
		t.Fatalf("write schema: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "plugin.yaml"), []byte(`
name: payment_demo
kind: payment
version: 1.0.0
api_version: "1"
type: external
config_schema: ./schemas/config.schema.json
`), 0600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	configPath := filepath.Join(dir, "config.yaml")
	invalid := `
mode: slave
plugins:
  payment_demo:
    enabled: true
    source: external
    exec: ./plugins/payment-demo/payment-demo
    manifest: ./plugins/payment-demo/plugin.yaml
    config:
      api_key: 42
`
	if err := os.WriteFile(configPath, []byte(invalid), 0600); err != nil {
		t.Fatalf("write invalid config: %v", err)
	}
	_, err := Load(configPath)
	if err == nil || !strings.Contains(err.Error(), "$.api_key: expected string") {
		t.Fatalf("Load() error = %v, want external schema type error", err)
	}

	valid := strings.Replace(invalid, "api_key: 42", "api_key: test-key", 1)
	if err := os.WriteFile(configPath, []byte(valid), 0600); err != nil {
		t.Fatalf("write valid config: %v", err)
	}
	if _, err := Load(configPath); err != nil {
		t.Fatalf("Load() valid external config error: %v", err)
	}
}
