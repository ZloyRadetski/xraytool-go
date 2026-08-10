// Package appconfig loads and provides the tool's own configuration (config.yaml).
// This is NOT the xray config — it's xraytool's settings (paths, domain, mode, etc).
package appconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"xraytool/internal/pluginmanifest"

	"gopkg.in/yaml.v3"
)

// Config is the top-level xraytool configuration.
type Config struct {
	Mode string `yaml:"mode"` // "master" or "slave"
	// Engine is the legacy single-engine configuration. It remains supported
	// while callers migrate to Engines.
	Engine EngineConf `yaml:"engine"`
	// Engines configures the set of VPN engine plugins and their routing mode.
	// It supersedes Engine without removing backward compatibility for existing
	// config files.
	Engines EnginesConf `yaml:"engines"`
	// Plugins configures optional and mandatory application plugins. It is kept
	// in appconfig rather than pluginhost to avoid an import cycle: pluginhost
	// already needs appconfig to construct built-in plugins.
	Plugins           PluginsConf           `yaml:"plugins"`
	Server            ServerConf            `yaml:"server"`
	Paths             PathsConf             `yaml:"paths"`
	Xray              XrayConf              `yaml:"xray"`
	Stats             StatsConf             `yaml:"stats"`
	SlaveAPI          SlaveAPIConf          `yaml:"slave_api"`
	MasterAPI         MasterAPIConf         `yaml:"master_api"`
	Ports             PortsConf             `yaml:"ports"`
	Logging           LoggingConf           `yaml:"logging"`
	Webhooks          []string              `yaml:"webhooks"`
	Worker            WorkerConf            `yaml:"worker"`
	Database          DatabaseConf          `yaml:"database"`
	PlategaMerchantID string                `yaml:"platega_merchant_id"`
	PlategaSecret     string                `yaml:"platega_secret"`
	WebhookSecret     string                `yaml:"webhook_secret"`
	Subscription      SubscriptionConf      `yaml:"subscription"`
	SlaveServers      map[string]SlaveEntry `yaml:"slave_servers"`
	AntiFraud         AntiFraudConf         `yaml:"anti_fraud"`
	Mailer            MailerConf            `yaml:"mailer"`
	Reality           RealityConf           `yaml:"reality"`
	BlacklistedAdmins []string              `yaml:"blacklisted_admins"`

	// configDir is set by Load and is deliberately not serialised. It gives
	// external plugin manifests a stable base for relative config_schema paths.
	configDir string
}

// SlaveEntry is the declarative configuration of a cluster node. It lives in
// appconfig rather than internal/slave so a minimal build can parse its config
// without linking the optional cluster transport. commandruntime converts it
// to slave.Entry only in the non-minimal compatibility path.
type SlaveEntry struct {
	URL    string      `json:"url" yaml:"url"`
	Domain string      `json:"domain" yaml:"domain"`
	Host   string      `json:"host" yaml:"host"`
	IP     string      `json:"ip" yaml:"ip"`
	Scheme string      `json:"scheme" yaml:"scheme"`
	Port   interface{} `json:"port" yaml:"port"`
	Path   string      `json:"path" yaml:"path"`

	APIKey           string `json:"api_key" yaml:"api_key"`
	APIKeyCamel      string `json:"apiKey" yaml:"apiKey"`
	XAPIKey          string `json:"x_api_key" yaml:"x_api_key"`
	XAPIKeyCamel     string `json:"xApiKey" yaml:"xApiKey"`
	Token            string `json:"token" yaml:"token"`
	APIToken         string `json:"apiToken" yaml:"apiToken"`
	Bearer           string `json:"bearer" yaml:"bearer"`
	BearerToken      string `json:"bearer_token" yaml:"bearer_token"`
	BearerTokenCamel string `json:"bearerToken" yaml:"bearerToken"`
	AuthHeader       string `json:"auth_header" yaml:"auth_header"`
	Authorization    string `json:"authorization" yaml:"authorization"`

	Insecure      bool `json:"insecure" yaml:"insecure"`
	AllowInsecure bool `json:"allow_insecure" yaml:"allow_insecure"`
}

// PluginConf is one configured plugin instance. Config deliberately stays
// untyped: every plugin owns and validates the schema of its own config block.
//
// Source values currently accepted by appconfig are:
//   - "builtin"              — compiled into xraytool;
//   - "internal:<path>"      — future locally linked internal plugin;
//   - "external" + Exec      — future subprocess plugin;
//   - "external:<executable>" — shorthand subprocess plugin source.
//
// Pluginhost is responsible for implementing non-builtin sources in later
// phases; appconfig only validates the declarative contract here.
type PluginConf struct {
	Enabled bool     `yaml:"enabled"`
	Source  string   `yaml:"source"`
	Exec    string   `yaml:"exec"`
	Args    []string `yaml:"args"`
	// Manifest is an optional plugin.yaml path. For external plugins it enables
	// offline metadata and JSON Schema validation; relative paths are resolved
	// relative to the main xraytool configuration file.
	Manifest string `yaml:"manifest"`
	// LogPath optionally overrides the persistent stdout/stderr log file for
	// an external plugin. When empty the Host uses its platform cache path (or
	// XRAYTOOL_PLUGIN_LOG_DIR); built-in plugins ignore this field.
	LogPath       string         `yaml:"log_path"`
	Config        map[string]any `yaml:"config"`
	RestartPolicy RestartPolicy  `yaml:"restart_policy"`
}

// RestartPolicy describes the desired retry behaviour for an external plugin.
// It is declarative for now; lifecycle enforcement belongs to pluginhost.
type RestartPolicy struct {
	MaxRestarts int           `yaml:"max_restarts"`
	Backoff     time.Duration `yaml:"backoff"`
}

// PluginsConf is the top-level plugins: section. Keys are plugin names, for
// example "core", "antifraud", or "payment_platega".
type PluginsConf map[string]PluginConf

// EnginesConf is the top-level engines: section. Engine entries are inline so
// YAML follows the planned ergonomic form:
//
//	engines:
//	  routing_mode: broadcast
//	  xray:
//	    enabled: true
//	    source: builtin
//
// The inline map allows new engines to be added without changing appconfig.
type EnginesConf struct {
	RoutingMode string                `yaml:"routing_mode"`
	Entries     map[string]PluginConf `yaml:",inline"`
}

// EngineConf selects the VPN proxy engine implementation.
// Setting type to an empty string or "xray" uses the default Xray-core adapter.
// Future values: "singbox", "mihomo", etc.
type EngineConf struct {
	// Type is the engine identifier: "xray" (default), "singbox", …
	Type string `yaml:"type"`
}

