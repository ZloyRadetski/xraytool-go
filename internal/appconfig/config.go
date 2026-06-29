// Package appconfig loads and provides the tool's own configuration (config.yaml).
// This is NOT the xray config — it's xraytool's settings (paths, domain, mode, etc).
package appconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level xraytool configuration.
type Config struct {
	Mode          string           `yaml:"mode"` // "master" or "slave"
	Server        ServerConf       `yaml:"server"`
	Paths         PathsConf        `yaml:"paths"`
	Xray          XrayConf         `yaml:"xray"`
	Stats         StatsConf        `yaml:"stats"`
	SlaveAPI      SlaveAPIConf     `yaml:"slave_api"`
	MasterAPI     MasterAPIConf    `yaml:"master_api"`
	Ports         PortsConf        `yaml:"ports"`
	Logging       LoggingConf      `yaml:"logging"`
	Webhooks      []string         `yaml:"webhooks"`
	Worker        WorkerConf       `yaml:"worker"`
	Database      DatabaseConf     `yaml:"database"`
	PlategaSecret string           `yaml:"platega_secret"`
	WebhookSecret string           `yaml:"webhook_secret"`
	Subscription  SubscriptionConf `yaml:"subscription"`
	AntiFraud     AntiFraudConf    `yaml:"anti_fraud"`
	Mailer        MailerConf       `yaml:"mailer"`
}

// WorkerConf holds background worker settings.
type WorkerConf struct {
	Enabled            bool     `yaml:"enabled"`             // defaults to true
	ExpiryInterval     string   `yaml:"expiry_interval"`     // e.g., "5m"
	ExpirationWarnings []string `yaml:"expiration_warnings"` // e.g., ["72h", "24h", "3h", "1h"]
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
	// Rotation: rename to .old → gRPC RestartLogger → read .old → delete .old.
	LogRotationSizeMB int `yaml:"log_rotation_size_mb"`
	// ReportToMaster enables forwarding of IP events to the master server (slave-only).
	// When true, the slave batches observed IP events every 5s and sends them to master
	// so that master has a global view across all nodes for fraud detection.
	// The master node must have anti_fraud.enabled: true.
	ReportToMaster bool `yaml:"report_to_master"`
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
	IP     string `yaml:"ip"`
	Domain string `yaml:"domain"`
}

// PortsConf holds the configurable ports used by xraytool.
type PortsConf struct {
	APIServer int `yaml:"api_server"` // default: 8080
}

// PathsConf holds all filesystem paths used by xraytool.
type PathsConf struct {
	XrayConfig    string `yaml:"xray_config"`
	LimitedDB     string `yaml:"limited_db"`
	StatsState    string `yaml:"stats_state"`
	InferredStats string `yaml:"inferred_stats"`
	ServersJSON   string `yaml:"servers_json"`
	DevicesState  string `yaml:"devices_state"`
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
	return &Config{
		Mode: "master",
		Server: ServerConf{
			Domain: "yourdomain.tld",
		},
		Paths: PathsConf{
			XrayConfig:               "/usr/local/etc/xray/config.json",
			LimitedDB:                "/etc/xraytool/limited_users.db",
			StatsState:               "/etc/xraytool/traffic_stats_state.json",
			InferredStats:            "/etc/xraytool/inferred_traffic.json",
			ServersJSON:              "/etc/xraytool/servers.json",
			DevicesState:             "/etc/xraytool/devices_state.json",
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
			APIKey:   "",
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
			ExpirationWarnings: []string{"72h", "24h", "3h", "1h"},
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
}

const defaultConfigYAML = `# ============================================================
# xraytool configuration
# Copy to /etc/xraytool/config.yaml and edit.
# Run with: xraytool --config /etc/xraytool/config.yaml <command>
# ============================================================

# Mode: "master" (manages slaves) or "slave" (executes commands from master)
mode: master

server:
  # Public IP of this server (informational, used in some outputs)
  ip: "1.2.3.4"
  # Domain for subscription links: https://<domain>/client?id=<subfile>
  domain: "yourdomain.tld"

paths:
  # Xray-core main config
  xray_config: "/usr/local/etc/xray/config.json"

  # Flat-text DB for blocked users (email|subfile|limit, one per line)
  # Kept in this format for compatibility with sub.php
  limited_db: "/etc/xraytool/limited_users.db"

  # JSON file storing cumulative traffic stats state
  stats_state: "/etc/xraytool/traffic_stats_state.json"

  # JSON file for inferred/analyzer traffic stats
  inferred_stats: "/etc/xraytool/inferred_traffic.json"

  # JSON file listing slave servers (required only when mode=master)
  servers_json: "/etc/xraytool/servers.json"

  # JSON file storing unique device HWIDs and states (for sub device limits)
  devices_state: "/etc/xraytool/devices_state.json"

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
  # The exact URL of the master's sync endpoint. Must include scheme, domain, and path.
  url: "https://master.example.com/api/v1/internal/xray/sync"
  # The internal API key of the master node (matches master's api.internal_key).
  api_key: "secret"
  # If true, skips TLS certificate verification when connecting to the master.
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

# Secret used to verify Platega webhooks and API calls between backend and bot
platega_secret: "your_secret_here"

# Secret used to sign outgoing webhooks sent to clients-tg-go
webhook_secret: "your_webhook_secret_here"

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
  # (slave only) If true, forward observed IP events to master every 5s for global fraud detection.
  # Master must have anti_fraud.enabled: true.
  report_to_master: false

mailer:
  # Enable transactional email delivery (OTP codes via Resend.com).
  enabled: false
  # Resend API key — create one at https://resend.com/api-keys
  resend_api_key: ""
  # Verified sender address (must match the domain verified in Resend)
  from_email: "noreply@yourdomain.tld"

# ============================================================
# servers.json format (object style):
# {
#   "server-name": {
#     "url": "https://slave.example.com/api/rest/xraytool",   # full URL (preferred)
#     "domain": "slave.example.com",   # OR domain + optional scheme/port/path
#     "scheme": "https",
#     "port": 443,
#     "path": "/api/rest/xraytool",
#     "api_key": "secret",             # sent as X-API-Key header
#     "bearer": "token",               # sent as Authorization: Bearer header
#     "insecure": false                # skip TLS verification
#   }
# }
#
# Array style also supported:
# [{"name": "server-name", "url": "...", "api_key": "..."}]
# ============================================================
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

	// Apply field-level defaults for anything left empty after unmarshal.
	defs := defaults()
	if cfg.Paths.XrayConfig == "" {
		cfg.Paths.XrayConfig = defs.Paths.XrayConfig
	}
	if cfg.Paths.LimitedDB == "" {
		cfg.Paths.LimitedDB = defs.Paths.LimitedDB
	}
	if cfg.Paths.StatsState == "" {
		cfg.Paths.StatsState = defs.Paths.StatsState
	}
	if cfg.Paths.InferredStats == "" {
		cfg.Paths.InferredStats = defs.Paths.InferredStats
	}
	if cfg.Paths.ServersJSON == "" {
		cfg.Paths.ServersJSON = defs.Paths.ServersJSON
	}
	if cfg.Paths.DevicesState == "" {
		cfg.Paths.DevicesState = defs.Paths.DevicesState
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
	return nil
}



