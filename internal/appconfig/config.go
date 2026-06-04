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
	Mode     string       `yaml:"mode"` // "master" or "slave"
	Server   ServerConf   `yaml:"server"`
	Paths    PathsConf    `yaml:"paths"`
	Xray     XrayConf     `yaml:"xray"`
	Stats    StatsConf    `yaml:"stats"`
	SlaveAPI SlaveAPIConf `yaml:"slave_api"`
	Ports    PortsConf    `yaml:"ports"`
	Logging  LoggingConf  `yaml:"logging"`
	Webhooks []string     `yaml:"webhooks"`
}

// ServerConf holds server identity information.
type ServerConf struct {
	IP     string `yaml:"ip"`
	Domain string `yaml:"domain"`
}

// PortsConf holds the configurable ports used by xraytool.
type PortsConf struct {
	APIServer int `yaml:"api_server"` // default: 8080
	PythonBot int `yaml:"python_bot"` // default: 8081
}

// PathsConf holds all filesystem paths used by xraytool.
type PathsConf struct {
	XrayConfig    string `yaml:"xray_config"`
	LimitedDB     string `yaml:"limited_db"`
	StatsState    string `yaml:"stats_state"`
	InferredStats string `yaml:"inferred_stats"`
	TemplatesDir  string `yaml:"templates_dir"`
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
		Paths: PathsConf{
			XrayConfig:               "/usr/local/etc/xray/config.json",
			LimitedDB:                "/etc/xraytool/limited_users.db",
			StatsState:               "/etc/xraytool/traffic_stats_state.json",
			InferredStats:            "/etc/xraytool/inferred_traffic.json",
			TemplatesDir:             "/etc/xraytool/inbound-client-templates",
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
			ConnectTimeout: 4 * time.Second,
			RequestTimeout: 15 * time.Second,
			RemotePath:     "/api/rest/xraytool",
		},
		Ports: PortsConf{
			APIServer: 8080,
			PythonBot: 8081,
		},
		Logging: LoggingConf{
			Level:    "info",
			FilePath: "",
			Format:   "console",
		},
		Webhooks: []string{},
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

  # Directory with per-inbound client templates (one file per inbound tag)
  # Example: /etc/xraytool/inbound-client-templates/reality-in-443.txt
  templates_dir: "/etc/xraytool/inbound-client-templates"

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

ports:
  # Port for the REST API server (api-server / start-server)
  api_server: 8080
  # Port for triggering the Python Telegram bot update
  python_bot: 8081

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
	if cfg.Paths.TemplatesDir == "" {
		cfg.Paths.TemplatesDir = defs.Paths.TemplatesDir
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
	if cfg.Ports.PythonBot == 0 {
		cfg.Ports.PythonBot = defs.Ports.PythonBot
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