// WorkerConf holds background worker settings.
type WorkerConf struct {
	Enabled            bool     `yaml:"enabled"`              // defaults to true
	ExpiryInterval     string   `yaml:"expiry_interval"`      // e.g., "5m"
	SyncStatesInterval string   `yaml:"sync_states_interval"` // e.g., "3m"
	ExpirationWarnings []string `yaml:"expiration_warnings"`  // e.g., ["72h", "24h", "3h", "1h"]
}

// DummyConfigsConf holds custom text arrays for error dummy profiles.
type DummyConfigsConf struct {
	Expired           []string `yaml:"expired"`
	DeviceLimit       []string `yaml:"device_limit"`
	UnsupportedClient []string `yaml:"unsupported_client"`
	AntiFraud         []string `yaml:"anti_fraud"`
}

// AntiFraudConf holds the configuration for the multi-IP anti-fraud system.
type AntiFraudConf struct {
	// Enabled toggles the anti-fraud module on or off.
	Enabled bool `yaml:"enabled"`
	// DryRun enables log-only mode. If true, detects fraud but does NOT apply bans.
	DryRun bool `yaml:"dry_run"`
	// LogPath is the path to the Xray access log file in tmpfs (e.g. /dev/shm/xray-access.log).
	// The file MUST reside in a RAM-backed filesystem to avoid disk I/O overhead.
	LogPath string `yaml:"log_path"`
	// MaxIPs is the maximum number of unique IP addresses allowed per user within IPLimitTTL.
	// Recommended: 3 (accounts for mobile network switching, dual-stack IPv4+IPv6, CGNAT).
	MaxIPs int `yaml:"max_ips"`
	// IPLimitTTL is the sliding window duration for IP activity tracking.
	// An IP is considered active for this duration after the last observed connection.
	IPLimitTTL string `yaml:"ip_limit_ttl"`
	// BanDuration is the duration of the soft ban applied when fraud is detected.
	// During the ban, the user is removed from Xray memory only; the config on disk is untouched.
	BanDuration string `yaml:"ban_duration"`
	// LogRotationSizeMB is the file size threshold (in MB) that triggers log rotation.
	// LogRotationSizeMB sets the max size of the access log in Megabytes before it is rotated.
	LogRotationSizeMB int `yaml:"log_rotation_size_mb"`
	// LogRotationMaxAge sets the maximum time a log file can exist before rotation (e.g. "5m").
	LogRotationMaxAge string `yaml:"log_rotation_max_age"`
	// ReportToMaster must be true on Slave nodes to enable sending batched IPs to Master.
	// When true, the slave batches observed IP events every 5s and sends them to master
	// so that master has a global view across all nodes for fraud detection.
	// The master node must have anti_fraud.enabled: true.
	ReportToMaster bool `yaml:"report_to_master"`
	// SaltSecret is an optional shared secret between Master and Slaves to ensure
	// deterministic IP hashing when Master and Slaves use different auth API keys.
	SaltSecret string `yaml:"salt_secret"`
}

// MailerConf holds configuration for transactional email delivery via Resend.
type MailerConf struct {
	// Enabled toggles email delivery. When false, codes are only logged (debug mode).
	Enabled bool `yaml:"enabled"`
	// ResendAPIKey is the Resend.com API key (starts with "re_").
	ResendAPIKey string `yaml:"resend_api_key"`
	// FromEmail is the verified sender address, e.g. "noreply@tvaldsforge.online".
	FromEmail string `yaml:"from_email"`
}

// SubscriptionConf holds subscription endpoint settings.
type SubscriptionConf struct {
	UserAgentWhitelist []string         `yaml:"user_agent_whitelist"`
	UserAgentNoChecks  []string         `yaml:"user_agent_no_checks"`
	DummyConfigs       DummyConfigsConf `yaml:"dummy_configs"`
}

// DatabaseConf holds database connection settings.
// Supports Postgres (production) and SQLite (lightweight/fallback).
type DatabaseConf struct {
	// DSN is the full Postgres connection string.
	// Example: postgres://user:pass@localhost:5432/xraytool?sslmode=disable
	DSN string `yaml:"dsn"`
	// Driver selects the database backend: "postgres" or "sqlite".
	// Defaults to "postgres".
	Driver string `yaml:"driver"`
	// SQLitePath is the file path used when Driver is "sqlite".
	// Defaults to /etc/xraytool/xraytool.db.
	SQLitePath string `yaml:"sqlite_path"`
}

// ServerConf holds server identity information.
type ServerConf struct {
	IP          string   `yaml:"ip"`
	Domain      string   `yaml:"domain"`
	APIKey      string   `yaml:"api_key"`
	AllowedDirs []string `yaml:"allowed_dirs"`
}

// PortsConf holds the configurable ports used by xraytool.
type PortsConf struct {
	APIServer int `yaml:"api_server"` // default: 8080
}

// PathsConf holds all filesystem paths used by xraytool.
type PathsConf struct {
	XrayConfig    string `yaml:"xray_config"`
	XrayTemplate  string `yaml:"xray_template"`
	StatsState    string `yaml:"stats_state"`
	InferredStats string `yaml:"inferred_stats"`
	// JSONSubscriptionTemplate is the path to the main (all-protocols) subscription txt file.
	// YAML key: json_subscription_template (legacy: subscription_template).
	JSONSubscriptionTemplate string `yaml:"json_subscription_template"`
	// VlessSubscriptionTemplate is the path to the VLESS-only subscription txt file.
	// Deprecated: VLESS subscriptions are now dynamically generated from the JSON subscription.
	// YAML key: vless_subscription_template (legacy: subscription_vless_template).
	VlessSubscriptionTemplate string `yaml:"vless_subscription_template"`
	RoutingTemplate           string `yaml:"routing_template"`
	RoutingRUTemplate         string `yaml:"routing_ru_template"`
	Hy2ConfigYAML             string `yaml:"hy2_config_yaml"`
	GeoIPDat                  string `yaml:"geoip_dat"`
	GeositeDat                string `yaml:"geosite_dat"`

	// Legacy keys — populated during Load() for backward compatibility.
	// Prefer using JSONSubscriptionTemplate and VlessSubscriptionTemplate.
	LegacySubscriptionTemplate      string `yaml:"subscription_template"`
	LegacySubscriptionVlessTemplate string `yaml:"subscription_vless_template"`
}

// XrayConf holds xray-core related settings.
type XrayConf struct {
	APIAddr string `yaml:"api_addr"`
}

// RealityConf holds Reality key rotation settings.
type RealityConf struct {
	RotationEnabled bool   `yaml:"rotation_enabled"`
	KeysFilepath    string `yaml:"keys_filepath"`
}

// MasterAPIConf defines how a slave node authenticates and connects to the master.
type MasterAPIConf struct {
	URL      string `yaml:"url"`
	APIKey   string `yaml:"api_key"`
	Insecure bool   `yaml:"insecure"`
}

// StatsConf holds traffic statistics settings.
type StatsConf struct {
	BucketSeconds         int `yaml:"bucket_seconds"`
	DetailedRetentionDays int `yaml:"detailed_retention_days"`
}

