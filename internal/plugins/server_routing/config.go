package server_routing

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"xraytool/internal/pluginapi"
)

const defaultRoutingDir = "/root/xraytool/data/routing"

type pluginConfig struct {
	RoutingDir string
}

func parseConfig(raw pluginapi.RawConfig) (pluginConfig, error) {
	cfg := pluginConfig{RoutingDir: defaultRoutingDir}
	if raw == nil {
		return cfg, nil
	}

	routingDir, exists := raw["routing_dir"]
	if !exists {
		return cfg, nil
	}
	value, ok := routingDir.(string)
	if !ok {
		return pluginConfig{}, fmt.Errorf("routing_dir must be a string")
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return pluginConfig{}, fmt.Errorf("routing_dir must not be empty")
	}
	if !strings.HasPrefix(value, "/") {
		return pluginConfig{}, fmt.Errorf("routing_dir must be an absolute path")
	}
	cfg.RoutingDir = filepath.Clean(value)
	return cfg, nil
}

func ensureDirectories(cfg pluginConfig) error {
	if err := os.MkdirAll(cfg.RoutingDir, 0o755); err != nil {
		return fmt.Errorf("create routing directory %q: %w", cfg.RoutingDir, err)
	}
	if err := os.MkdirAll(filepath.Join(cfg.RoutingDir, "outbounds"), 0o755); err != nil {
		return fmt.Errorf("create routing outbounds directory: %w", err)
	}
	return nil
}
