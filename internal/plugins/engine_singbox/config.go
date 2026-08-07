package engine_singbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"xraytool/internal/pluginapi"
)

// CommandRunner is the process boundary used for the optional Sing-box config
// check and reload hooks. Deployments commonly run Sing-box under systemd, so
// the adapter deliberately does not own a second long-running Sing-box child
// process. Instead it atomically writes its configuration and invokes the
// explicitly configured service-manager command.
type CommandRunner interface {
	Run(ctx context.Context, command string, args ...string) error
}

// CommandRunnerFunc adapts a function for tests and small embedding users.
type CommandRunnerFunc func(ctx context.Context, command string, args ...string) error

func (f CommandRunnerFunc) Run(ctx context.Context, command string, args ...string) error {
	return f(ctx, command, args...)
}

type pluginConfig struct {
	APIAddr       string
	ConfigPath    string
	TemplatePath  string
	ServerAddress string

	// CheckCommand receives the candidate temporary config in place of
	// {config_path}. Example: ["sing-box", "check", "-c", "{config_path}"].
	// It is optional because some installations validate config in their
	// service manager rather than on the xraytool node.
	CheckCommand []string
	// ReloadCommand is run after atomically replacing config_path. Example:
	// ["systemctl", "reload", "sing-box"]. An omitted command intentionally
	// means configuration-only mode; it is useful when a separate watcher owns
	// reloads, but should not be mistaken for hot reload.
	ReloadCommand []string

	ManagedInboundTags map[string]struct{}
	StatsEndpoint      string
	HealthEndpoint     string
	HTTPTimeout        time.Duration
}

func parseConfig(raw pluginapi.RawConfig) pluginConfig {
	cfg := pluginConfig{HTTPTimeout: 5 * time.Second}
	if raw == nil {
		return cfg
	}
	cfg.APIAddr = rawString(raw, "api_addr")
	cfg.ConfigPath = rawString(raw, "config_path")
	cfg.TemplatePath = rawString(raw, "template_path")
	for _, key := range []string{"server_address", "server", "host", "address"} {
		if value := rawString(raw, key); value != "" {
			cfg.ServerAddress = value
			break
		}
	}
	cfg.CheckCommand = rawCommand(raw["check_command"])
	cfg.ReloadCommand = rawCommand(raw["reload_command"])
	cfg.ManagedInboundTags = rawStringSet(raw["managed_inbound_tags"])
	cfg.StatsEndpoint = rawString(raw, "stats_endpoint")
	cfg.HealthEndpoint = rawString(raw, "health_endpoint")
	if timeout := rawDuration(raw["http_timeout"]); timeout > 0 {
		cfg.HTTPTimeout = timeout
	}
	return cfg
}

func rawString(raw pluginapi.RawConfig, key string) string {
	value, _ := raw[key].(string)
	return strings.TrimSpace(value)
}

func rawCommand(value any) []string {
	var parts []string
	switch command := value.(type) {
	case []string:
		parts = command
	case []any:
		parts = make([]string, 0, len(command))
		for _, item := range command {
			if text, ok := item.(string); ok {
				parts = append(parts, text)
			}
		}
	case string:
		// Arrays are preferred because they preserve quoted arguments. This
		// permissive form keeps a simple single executable usable too.
		parts = strings.Fields(command)
	}
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			result = append(result, part)
		}
	}
	return result
}

func rawStringSet(value any) map[string]struct{} {
	values := rawCommand(value)
	if len(values) == 0 {
		return nil
	}
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}

func rawDuration(value any) time.Duration {
	switch value := value.(type) {
	case time.Duration:
		return value
	case string:
		duration, _ := time.ParseDuration(strings.TrimSpace(value))
		return duration
	case int:
		return time.Duration(value) * time.Second
	case int64:
		return time.Duration(value) * time.Second
	case float64:
		return time.Duration(value * float64(time.Second))
	default:
		return 0
	}
}

func decodeConfig(data []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var document map[string]any
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("decode Sing-box JSON: %w", err)
	}
	if document == nil {
		return nil, errors.New("Sing-box JSON root must be an object")
	}
	if extra, err := decoder.Token(); err != io.EOF || extra != nil {
		if err == nil {
			return nil, errors.New("Sing-box JSON contains trailing content")
		}
		return nil, fmt.Errorf("decode Sing-box JSON: %w", err)
	}
	return document, nil
}

func (p *Plugin) loadConfigLocked() (map[string]any, []byte, error) {
	data, err := os.ReadFile(p.cfg.ConfigPath)
	if err != nil {
		return nil, nil, fmt.Errorf("engine_singbox: read config %q: %w", p.cfg.ConfigPath, err)
	}
	document, err := decodeConfig(data)
	if err != nil {
		return nil, nil, fmt.Errorf("engine_singbox: %w", err)
	}
	return document, data, nil
}

func loadTemplateOrConfig(cfg pluginConfig) (map[string]any, error) {
	path := cfg.ConfigPath
	if cfg.TemplatePath != "" {
		path = cfg.TemplatePath
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("engine_singbox: read %s %q: %w", map[bool]string{true: "template", false: "config"}[cfg.TemplatePath != ""], path, err)
	}
	document, err := decodeConfig(data)
	if err != nil {
		return nil, fmt.Errorf("engine_singbox: %w", err)
	}
	return document, nil
}

func (p *Plugin) mutateConfig(ctx context.Context, mutate func(map[string]any) error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.initialized {
		return errors.New("engine_singbox: plugin is not initialised")
	}
	document, original, err := p.loadConfigLocked()
	if err != nil {
		return err
	}
	if err := mutate(document); err != nil {
		return err
	}
	return p.writeAndReloadLocked(ctx, document, original)
}