// SlaveAPIConf holds HTTP client settings for slave communication.
type SlaveAPIConf struct {
	ConnectTimeout time.Duration `yaml:"connect_timeout"`
	RequestTimeout time.Duration `yaml:"request_timeout"`
	RemotePath     string        `yaml:"remote_path"`
}

// LoggingConf holds configuration for structured logging.
type LoggingConf struct {
	Level    string `yaml:"level"`     // "debug", "info", "warn", "error"
	FilePath string `yaml:"file_path"` // path to write logs, empty for stdout only
	Format   string `yaml:"format"`    // "json" or "console"
}

// defaults returns a Config with sane default values.
func defaults() *Config {
	cfg := &Config{
		Mode: "master",
		Engine: EngineConf{
			Type: "xray",
		},
		Server: ServerConf{
			Domain: "yourdomain.tld",
			APIKey: "CHANGE_ME_IN_CONFIG",
			AllowedDirs: []string{
				"/etc/xraytool",
				"/var/www/TorvaldsVPN",
				"/var/log/xray",
			},
		},
		Paths: PathsConf{
			XrayConfig:               "/usr/local/etc/xray/config.json",
			XrayTemplate:             "/etc/xraytool/xray_template.json",
			StatsState:               "/etc/xraytool/traffic_stats_state.json",
			InferredStats:            "/etc/xraytool/inferred_traffic.json",
			JSONSubscriptionTemplate: "/etc/xraytool/configs.txt",
			RoutingTemplate:          "/etc/xraytool/routing.json",
			RoutingRUTemplate:        "/etc/xraytool/routing_ALL_RU.json",
			Hy2ConfigYAML:            "/etc/hysteria/config.yaml",
			GeoIPDat:                 "/usr/local/share/xray/geoip.dat",
			GeositeDat:               "/usr/local/share/xray/geosite.dat",
		},
		Xray: XrayConf{
			APIAddr: "127.0.0.1:10085",
		},
		Stats: StatsConf{
			BucketSeconds:         60,
			DetailedRetentionDays: 2,
		},
		SlaveAPI: SlaveAPIConf{
			ConnectTimeout: 5 * time.Second,
			RequestTimeout: 30 * time.Second,
			RemotePath:     "/api/v1/internal/xray/sync",
		},
		MasterAPI: MasterAPIConf{
			URL:      "",
			APIKey:   "CHANGE_ME_IN_CONFIG",
			Insecure: false,
		},
		Ports: PortsConf{
			APIServer: 8080,
		},
		Logging: LoggingConf{
			Level:    "info",
			FilePath: "",
			Format:   "console",
		},
		Webhooks: []string{},
		Worker: WorkerConf{
			Enabled:            true,
			ExpiryInterval:     "5m",
			SyncStatesInterval: "3m",
			ExpirationWarnings: []string{"72h", "24h", "3h", "1h"},
		},
		Reality: RealityConf{
			RotationEnabled: false,
			KeysFilepath:    "/etc/xraytool/configs/reality.keys",
		},
		Database: DatabaseConf{
			Driver:     "sqlite",
			SQLitePath: "/etc/xraytool/xraytool.db",
		},
		Subscription: SubscriptionConf{
			UserAgentWhitelist: []string{"happ", "incy", "megasupersecretua", "v2ray"},
			UserAgentNoChecks:  []string{"megasupersecretua", "v2ray"},
			DummyConfigs: DummyConfigsConf{
				Expired: []string{
					"🛑 ПОДПИСКА ЗАКОНЧИЛАСЬ 🛑",
					"Пожалуйста, продлите её,",
					"Чтобы вернуть доступ к сети:",
					"👉 @torvaldsvpnbot",
				},
				DeviceLimit: []string{
					"🛑 Лимит устройств 🛑",
					"Удалите старые устройства",
					"Или расширьте свой лимит",
					"В нашем боте: @torvaldsvpnbot",
				},
				UnsupportedClient: []string{
					"🛑 Приложение не поддерживается 🛑",
					"Клиент не отправил HWID",
					"Поддержка -> @torvaldsvpnbot",
				},
				AntiFraud: []string{
					"⚠️ АНТИФРОД СИСТЕМА ⚠️",
					"Обнаружено подозрительное подключение",
					"Доступ временно ограничен на 10 минут",
					"👉 @torvaldsvpnbot",
				},
			},
		},
		AntiFraud: AntiFraudConf{
			Enabled:           false,
			DryRun:            true,
			LogPath:           "/dev/shm/xray-access.log",
			MaxIPs:            3,
			IPLimitTTL:        "3m",
			BanDuration:       "10m",
			LogRotationSizeMB: 20,
		},
		Mailer: MailerConf{
			Enabled:      false,
			ResendAPIKey: "",
			FromEmail:    "",
		},
	}

	// These depend on values above, so construct them after the base config.
	// Load() repeats this materialisation after YAML and field defaults have been
	// applied in order to preserve legacy configuration values.
	cfg.Plugins = defaultPluginConfigs(cfg)
	cfg.Engines = defaultEngineConfigs(cfg)
	return cfg
}

