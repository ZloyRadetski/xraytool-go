package support_chat

import (
	"fmt"
	"os"
	"strings"
	"time"

	"xraytool/internal/pluginapi"
)

type pluginConfig struct {
	Database          DBConfig
	MasterKey         string
	MasterKeyFile     string
	LegacyMasterKeys  []string
	KeyVersion        uint16
	MigrateLegacyData bool
	RetentionDays     int
	MaxOpenPerUser    int
	WebSocketEnabled  bool
	WSPingInterval    time.Duration
	WSPongTimeout     time.Duration
	Media             MediaConfig
}

type MediaConfig struct {
	StoragePath   string
	MaxFileSizeMB int
}

type DBConfig struct {
	Driver     string
	SQLitePath string
	DSN        string
}

func parseConfig(raw pluginapi.RawConfig) (pluginConfig, error) {
	cfg := pluginConfig{
		KeyVersion:        1,
		MigrateLegacyData: true,
		RetentionDays:     90,
		MaxOpenPerUser:    3,
		WebSocketEnabled:  true,
		WSPingInterval:    30 * time.Second,
		WSPongTimeout:     10 * time.Second,
		Database: DBConfig{
			Driver:     "sqlite",
			SQLitePath: "data/support_chat.db",
		},
		Media: MediaConfig{
			StoragePath:   "/etc/xraytool/support_media",
			MaxFileSizeMB: 50,
		},
	}

	if raw == nil {
		return cfg, fmt.Errorf("config is empty")
	}

	if mk, ok := raw["master_key"].(string); ok && mk != "" {
		cfg.MasterKey = mk
	}
	if path, ok := raw["master_key_file"].(string); ok && strings.TrimSpace(path) != "" {
		cfg.MasterKeyFile = path
		data, err := os.ReadFile(path)
		if err != nil {
			return cfg, fmt.Errorf("read master_key_file: %w", err)
		}
		cfg.MasterKey = strings.TrimSpace(string(data))
	}
	if cfg.MasterKey == "" {
		return cfg, fmt.Errorf("master_key or master_key_file is required and must not be empty")
	}
	if values, ok := raw["legacy_master_keys"].([]any); ok {
		for _, value := range values {
			if key, ok := value.(string); ok && strings.TrimSpace(key) != "" {
				cfg.LegacyMasterKeys = append(cfg.LegacyMasterKeys, strings.TrimSpace(key))
			}
		}
	}
	if v, ok := raw["key_version"].(float64); ok && v > 0 && v <= 65535 {
		cfg.KeyVersion = uint16(v)
	} else if v, ok := raw["key_version"].(int); ok && v > 0 && v <= 65535 {
		cfg.KeyVersion = uint16(v)
	}
	if v, ok := raw["migrate_legacy_data"].(bool); ok {
		cfg.MigrateLegacyData = v
	}

	if dbMap, ok := raw["database"].(map[string]any); ok {
		if driver, ok := dbMap["driver"].(string); ok {
			cfg.Database.Driver = driver
		}
		if path, ok := dbMap["sqlite_path"].(string); ok {
			cfg.Database.SQLitePath = path
		}
		if dsn, ok := dbMap["dsn"].(string); ok {
			cfg.Database.DSN = dsn
		}
	} else {
		// default to sqlite
		cfg.Database.Driver = "sqlite"
		cfg.Database.SQLitePath = "/etc/xraytool/support_chat.db"
	}

	if v, ok := raw["retention_days"].(float64); ok {
		cfg.RetentionDays = int(v)
	} else if v, ok := raw["retention_days"].(int); ok {
		cfg.RetentionDays = v
	}

	if v, ok := raw["max_open_per_user"].(float64); ok {
		cfg.MaxOpenPerUser = int(v)
	} else if v, ok := raw["max_open_per_user"].(int); ok {
		cfg.MaxOpenPerUser = v
	}

	if mediaMap, ok := raw["media"].(map[string]any); ok {
		if path, ok := mediaMap["storage_path"].(string); ok {
			cfg.Media.StoragePath = path
		}
		if size, ok := mediaMap["max_file_size_mb"].(float64); ok {
			cfg.Media.MaxFileSizeMB = int(size)
		} else if size, ok := mediaMap["max_file_size_mb"].(int); ok {
			cfg.Media.MaxFileSizeMB = size
		}
	}

	if v, ok := raw["websocket_enabled"].(bool); ok {
		cfg.WebSocketEnabled = v
	}

	if v, ok := raw["ws_ping_interval"].(string); ok {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.WSPingInterval = d
		}
	}
	if v, ok := raw["ws_pong_timeout"].(string); ok {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.WSPongTimeout = d
		}
	}

	// Validate DB config
	if cfg.Database.Driver == "sqlite" && cfg.Database.SQLitePath == "" {
		return cfg, fmt.Errorf("database.sqlite_path is required when driver is sqlite")
	}
	if cfg.Database.Driver == "postgres" && cfg.Database.DSN == "" {
		return cfg, fmt.Errorf("database.dsn is required when driver is postgres")
	}
	if cfg.Database.Driver != "sqlite" && cfg.Database.Driver != "postgres" {
		return cfg, fmt.Errorf("unsupported database driver: %s", cfg.Database.Driver)
	}

	return cfg, nil
}