// mutateSyncConfig applies a full desired-state reconciliation. When a
// template is configured, it is deliberately the source document: stale
// unmanaged topology from a previously generated config must not survive a
// core-driven rebuild. Incremental operations continue to use mutateConfig so
// they preserve the currently deployed topology.
func (p *Plugin) mutateSyncConfig(ctx context.Context, mutate func(map[string]any) error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.initialized {
		return errors.New("engine_singbox: plugin is not initialised")
	}

	original, err := os.ReadFile(p.cfg.ConfigPath)
	if err != nil {
		return fmt.Errorf("engine_singbox: read config %q: %w", p.cfg.ConfigPath, err)
	}
	document, err := loadTemplateOrConfig(p.cfg)
	if err != nil {
		return err
	}
	if err := mutate(document); err != nil {
		return err
	}
	return p.writeAndReloadLocked(ctx, document, original)
}

func (p *Plugin) writeAndReloadLocked(ctx context.Context, document map[string]any, original []byte) error {
	candidate, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return fmt.Errorf("engine_singbox: encode config: %w", err)
	}
	candidate = append(candidate, '\n')
	if err := p.runCheckLocked(ctx, candidate); err != nil {
		return err
	}
	if err := atomicWriteFile(p.cfg.ConfigPath, candidate); err != nil {
		return fmt.Errorf("engine_singbox: write config %q: %w", p.cfg.ConfigPath, err)
	}
	if err := p.runCommandLocked(ctx, p.cfg.ReloadCommand, p.cfg.ConfigPath); err != nil {
		// The process is still using the previous configuration after a failed
		// reload. Restore the file and make one best-effort reload attempt so
		// a later service-manager retry cannot pick up a broken candidate.
		_ = atomicWriteFile(p.cfg.ConfigPath, original)
		_ = p.runCommandLocked(ctx, p.cfg.ReloadCommand, p.cfg.ConfigPath)
		return fmt.Errorf("engine_singbox: reload after config update: %w", err)
	}
	return nil
}

func (p *Plugin) runCheckLocked(ctx context.Context, candidate []byte) error {
	if len(p.cfg.CheckCommand) == 0 {
		return nil
	}
	directory := filepath.Dir(p.cfg.ConfigPath)
	temporary, err := os.CreateTemp(directory, ".singbox-check-*.json")
	if err != nil {
		return fmt.Errorf("engine_singbox: create check candidate: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath) //nolint:errcheck
	if _, err := temporary.Write(candidate); err != nil {
		temporary.Close() //nolint:errcheck
		return fmt.Errorf("engine_singbox: write check candidate: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("engine_singbox: close check candidate: %w", err)
	}
	if err := p.runCommandLocked(ctx, p.cfg.CheckCommand, temporaryPath); err != nil {
		return fmt.Errorf("engine_singbox: validate candidate config: %w", err)
	}
	return nil
}

func (p *Plugin) runCommandLocked(ctx context.Context, command []string, configPath string) error {
	if len(command) == 0 {
		return nil
	}
	if p.runner == nil {
		return errors.New("engine_singbox: command runner is not configured")
	}
	args := make([]string, 0, len(command)-1)
	for _, argument := range command[1:] {
		args = append(args, strings.ReplaceAll(argument, "{config_path}", configPath))
	}
	return p.runner.Run(ctx, command[0], args...)
}

func atomicWriteFile(path string, data []byte) error {
	directory := filepath.Dir(path)
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".singbox-config-*.json")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath) //nolint:errcheck
	if err := temporary.Chmod(info.Mode()); err != nil {
		temporary.Close() //nolint:errcheck
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close() //nolint:errcheck
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close() //nolint:errcheck
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func supportedInbound(inbound map[string]any) bool {
	switch strings.ToLower(mapString(inbound, "type")) {
	case "vless", "vmess", "trojan", "shadowsocks", "hysteria2", "tuic":
		return true
	default:
		return false
	}
}

func managedInbounds(document map[string]any, tags map[string]struct{}) []map[string]any {
	rawInbounds, _ := document["inbounds"].([]any)
	result := make([]map[string]any, 0, len(rawInbounds))
	for _, rawInbound := range rawInbounds {
		inbound, ok := rawInbound.(map[string]any)
		if !ok || !supportedInbound(inbound) {
			continue
		}
		if len(tags) > 0 {
			if _, ok := tags[mapString(inbound, "tag")]; !ok {
				continue
			}
		}
		result = append(result, inbound)
	}
	return result
}

func inboundUsers(inbound map[string]any) []map[string]any {
	rawUsers, _ := inbound["users"].([]any)
	users := make([]map[string]any, 0, len(rawUsers))
	for _, rawUser := range rawUsers {
		if user, ok := rawUser.(map[string]any); ok {
			users = append(users, user)
		}
	}
	return users
}

func writeInboundUsers(inbound map[string]any, users []map[string]any) {
	rawUsers := make([]any, len(users))
	for index, user := range users {
		rawUsers[index] = user
	}
	inbound["users"] = rawUsers
}

func mapString(value map[string]any, key string) string {
	text, _ := value[key].(string)
	return strings.TrimSpace(text)
}

func mapInt(value map[string]any, key string) int {
	switch number := value[key].(type) {
	case int:
		return number
	case int64:
		return int(number)
	case float64:
		return int(number)
	case json.Number:
		parsed, _ := number.Int64()
		return int(parsed)
	case string:
		parsed, _ := strconv.Atoi(strings.TrimSpace(number))
		return parsed
	default:
		return 0
	}
}

func cloneMap(source map[string]any) map[string]any {
	result := make(map[string]any, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func sortedMapKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for value := range values {
		keys = append(keys, value)
	}
	sort.Strings(keys)
	return keys
}