// defaultPluginConfigs materialises the plugin configuration that corresponds
// to the legacy top-level settings. Keeping this translation here means a
// config written before the plugin refactor continues to have the same runtime
// behaviour once callers switch from the legacy fields to Config.Plugins.
func defaultPluginConfigs(cfg *Config) PluginsConf {
	return PluginsConf{
		"core": {
			Enabled: true,
			Source:  "builtin",
			Config: map[string]any{
				"device_limit_default": 3,
			},
		},
		// Pricing is a mandatory business capability even though it is a
		// separately replaceable plugin. The default implementation is pure and
		// is injected into core's payment service after the host has loaded.
		"pricing_default": {
			Enabled: true,
			Source:  "builtin",
			Config:  map[string]any{},
		},
		// Preserve the legacy Platega credentials as a declarative optional
		// provider. It stays disabled until both values are configured, so a
		// single-node installation without online payments does not load it.
		"payment_platega": {
			Enabled: cfg.PlategaMerchantID != "" && cfg.PlategaSecret != "",
			Source:  "builtin",
			Config: map[string]any{
				"merchant_id": cfg.PlategaMerchantID,
				"secret":      cfg.PlategaSecret,
				"currency":    "RUB",
			},
		},
		"billing": {
			Enabled: true,
			Source:  "builtin",
			Config:  map[string]any{},
		},
		"promo": {
			Enabled: true,
			Source:  "builtin",
			Config:  map[string]any{},
		},
		"referral": {
			Enabled: true,
			Source:  "builtin",
			Config:  map[string]any{},
		},
		// Keep the current JSON traffic-state behaviour behind a replaceable
		// provider. A deployment can disable this entry and enable another
		// traffic plugin that publishes the same services.
		"traffic_file": {
			Enabled: true,
			Source:  "builtin",
			Config:  map[string]any{},
		},
		"subscription_lifecycle": {
			Enabled: true,
			Source:  "builtin",
			Config:  map[string]any{},
		},
		"subscription_format_legacy": {
			Enabled: true,
			Source:  "builtin",
			Config:  map[string]any{},
		},
		"config_storage": {
			Enabled: true,
			Source:  "builtin",
			Config:  map[string]any{},
		},
		"identity_memory": {
			Enabled: true,
			Source:  "builtin",
			Config:  map[string]any{},
		},
		"support_chat": {
			// Support chat requires a deployment-specific encryption key. It is
			// deliberately opt-in so a new installation never starts with a
			// shared development key.
			Enabled: false,
			Source:  "builtin",
			Config:  map[string]any{},
		},
		"antifraud": {
			Enabled: cfg.AntiFraud.Enabled,
			Source:  "builtin",
			Config: map[string]any{
				"enabled":              cfg.AntiFraud.Enabled,
				"dry_run":              cfg.AntiFraud.DryRun,
				"log_path":             cfg.AntiFraud.LogPath,
				"max_ips":              cfg.AntiFraud.MaxIPs,
				"ip_limit_ttl":         cfg.AntiFraud.IPLimitTTL,
				"ban_duration":         cfg.AntiFraud.BanDuration,
				"log_rotation_size_mb": cfg.AntiFraud.LogRotationSizeMB,
				"log_rotation_max_age": cfg.AntiFraud.LogRotationMaxAge,
				"report_to_master":     cfg.AntiFraud.ReportToMaster,
				"salt_secret":          cfg.AntiFraud.SaltSecret,
				"is_master":            cfg.IsMaster(),
			},
		},
		"mailer_resend": {
			Enabled: cfg.Mailer.Enabled && cfg.Mailer.ResendAPIKey != "",
			Source:  "builtin",
			Config: map[string]any{
				"resend_api_key": cfg.Mailer.ResendAPIKey,
				"from_email":     cfg.Mailer.FromEmail,
			},
		},
		"eventsink_webhook": {
			Enabled: len(cfg.Webhooks) > 0,
			Source:  "builtin",
			Config: map[string]any{
				"webhooks":       stringsToAny(cfg.Webhooks),
				"webhook_secret": cfg.WebhookSecret,
			},
		},
		"cluster_sync": {
			Enabled: cfg.IsMaster() && len(cfg.SlaveServers) > 0,
			Source:  "builtin",
			Config: map[string]any{
				"sync_interval": cfg.Worker.SyncStatesInterval,
				"sync_on_start": true,
			},
		},
	}
}

// defaultEngineConfigs materialises the planned engines: section from the
// existing single-Xray fields. The legacy engine.type switch currently falls
// back to Xray for every value, so the compatible default remains Xray.
func defaultEngineConfigs(cfg *Config) EnginesConf {
	return EnginesConf{
		RoutingMode: "broadcast",
		Entries: map[string]PluginConf{
			"xray": {
				Enabled: true,
				Source:  "builtin",
				Config: map[string]any{
					"grpc_addr":          cfg.Xray.APIAddr,
					"config_path":        cfg.Paths.XrayConfig,
					"template_path":      cfg.Paths.XrayTemplate,
					"server_address":     cfg.Server.IP,
					"hy2_config_yaml":    cfg.Paths.Hy2ConfigYAML,
					"reality_rotation":   cfg.Reality.RotationEnabled,
					"reality_keys_path":  cfg.Reality.KeysFilepath,
					"blacklisted_admins": stringsToAny(cfg.BlacklistedAdmins),
				},
			},
		},
	}
}

func stringsToAny(values []string) []any {
	out := make([]any, len(values))
	for i, value := range values {
		out[i] = value
	}
	return out
}

// configPresence records which settings appeared explicitly in YAML. Bool
// fields need this extra information because false is both a legitimate value
// and Go's zero value; without it a legacy default could overwrite an explicit
// `enabled: false` during compatibility materialisation.
type configPresence struct {
	plugins map[string]pluginEntryPresence
	engines map[string]pluginEntryPresence
}

type pluginEntryPresence struct {
	exists     bool
	enabled    bool
	source     bool
	config     bool
	configKeys map[string]bool
}

func readPluginConfigPresence(data []byte) (configPresence, error) {
	presence := configPresence{
		plugins: make(map[string]pluginEntryPresence),
		engines: make(map[string]pluginEntryPresence),
	}

	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return configPresence{}, err
	}
	if len(document.Content) == 0 || document.Content[0].Kind != yaml.MappingNode {
		return presence, nil
	}

	root := document.Content[0]
	if plugins := mappingValue(root, "plugins"); plugins != nil && plugins.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(plugins.Content); i += 2 {
			name := plugins.Content[i].Value
			presence.plugins[name] = readPluginEntryPresence(plugins.Content[i+1])
		}
	}
	if engines := mappingValue(root, "engines"); engines != nil && engines.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(engines.Content); i += 2 {
			name := engines.Content[i].Value
			if name == "routing_mode" {
				continue
			}
			presence.engines[name] = readPluginEntryPresence(engines.Content[i+1])
		}
	}

	return presence, nil
}

func mappingValue(node *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

func readPluginEntryPresence(node *yaml.Node) pluginEntryPresence {
	presence := pluginEntryPresence{exists: true, configKeys: make(map[string]bool)}
	if node == nil || node.Kind != yaml.MappingNode {
		return presence
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i].Value
		switch key {
		case "enabled":
			presence.enabled = true
		case "source":
			presence.source = true
		case "config":
			presence.config = true
			config := node.Content[i+1]
			if config.Kind != yaml.MappingNode {
				continue
			}
			for j := 0; j+1 < len(config.Content); j += 2 {
				presence.configKeys[config.Content[j].Value] = true
			}
		}
	}
	return presence
}

// materializePluginConfiguration fills known plugin and engine entries with
// safe defaults, then overlays legacy values only where new YAML did not make
// an explicit choice. Unknown entries are preserved untouched for future or
// external plugins.
func materializePluginConfiguration(cfg *Config, presence configPresence) {
	pluginDefaults := defaultPluginConfigs(cfg)
	if cfg.Plugins == nil {
		cfg.Plugins = make(PluginsConf)
	}
	for name, fallback := range pluginDefaults {
		entry, exists := cfg.Plugins[name]
		entryPresence := presence.plugins[name]
		if !exists || !entryPresence.exists {
			cfg.Plugins[name] = fallback
			continue
		}
		cfg.Plugins[name] = mergePluginDefaults(entry, fallback, entryPresence)
	}

	engineDefaults := defaultEngineConfigs(cfg)
	if cfg.Engines.Entries == nil {
		cfg.Engines.Entries = make(map[string]PluginConf)
	}
	if cfg.Engines.RoutingMode == "" {
		cfg.Engines.RoutingMode = engineDefaults.RoutingMode
	}
	for name, fallback := range engineDefaults.Entries {
		entry, exists := cfg.Engines.Entries[name]
		entryPresence := presence.engines[name]
		if !exists || !entryPresence.exists {
			cfg.Engines.Entries[name] = fallback
			continue
		}
		cfg.Engines.Entries[name] = mergePluginDefaults(entry, fallback, entryPresence)
	}
}

// applyXrayEngineCompatibilityOverrides keeps legacy consumers on the same
// effective Xray adapter settings as the declarative engines.xray plugin.
// During the migration cmd/root.go still creates the shared adapter before the
// plugin host starts, so accepting an engines.xray override must not silently
// leave that adapter on stale top-level values.
func applyXrayEngineCompatibilityOverrides(cfg *Config) {
	if cfg == nil || cfg.Engines.Entries == nil {
		return
	}
	entry, ok := cfg.Engines.Entries["xray"]
	if !ok || entry.Config == nil {
		return
	}
	if value := rawConfigString(entry.Config, "grpc_addr"); value != "" {
		cfg.Xray.APIAddr = value
	}
	if value := rawConfigString(entry.Config, "config_path"); value != "" {
		cfg.Paths.XrayConfig = value
	}
	if value := rawConfigString(entry.Config, "template_path"); value != "" {
		cfg.Paths.XrayTemplate = value
	}
	if value := rawConfigString(entry.Config, "hy2_config_yaml"); value != "" {
		cfg.Paths.Hy2ConfigYAML = value
	}
	if value, ok := entry.Config["reality_rotation"].(bool); ok {
		cfg.Reality.RotationEnabled = value
	}
	if value := rawConfigString(entry.Config, "reality_keys_path"); value != "" {
		cfg.Reality.KeysFilepath = value
	}
	if values, ok := rawConfigStrings(entry.Config, "blacklisted_admins"); ok {
		cfg.BlacklistedAdmins = values
	}
}

func rawConfigString(config map[string]any, key string) string {
	value, _ := config[key].(string)
	return strings.TrimSpace(value)
}

func rawConfigStrings(config map[string]any, key string) ([]string, bool) {
	value, exists := config[key]
	if !exists {
		return nil, false
	}
	switch values := value.(type) {
	case []string:
		return append([]string(nil), values...), true
	case []any:
		result := make([]string, 0, len(values))
		for _, value := range values {
			text, ok := value.(string)
			if !ok {
				continue
			}
			result = append(result, text)
		}
		return result, true
	default:
		return nil, false
	}
}

func mergePluginDefaults(entry, fallback PluginConf, presence pluginEntryPresence) PluginConf {
	if !presence.enabled {
		entry.Enabled = fallback.Enabled
	}
	if !presence.source {
		entry.Source = fallback.Source
	}
	if !presence.config {
		return withDefaultRawConfig(entry, fallback.Config)
	}
	entry.Config = mergeRawConfigDefaults(entry.Config, fallback.Config, presence.configKeys)
	return entry
}

func withDefaultRawConfig(entry PluginConf, defaults map[string]any) PluginConf {
	if len(defaults) == 0 {
		entry.Config = nil
		return entry
	}
	entry.Config = make(map[string]any, len(defaults))
	for key, value := range defaults {
		entry.Config[key] = value
	}
	return entry
}

func mergeRawConfigDefaults(config, defaults map[string]any, explicitlySet map[string]bool) map[string]any {
	if len(defaults) == 0 {
		return config
	}
	if config == nil {
		config = make(map[string]any, len(defaults))
	}
	for key, value := range defaults {
		if explicitlySet[key] {
			continue
		}
		if _, exists := config[key]; !exists {
			config[key] = value
		}
	}
	return config
}

const defaultConfigYAML = `# ============================================================
# xraytool configuration
# Copy to /etc/xraytool/config.yaml and edit.
# Run with: xraytool --config /etc/xraytool/config.yaml <command>
# ============================================================

# Mode: "master" (manages slaves) or "slave" (executes commands from master)
mode: master

# Legacy single-engine selector. It remains supported while the engines:
# section below is adopted; current values still resolve to the Xray engine.
engine:
  # VPN proxy engine type: "xray" (default)
  type: "xray"

# Secret used to verify Platega webhooks and API calls between backend and bot
platega_merchant_id: "your_merchant_id_here"
platega_secret: "your_secret_here"

# Secret used to sign outgoing webhooks sent to clients-tg-go
webhook_secret: "your_webhook_secret_here"

# Reality key and Short ID rotation settings
reality:
  # Automatically generate and rotate Reality keys and Short IDs on the Master node.
  # Set to false on Slave nodes.
  rotation_enabled: false
  # Path to store the generated JSON keys file.
  keys_filepath: "/etc/xraytool/configs/reality.keys"

# Hardcoded admins from template to exclude/disable from being sync'd/added to config.json
blacklisted_admins:
  # - "bazhon"
  # - "admin2"

server:
  # Public IP of this server (informational, used in some outputs)
  ip: "1.2.3.4"
  # Domain for subscription links: https://<domain>/client?id=<subfile>
  domain: "yourdomain.tld"
  # Authentication key for INCOMING API requests to this server.
  # If mode=master, this protects master's admin API.
  # If mode=slave, this authenticates incoming commands from the master.
  api_key: "CHANGE_ME_IN_CONFIG"
  # Directories allowed for the /api/download and /api/upload endpoints
  allowed_dirs:
    - "/etc/xraytool"
    - "/var/www/TorvaldsVPN"
    - "/var/log/xray"

paths:
  # Xray-core main config
  xray_config: "/usr/local/etc/xray/config.json"

  # Xray config template (structural skeleton with static clients)
  # xraytool regenerates xray_config from this template + DB users on startup.
  # Leave empty to disable template-based generation (uses xray_config directly).
  xray_template: "/etc/xraytool/xray_template.json"

  # JSON file storing cumulative traffic stats state
  stats_state: "/etc/xraytool/traffic_stats_state.json"

  # JSON file for tracking dynamic inferred stats
  inferred_stats: "/etc/xraytool/inferred_traffic.json"

  # Main subscription config template (configs.txt)
  json_subscription_template: "/etc/xraytool/configs.txt"

  # Routing templates used to replace placeholders in subscriptions
  routing_template: "/etc/xraytool/routing.json"
  routing_ru_template: "/etc/xraytool/routing_ALL_RU.json"

  # Hysteria 2 configuration file
  hy2_config_yaml: "/etc/hysteria/config.yaml"

  # Xray GeoIP and Geosite database paths
  geoip_dat: "/usr/local/share/xray/geoip.dat"
  geosite_dat: "/usr/local/share/xray/geosite.dat"

xray:
  # Address of the xray gRPC API (set in config.json api section)
  api_addr: "127.0.0.1:10085"

# Plugin host configuration. Every enabled plugin has an explicit source.
# Omitted optional plugin entries are disabled by default. During the migration,
# legacy anti_fraud, mailer and webhooks settings below populate their matching
# plugin configs unless an explicit plugins.<name>.config value is supplied.
plugins:
  core:
    enabled: true
    source: builtin

# Multiple VPN engines are loaded through the same Plugin Host. The legacy
# xray/paths/reality settings above provide defaults for engines.xray.config;
# add config keys here only when overriding those values.
engines:
  routing_mode: broadcast
  xray:
    enabled: true
    source: builtin

stats:
  # Time resolution for traffic buckets (seconds)
  bucket_seconds: 60
  # How many days to keep detailed per-bucket data (older data is archived/summed)
  detailed_retention_days: 2

slave_api:
  # Timeout for connecting to a slave server
  connect_timeout: 4s
  # Total timeout for a slave API request
  request_timeout: 15s
  # Default API path on slave servers (can be overridden per-server in servers.json)
  remote_path: "/api/rest/xraytool"

# ============================================================
# Master Node Access (used ONLY when mode=slave)
# ============================================================
master_api:
  # Master URL (used by slave to send events/stats back)
  url: "https://master.domain.com/api/v1/internal/xray/sync"
  # Authentication key used to connect TO the master (must match a key in master's slave_servers OR master's server.api_key)
  api_key: "your_master_or_slave_secret_key"
  # Ignore self-signed certificates when connecting to master
  insecure: false

ports:
  # Port for the REST API server (api-server / start-server)
  api_server: 8080

logging:
  # Logging level: "debug", "info", "warn", "error" (default: "info")
  level: "info"
  # Log file path (leave empty for stdout/console only)
  file_path: "/var/log/xraytool/xraytool.log"
  # Log format: "json" (production) or "console" (development)
  format: "console"

webhooks:
  # List of URLs to receive JSON event notifications (webhooks)
  # - "http://127.0.0.1:8081/api/v1/notify"

database:
  # Database driver: "sqlite" (production) or "sqlite" (lightweight/fallback)
  driver: "sqlite"
  # Full Postgres DSN (used when driver=postgres)
  # dsn: "postgres://user:pass@localhost:5432/xraytool?sslmode=disable"
  dsn: ""
  # File path used when driver=sqlite
  sqlite_path: "/etc/xraytool/xraytool.db"

subscription:
  # Allowed User-Agents to access the subscription
  user_agent_whitelist:
    - "happ"
    - "incy"
    - "megasupersecretua"
    - "v2ray"
  # User-Agents that bypass device limit and HWID checks
  user_agent_no_checks:
    - "megasupersecretua"
    - "v2ray"
  # Custom VLESS proxy dummy texts for subscription errors
  dummy_configs:
    expired:
      - "🛑 ПОДПИСКА ЗАКОНЧИЛАСЬ 🛑"
      - "Пожалуйста, продлите её,"
      - "Чтобы вернуть доступ к сети"
      - "👉 @torvaldsvpnbot"
    device_limit:
      - "🛑 Лимит устройств 🛑"
      - "Удалите старые устройства"
      - "Или расширьте свой лимит"
      - "👉 @torvaldsvpnbot"
    unsupported_client:
      - "🛑 Приложение не поддерживается 🛑"
      - "Клиент не отправил HWID"
      - "👉 @torvaldsvpnbot"
    anti_fraud:
      - "⚠️ АНТИФРОД СИСТЕМА ⚠️"
      - "Обнаружено подозрительное подключение"
      - "Доступ временно ограничен на 10 минут"
      - "👉 @torvaldsvpnbot"

worker:
  # Enable or disable the background expiry worker
  enabled: true
  # How often the worker checks the database for expired users and warnings
  expiry_interval: "5m"
  # How often the master synchronizes states to all slaves
  sync_states_interval: "3m"
  # Thresholds for expiration warnings sent to the client app
  expiration_warnings:
    - "72h"
    - "24h"
    - "3h"
    - "1h"

anti_fraud:
  # Enable or disable the Anti-Fraud module
  enabled: false
  # If true, detect fraud and log it, but do NOT apply bans or delete users
  dry_run: true
  # Path to the Xray access log to parse
  log_path: "/dev/shm/xray-access.log"
  # Maximum allowed unique IPs within the TTL window
  max_ips: 3
  # Time window to track IPs for a single user
  ip_limit_ttl: "3m"
  # How long the user should be banned
  ban_duration: "10m"
  # Max size of the log file before it is automatically rotated (in MB)
  log_rotation_size_mb: 20
  # Max age of log file before it is automatically rotated (e.g. "5m")
  log_rotation_max_age: "5m"
  # (slave only) If true, forward observed IP events to master every 5s for global fraud detection.
  # Master must have anti_fraud.enabled: true.
  report_to_master: false
  # Salt secret used to hash IPs before reporting to master (ensures privacy)
  salt_secret: "some_shared_salt_here"

mailer:
  # Enable transactional email delivery (OTP codes via Resend.com).
  enabled: false
  # Resend API key — create one at https://resend.com/api-keys
  resend_api_key: ""
  # Verified sender address (must match the domain verified in Resend)
  from_email: "noreply@yourdomain.tld"

slave_servers:
  # slave-1:
  #   url: "https://slave.example.com/api/v1/internal/xray/sync"
  #   api_key: "slave_1_unique_secret" # sent as X-API-Key header to the slave
  #   insecure: false                  # skip TLS verification
`

// Load reads and parses the config file at the given path.
// If the config file does not exist, it creates it with the default pattern.
// Missing fields fall back to sensible defaults.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			dir := filepath.Dir(path)
			if err := os.MkdirAll(dir, 0755); err != nil {
				return nil, fmt.Errorf("creating config directory %q: %w", dir, err)
			}
			if err := os.WriteFile(path, []byte(defaultConfigYAML), 0600); err != nil {
				return nil, fmt.Errorf("creating default config %q: %w", path, err)
			}
			data = []byte(defaultConfigYAML)
		} else {
			return nil, fmt.Errorf("reading config %q: %w", path, err)
		}
	}

	cfg := defaults()
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config %q: %w", path, err)
	}
	presence, err := readPluginConfigPresence(data)
	if err != nil {
		return nil, fmt.Errorf("parsing config %q: %w", path, err)
	}

	// Apply field-level defaults for anything left empty after unmarshal.
	defs := defaults()
	if cfg.Paths.XrayConfig == "" {
		cfg.Paths.XrayConfig = defs.Paths.XrayConfig
	}
	if cfg.Paths.XrayTemplate == "" {
		cfg.Paths.XrayTemplate = defs.Paths.XrayTemplate
	}
	if cfg.Paths.StatsState == "" {
		cfg.Paths.StatsState = defs.Paths.StatsState
	}
	if cfg.Paths.InferredStats == "" {
		cfg.Paths.InferredStats = defs.Paths.InferredStats
	}
	// Backward compatibility: if old yaml keys were used, migrate them to new fields.
	if cfg.Paths.LegacySubscriptionTemplate != "" && cfg.Paths.JSONSubscriptionTemplate == "" {
		cfg.Paths.JSONSubscriptionTemplate = cfg.Paths.LegacySubscriptionTemplate
	}
	if cfg.Paths.LegacySubscriptionVlessTemplate != "" && cfg.Paths.VlessSubscriptionTemplate == "" {
		cfg.Paths.VlessSubscriptionTemplate = cfg.Paths.LegacySubscriptionVlessTemplate
	}
	if cfg.Paths.JSONSubscriptionTemplate == "" {
		cfg.Paths.JSONSubscriptionTemplate = defs.Paths.JSONSubscriptionTemplate
	}
	// VlessSubscriptionTemplate is deprecated and no longer has a fallback or default value.
	if cfg.Paths.RoutingTemplate == "" {
		cfg.Paths.RoutingTemplate = defs.Paths.RoutingTemplate
	}
	if cfg.Paths.RoutingRUTemplate == "" {
		cfg.Paths.RoutingRUTemplate = defs.Paths.RoutingRUTemplate
	}
	if cfg.Paths.Hy2ConfigYAML == "" {
		cfg.Paths.Hy2ConfigYAML = defs.Paths.Hy2ConfigYAML
	}
	if cfg.Paths.GeoIPDat == "" {
		cfg.Paths.GeoIPDat = defs.Paths.GeoIPDat
	}
	if cfg.Paths.GeositeDat == "" {
		cfg.Paths.GeositeDat = defs.Paths.GeositeDat
	}
	if cfg.Reality.KeysFilepath == "" {
		cfg.Reality.KeysFilepath = defs.Reality.KeysFilepath
	}
	if cfg.Xray.APIAddr == "" {
		cfg.Xray.APIAddr = defs.Xray.APIAddr
	}
	if cfg.Stats.BucketSeconds == 0 {
		cfg.Stats.BucketSeconds = defs.Stats.BucketSeconds
	}
	if cfg.Stats.DetailedRetentionDays == 0 {
		cfg.Stats.DetailedRetentionDays = defs.Stats.DetailedRetentionDays
	}
	if cfg.SlaveAPI.ConnectTimeout == 0 {
		cfg.SlaveAPI.ConnectTimeout = defs.SlaveAPI.ConnectTimeout
	}
	if cfg.SlaveAPI.RequestTimeout == 0 {
		cfg.SlaveAPI.RequestTimeout = defs.SlaveAPI.RequestTimeout
	}
	if cfg.SlaveAPI.RemotePath == "" {
		cfg.SlaveAPI.RemotePath = defs.SlaveAPI.RemotePath
	}
	if cfg.Ports.APIServer == 0 {
		cfg.Ports.APIServer = defs.Ports.APIServer
	}
	if cfg.Logging.Level == "" {
		cfg.Logging.Level = defs.Logging.Level
	}
	if cfg.Logging.Format == "" {
		cfg.Logging.Format = defs.Logging.Format
	}
	if cfg.Webhooks == nil {
		cfg.Webhooks = []string{}
	}
	if cfg.Database.Driver == "" {
		cfg.Database.Driver = defs.Database.Driver
	}
	if cfg.Database.SQLitePath == "" {
		cfg.Database.SQLitePath = defs.Database.SQLitePath
	}
	if len(cfg.Subscription.UserAgentWhitelist) == 0 {
		cfg.Subscription.UserAgentWhitelist = defs.Subscription.UserAgentWhitelist
	}
	if len(cfg.Subscription.UserAgentNoChecks) == 0 {
		cfg.Subscription.UserAgentNoChecks = defs.Subscription.UserAgentNoChecks
	}
	if cfg.Worker.ExpiryInterval == "" {
		cfg.Worker.ExpiryInterval = defs.Worker.ExpiryInterval
	}
	if cfg.Worker.SyncStatesInterval == "" {
		cfg.Worker.SyncStatesInterval = defs.Worker.SyncStatesInterval
	}
	if len(cfg.Worker.ExpirationWarnings) == 0 {
		cfg.Worker.ExpirationWarnings = defs.Worker.ExpirationWarnings
	}
	if len(cfg.Subscription.DummyConfigs.Expired) == 0 {
		cfg.Subscription.DummyConfigs.Expired = defs.Subscription.DummyConfigs.Expired
	}
	if len(cfg.Subscription.DummyConfigs.DeviceLimit) == 0 {
		cfg.Subscription.DummyConfigs.DeviceLimit = defs.Subscription.DummyConfigs.DeviceLimit
	}
	if len(cfg.Subscription.DummyConfigs.UnsupportedClient) == 0 {
		cfg.Subscription.DummyConfigs.UnsupportedClient = defs.Subscription.DummyConfigs.UnsupportedClient
	}
	if len(cfg.Subscription.DummyConfigs.AntiFraud) == 0 {
		cfg.Subscription.DummyConfigs.AntiFraud = defs.Subscription.DummyConfigs.AntiFraud
	}
	// AntiFraud defaults: only set if the block wasn't configured at all.
	if cfg.AntiFraud.LogPath == "" {
		cfg.AntiFraud.LogPath = defs.AntiFraud.LogPath
	}
	if cfg.AntiFraud.MaxIPs == 0 {
		cfg.AntiFraud.MaxIPs = defs.AntiFraud.MaxIPs
	}
	if cfg.AntiFraud.IPLimitTTL == "" {
		cfg.AntiFraud.IPLimitTTL = defs.AntiFraud.IPLimitTTL
	}
	if cfg.AntiFraud.BanDuration == "" {
		cfg.AntiFraud.BanDuration = defs.AntiFraud.BanDuration
	}
	if cfg.AntiFraud.LogRotationSizeMB == 0 {
		cfg.AntiFraud.LogRotationSizeMB = defs.AntiFraud.LogRotationSizeMB
	}
	if len(cfg.Server.AllowedDirs) == 0 {
		cfg.Server.AllowedDirs = defs.Server.AllowedDirs
	}

	// Materialise the new plugins:/engines: configuration only after all legacy
	// field defaults are known, so an existing YAML file without the new blocks
	// retains its previous effective runtime configuration.
	materializePluginConfiguration(cfg, presence)
	applyXrayEngineCompatibilityOverrides(cfg)
	cfg.configDir = filepath.Dir(path)

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	return cfg, nil
}

// IsMaster returns true when this node is configured as master.
func (c *Config) IsMaster() bool {
	return c.Mode == "master"
}

// DetailedRetentionSeconds returns the retention window in seconds.
func (c *Config) DetailedRetentionSeconds() int64 {
	return int64(c.Stats.DetailedRetentionDays) * 24 * 60 * 60
}

// Validate ensures critical configuration fields are present.
func (c *Config) Validate() error {
	if c.IsMaster() {
		if c.Server.Domain == "" {
			return fmt.Errorf("server.domain is required for master nodes")
		}
		if c.Database.DSN == "" && c.Database.Driver != "sqlite" {
			return fmt.Errorf("database.dsn is required for master nodes when not using sqlite")
		}
	}
	return c.validatePluginConfiguration()
}

func (c *Config) validatePluginConfiguration() error {
	core, ok := c.Plugins["core"]
	if !ok {
		return fmt.Errorf("plugins.core is required")
	}
	if !core.Enabled {
		return fmt.Errorf("plugins.core is mandatory and cannot be disabled")
	}
	if core.Source != "builtin" {
		return fmt.Errorf("plugins.core.source must be %q, got %q", "builtin", core.Source)
	}

	if err := validatePluginEntries("plugins", c.Plugins, c.configDir); err != nil {
		return err
	}
	if err := validatePluginEntries("engines", c.Engines.Entries, c.configDir); err != nil {
		return err
	}

	switch c.Engines.RoutingMode {
	case "broadcast", "by-plan", "by-subscription-override":
		// Valid modes are intentionally constrained here; routing behaviour is
		// implemented by pluginhost.MultiEngine.
	default:
		return fmt.Errorf("engines.routing_mode must be one of broadcast, by-plan, or by-subscription-override (got %q)", c.Engines.RoutingMode)
	}

	for _, entry := range c.Engines.Entries {
		if entry.Enabled {
			return nil
		}
	}
	return fmt.Errorf("at least one engine in engines must be enabled")
}

func validatePluginEntries(section string, entries map[string]PluginConf, configDir string) error {
	for name, entry := range entries {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("%s contains an empty plugin name", section)
		}
		if entry.RestartPolicy.MaxRestarts < 0 {
			return fmt.Errorf("%s.%s.restart_policy.max_restarts must not be negative", section, name)
		}
		if entry.RestartPolicy.Backoff < 0 {
			return fmt.Errorf("%s.%s.restart_policy.backoff must not be negative", section, name)
		}

		source := strings.TrimSpace(entry.Source)
		if source == "" {
			if entry.Enabled {
				return fmt.Errorf("%s.%s.source is required when enabled", section, name)
			}
			continue
		}

		switch {
		case source == "builtin":
			// No additional location is needed.
		case source == "external":
			if strings.TrimSpace(entry.Exec) == "" {
				return fmt.Errorf("%s.%s.exec is required when source is external", section, name)
			}
		case strings.HasPrefix(source, "internal:"):
			if strings.TrimSpace(strings.TrimPrefix(source, "internal:")) == "" {
				return fmt.Errorf("%s.%s.source must include a path after internal:", section, name)
			}
		case strings.HasPrefix(source, "external:"):
			if strings.TrimSpace(strings.TrimPrefix(source, "external:")) == "" {
				return fmt.Errorf("%s.%s.source must include an executable after external:", section, name)
			}
		default:
			return fmt.Errorf("%s.%s.source %q is unsupported", section, name, entry.Source)
		}

		if entry.Enabled {
			if err := validatePluginEntryConfig(section, name, entry, source, configDir); err != nil {
				return err
			}
		}
	}
	return nil
}

// validatePluginEntryConfig performs declarative validation before a plugin
// has a chance to start. Built-in schemas are embedded in the binary. External
// schemas are optional for compatibility, but when a manifest is explicitly
// configured (or shipped next to the executable) its config_schema is resolved
// relative to that manifest rather than the process working directory.
func validatePluginEntryConfig(section, name string, entry PluginConf, source, configDir string) error {
	if source == "builtin" {
		builtinName := name
		if section == "engines" && !strings.HasPrefix(builtinName, "engine_") {
			builtinName = "engine_" + builtinName
		}
		if err := pluginmanifest.ValidateBuiltinConfig(builtinName, entry.Config); err != nil {
			return fmt.Errorf("%s.%s.config: %w", section, name, err)
		}
		return nil
	}

	if source != "external" && !strings.HasPrefix(source, "external:") {
		return nil
	}

	manifestPath, explicit, err := externalPluginManifestPath(entry, source, configDir)
	if err != nil {
		return fmt.Errorf("%s.%s.manifest: %w", section, name, err)
	}
	if manifestPath == "" {
		return nil
	}
	if !explicit {
		if _, statErr := os.Stat(manifestPath); os.IsNotExist(statErr) {
			// Schema adoption is opt-in for existing third-party plugins. Once a
			// bundle ships plugin.yaml beside its executable, validation activates
			// automatically without changing the main configuration file.
			return nil
		} else if statErr != nil {
			return fmt.Errorf("%s.%s.manifest: inspect implicit manifest %q: %w", section, name, manifestPath, statErr)
		}
	}

	manifest, err := pluginmanifest.Load(manifestPath)
	if err != nil {
		return fmt.Errorf("%s.%s.manifest: %w", section, name, err)
	}
	if manifest.Type != "external" {
		return fmt.Errorf("%s.%s.manifest: manifest type must be external, got %q", section, name, manifest.Type)
	}
	expectedName := name
	if section == "engines" && !strings.HasPrefix(expectedName, "engine_") {
		expectedName = "engine_" + expectedName
	}
	if manifest.Name != expectedName {
		return fmt.Errorf("%s.%s.manifest: manifest declares name %q, want %q", section, name, manifest.Name, expectedName)
	}
	if err := pluginmanifest.ValidateConfigForManifest(manifestPath, *manifest, entry.Config); err != nil {
		return fmt.Errorf("%s.%s.config: %w", section, name, err)
	}
	return nil
}

// externalPluginManifestPath returns the manifest used for schema validation.
// An explicit `manifest:` path is mandatory once configured; otherwise a
// plugin.yaml adjacent to the executable is discovered opportunistically.
func externalPluginManifestPath(entry PluginConf, source, configDir string) (path string, explicit bool, err error) {
	ref := strings.TrimSpace(entry.Manifest)
	if ref != "" {
		return resolveConfigRelativePath(ref, configDir), true, nil
	}

	execPath := strings.TrimSpace(entry.Exec)
	if execPath == "" && strings.HasPrefix(source, "external:") {
		execPath = strings.TrimSpace(strings.TrimPrefix(source, "external:"))
	}
	if execPath == "" {
		return "", false, nil
	}
	execPath = resolveConfigRelativePath(execPath, configDir)
	return filepath.Join(filepath.Dir(execPath), "plugin.yaml"), false, nil
}

func resolveConfigRelativePath(path, configDir string) string {
	if filepath.IsAbs(path) || strings.TrimSpace(configDir) == "" {
		return filepath.Clean(path)
	}
	return filepath.Clean(filepath.Join(configDir, path))
}
