// Package engine_singbox implements a file-managed Sing-box EngineProvider.
//
// Sing-box deliberately has no universal hot user-management API comparable to
// Xray's gRPC handler service. This adapter therefore manages standard
// Sing-box JSON inbounds atomically and delegates process reload to an explicit
// deployment-owned command (usually systemctl). It is a real adapter, not a
// no-op: all mutations update the native users arrays, sync regenerates the
// managed config, and client links are derived from Sing-box's own format.
package engine_singbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"sort"
	"strings"
	"sync"

	"xraytool/internal/pluginapi"
)

// ErrStatsUnavailable means the configured Sing-box deployment has not
// exposed a per-user stats endpoint. Sing-box's stock Clash API has connection
// counters but no portable per-user traffic endpoint, so callers can choose to
// log/degrade this engine while other engines continue to report stats.
var ErrStatsUnavailable = errors.New("engine_singbox: per-user stats endpoint is not configured")

// Plugin is an EngineProvider backed by a Sing-box JSON configuration file.
type Plugin struct {
	mu          sync.Mutex
	log         *slog.Logger
	cfg         pluginConfig
	runner      CommandRunner
	initialized bool

	// banned retains the exact native user records removed for a soft ban. It
	// is intentionally memory-local: persistent truth remains the core
	// subscription repository and a later SyncUsers rebuild restores users
	// after process restart.
	banned map[string]map[string][]map[string]any // email -> inbound tag -> users
}

// New creates an uninitialised Sing-box plugin using os/exec for configured
// check/reload commands.
func New() *Plugin {
	return NewWithRunner(execCommandRunner{})
}

// NewWithRunner permits deterministic tests and embedding-specific process
// supervision without weakening the production file-management path.
func NewWithRunner(runner CommandRunner) *Plugin {
	return &Plugin{
		log:    slog.Default().With("plugin", "engine_singbox"),
		runner: runner,
		banned: make(map[string]map[string][]map[string]any),
	}
}

type execCommandRunner struct{}

func (execCommandRunner) Run(ctx context.Context, command string, args ...string) error {
	cmd := exec.CommandContext(ctx, command, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message != "" {
			return fmt.Errorf("%w: %s", err, message)
		}
		return err
	}
	return nil
}

func (p *Plugin) Metadata() pluginapi.Metadata {
	return pluginapi.Metadata{
		Name:        "engine_singbox",
		Kind:        "engine",
		Version:     "1.0.0",
		APIVersion:  pluginapi.CurrentAPIVersion,
		Description: "Sing-box VPN engine: atomic JSON user management, reload hooks, and subscription links.",
		Publishes: []pluginapi.ServiceRef{
			{Name: "engine.softban"},
			{Name: "engine.logger_control"},
		},
	}
}

func (p *Plugin) Init(_ context.Context, rawCfg pluginapi.RawConfig, reg pluginapi.ServiceResolver) error {
	if p == nil {
		return errors.New("engine_singbox: plugin is nil")
	}
	cfg := parseConfig(rawCfg)
	if cfg.ConfigPath == "" {
		return errors.New("engine_singbox: config_path is required")
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if _, _, err := p.loadConfigForPathLocked(cfg); err != nil {
		return err
	}
	p.cfg = cfg
	p.initialized = true
	if p.banned == nil {
		p.banned = make(map[string]map[string][]map[string]any)
	}
	if reg != nil {
		reg.Logger().Info("engine_singbox: config-managed adapter initialised",
			"config_path", cfg.ConfigPath,
			"template_path", cfg.TemplatePath,
			"reload_configured", len(cfg.ReloadCommand) > 0,
		)
	}
	return nil
}

func (p *Plugin) loadConfigForPathLocked(cfg pluginConfig) (map[string]any, []byte, error) {
	old := p.cfg
	p.cfg = cfg
	document, data, err := p.loadConfigLocked()
	p.cfg = old
	return document, data, err
}

// Start has no child process to spawn: systemd, Docker, or another configured
// process supervisor owns Sing-box. The plugin remains running while the Host
// context is alive so lifecycle behaviour matches every other engine plugin.
func (p *Plugin) Start(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

func (p *Plugin) Stop(_ context.Context) error { return nil }

func (p *Plugin) Health(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.initialized {
		return errors.New("engine_singbox: plugin is not initialised")
	}
	if _, _, err := p.loadConfigLocked(); err != nil {
		return err
	}
	if endpoint := endpointURL(p.cfg.APIAddr, p.cfg.HealthEndpoint); endpoint != "" {
		return checkHTTPEndpoint(ctx, endpoint, p.cfg.HTTPTimeout)
	}
	return nil
}

func (p *Plugin) ID() string { return "singbox" }

func (p *Plugin) PublishedServices() map[string]any {
	return map[string]any{
		"engine.softban":        p,
		"engine.logger_control": p,
	}
}

func (p *Plugin) AddUser(ctx context.Context, user pluginapi.VPNUserConfig) error {
	if strings.TrimSpace(user.Email) == "" {
		return errors.New("engine_singbox: user email is required")
	}
	return p.mutateConfig(ctx, func(document map[string]any) error {
		inbounds := managedInbounds(document, p.cfg.ManagedInboundTags)
		if len(inbounds) == 0 {
			return errors.New("engine_singbox: no supported managed inbounds found")
		}
		for _, inbound := range inbounds {
			users := inboundUsers(inbound)
			next := userRecordForInbound(inbound, user)
			updated := false
			for index, current := range users {
				if userEmail(current) != user.Email {
					continue
				}
				users[index] = mergeUserRecord(current, next)
				updated = true
				break
			}
			if !updated {
				users = append(users, next)
			}
			writeInboundUsers(inbound, users)
		}
		delete(p.banned, user.Email)
		return nil
	})
}

func (p *Plugin) AddUsersBulk(ctx context.Context, users []pluginapi.VPNUserConfig) error {
	if len(users) == 0 {
		return nil
	}
	return p.mutateConfig(ctx, func(document map[string]any) error {
		inbounds := managedInbounds(document, p.cfg.ManagedInboundTags)
		if len(inbounds) == 0 {
			return errors.New("engine_singbox: no supported managed inbounds found")
		}
		for _, inbound := range inbounds {
			currentUsers := inboundUsers(inbound)
			indexByEmail := make(map[string]int, len(currentUsers))
			for index, current := range currentUsers {
				if email := userEmail(current); email != "" {
					indexByEmail[email] = index
				}
			}
			for _, user := range users {
				if strings.TrimSpace(user.Email) == "" {
					return errors.New("engine_singbox: user email is required")
				}
				next := userRecordForInbound(inbound, user)
				if index, exists := indexByEmail[user.Email]; exists {
					currentUsers[index] = mergeUserRecord(currentUsers[index], next)
				} else {
					indexByEmail[user.Email] = len(currentUsers)
					currentUsers = append(currentUsers, next)
				}
				delete(p.banned, user.Email)
			}
			writeInboundUsers(inbound, currentUsers)
		}
		return nil
	})
}

func (p *Plugin) RemoveUser(ctx context.Context, email string) error {
	email = strings.TrimSpace(email)
	if email == "" {
		return errors.New("engine_singbox: user email is required")
	}
	return p.mutateConfig(ctx, func(document map[string]any) error {
		for _, inbound := range managedInbounds(document, p.cfg.ManagedInboundTags) {
			users := inboundUsers(inbound)
			filtered := users[:0]
			for _, user := range users {
				if userEmail(user) != email {
					filtered = append(filtered, user)
				}
			}
			writeInboundUsers(inbound, filtered)
		}
		delete(p.banned, email)
		return nil
	})
}

func (p *Plugin) RemoveUsersBulk(ctx context.Context, emails []string) error {
	if len(emails) == 0 {
		return nil
	}
	remove := make(map[string]struct{}, len(emails))
	for _, email := range emails {
		if email = strings.TrimSpace(email); email != "" {
			remove[email] = struct{}{}
		}
	}
	if len(remove) == 0 {
		return nil
	}
	return p.mutateConfig(ctx, func(document map[string]any) error {
		for _, inbound := range managedInbounds(document, p.cfg.ManagedInboundTags) {
			users := inboundUsers(inbound)
			filtered := users[:0]
			for _, user := range users {
				if _, found := remove[userEmail(user)]; !found {
					filtered = append(filtered, user)
				}
			}
			writeInboundUsers(inbound, filtered)
		}
		for email := range remove {
			delete(p.banned, email)
		}
		return nil
	})
}

// SetExpire records the lifecycle value in the engine-owned xraytool metadata
// on every native user record. Sing-box itself delegates expiry policy to its
// surrounding control plane; retaining the value in config keeps regenerated
// state inspectable and lets a custom Sing-box rule/watcher consume it.
func (p *Plugin) SetExpire(ctx context.Context, email string, expire string) error {
	return p.updateUserMetadata(ctx, email, func(metadata map[string]any) {
		metadata["expire"] = expire
	})
}

func (p *Plugin) SetLimit(ctx context.Context, email string, limit float64) error {
	return p.updateUserMetadata(ctx, email, func(metadata map[string]any) {
		metadata["max_devices"] = limit
	})
}

func (p *Plugin) updateUserMetadata(ctx context.Context, email string, update func(map[string]any)) error {
	email = strings.TrimSpace(email)
	if email == "" {
		return errors.New("engine_singbox: user email is required")
	}
	return p.mutateConfig(ctx, func(document map[string]any) error {
		found := false
		for _, inbound := range managedInbounds(document, p.cfg.ManagedInboundTags) {
			users := inboundUsers(inbound)
			for _, user := range users {
				if userEmail(user) != email {
					continue
				}
				metadata, _ := user["xraytool"].(map[string]any)
				if metadata == nil {
					metadata = make(map[string]any)
					user["xraytool"] = metadata
				}
				update(metadata)
				found = true
			}
			writeInboundUsers(inbound, users)
		}
		if !found {
			return fmt.Errorf("engine_singbox: user %q not found", email)
		}
		return nil
	})
}

// RebuildInbound invokes the configured reload hook after verifying the named
// managed inbound exists. Sing-box reloads its complete JSON configuration, so
// this is the closest native equivalent to Xray's per-inbound rebuild.
func (p *Plugin) RebuildInbound(ctx context.Context, tag string) error {
	tag = strings.TrimSpace(tag)
	if tag == "" {
		return errors.New("engine_singbox: inbound tag is required")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.initialized {
		return errors.New("engine_singbox: plugin is not initialised")
	}
	document, _, err := p.loadConfigLocked()
	if err != nil {
		return err
	}
	for _, inbound := range managedInbounds(document, p.cfg.ManagedInboundTags) {
		if mapString(inbound, "tag") == tag {
			return p.runCommandLocked(ctx, p.cfg.ReloadCommand, p.cfg.ConfigPath)
		}
	}
	return fmt.Errorf("engine_singbox: managed inbound %q not found", tag)
}

func (p *Plugin) QueryStats(ctx context.Context) ([]pluginapi.TrafficStat, error) {
	p.mu.Lock()
	if !p.initialized {
		p.mu.Unlock()
		return nil, errors.New("engine_singbox: plugin is not initialised")
	}
	endpoint := endpointURL(p.cfg.APIAddr, p.cfg.StatsEndpoint)
	timeout := p.cfg.HTTPTimeout
	p.mu.Unlock()
	if endpoint == "" {
		return nil, ErrStatsUnavailable
	}
	return fetchTrafficStats(ctx, endpoint, timeout)
}

func (p *Plugin) BanUser(ctx context.Context, email string) error {
	email = strings.TrimSpace(email)
	if email == "" {
		return errors.New("engine_singbox: user email is required")
	}
	return p.mutateConfig(ctx, func(document map[string]any) error {
		records := make(map[string][]map[string]any)
		for _, inbound := range managedInbounds(document, p.cfg.ManagedInboundTags) {
			tag := mapString(inbound, "tag")
			users := inboundUsers(inbound)
			filtered := users[:0]
			for _, user := range users {
				if userEmail(user) == email {
					records[tag] = append(records[tag], cloneMap(user))
					continue
				}
				filtered = append(filtered, user)
			}
			writeInboundUsers(inbound, filtered)
		}
		if len(records) > 0 {
			p.banned[email] = records
		}
		return nil
	})
}

func (p *Plugin) UnbanUser(ctx context.Context, email string) error {
	email = strings.TrimSpace(email)
	if email == "" {
		return errors.New("engine_singbox: user email is required")
	}
	return p.mutateConfig(ctx, func(document map[string]any) error {
		records := p.banned[email]
		if len(records) == 0 {
			return nil
		}
		for _, inbound := range managedInbounds(document, p.cfg.ManagedInboundTags) {
			tag := mapString(inbound, "tag")
			for _, record := range records[tag] {
				users := inboundUsers(inbound)
				alreadyPresent := false
				for _, user := range users {
					if userEmail(user) == email {
						alreadyPresent = true
						break
					}
				}
				if !alreadyPresent {
					users = append(users, cloneMap(record))
					writeInboundUsers(inbound, users)
				}
			}
		}
		delete(p.banned, email)
		return nil
	})
}

func (p *Plugin) RestartLogger(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.initialized {
		return errors.New("engine_singbox: plugin is not initialised")
	}
	return p.runCommandLocked(ctx, p.cfg.ReloadCommand, p.cfg.ConfigPath)
}

func (p *Plugin) ListUsers(_ context.Context) ([]pluginapi.VPNUserConfig, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.initialized {
		return nil, errors.New("engine_singbox: plugin is not initialised")
	}
	document, _, err := p.loadConfigLocked()
	if err != nil {
		return nil, err
	}
	usersByEmail := make(map[string]pluginapi.VPNUserConfig)
	for _, inbound := range managedInbounds(document, p.cfg.ManagedInboundTags) {
		for _, user := range inboundUsers(inbound) {
			candidate := vpnUserFromRecord(user)
			if candidate.Email == "" {
				continue
			}
			if _, exists := usersByEmail[candidate.Email]; !exists {
				usersByEmail[candidate.Email] = candidate
			}
		}
	}
	users := make([]pluginapi.VPNUserConfig, 0, len(usersByEmail))
	for _, user := range usersByEmail {
		users = append(users, user)
	}
	sort.Slice(users, func(i, j int) bool { return users[i].Email < users[j].Email })
	return users, nil
}

func (p *Plugin) SyncUsers(ctx context.Context, dbUsers []pluginapi.VPNUserConfig, removeOrphans bool) (*pluginapi.EngineSyncResult, error) {
	result := &pluginapi.EngineSyncResult{}
	err := p.mutateSyncConfig(ctx, func(document map[string]any) error {
		inbounds := managedInbounds(document, p.cfg.ManagedInboundTags)
		if len(inbounds) == 0 {
			return errors.New("engine_singbox: no supported managed inbounds found")
		}

		desired := make(map[string]pluginapi.VPNUserConfig, len(dbUsers))
		for _, user := range dbUsers {
			if email := strings.TrimSpace(user.Email); email != "" {
				user.Email = email
				desired[email] = user
			}
		}
		preExisting := make(map[string]struct{})
		for _, inbound := range inbounds {
			for _, user := range inboundUsers(inbound) {
				if email := userEmail(user); email != "" {
					preExisting[email] = struct{}{}
				}
			}
		}

		removed := make(map[string]struct{})
		for _, inbound := range inbounds {
			current := inboundUsers(inbound)
			byEmail := make(map[string]map[string]any, len(current))
			unmanaged := make([]map[string]any, 0)
			for _, user := range current {
				email := userEmail(user)
				if email == "" {
					unmanaged = append(unmanaged, user)
					continue
				}
				byEmail[email] = user
			}

			next := make([]map[string]any, 0, len(unmanaged)+len(desired))
			next = append(next, unmanaged...)
			for _, email := range sortedUserEmails(desired) {
				user := desired[email]
				record := userRecordForInbound(inbound, user)
				if existing, exists := byEmail[email]; exists {
					record = mergeUserRecord(existing, record)
				}
				next = append(next, record)
			}
			if !removeOrphans {
				for _, email := range sortedUserRecordEmails(byEmail) {
					if _, wanted := desired[email]; !wanted {
						next = append(next, byEmail[email])
					}
				}
			} else {
				for email := range byEmail {
					if _, wanted := desired[email]; !wanted {
						removed[email] = struct{}{}
					}
				}
			}
			writeInboundUsers(inbound, next)
		}

		for email := range desired {
			if _, existed := preExisting[email]; !existed {
				result.Added++
			}
			delete(p.banned, email)
		}
		result.Removed = len(removed)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func userEmail(user map[string]any) string {
	for _, key := range []string{"name", "email"} {
		if email := mapString(user, key); email != "" {
			return email
		}
	}
	return ""
}

func userRecordForInbound(inbound map[string]any, user pluginapi.VPNUserConfig) map[string]any {
	record := map[string]any{"name": user.Email}
	switch strings.ToLower(mapString(inbound, "type")) {
	case "vless", "vmess":
		record["uuid"] = user.UUID
		if user.Flow != "" && strings.EqualFold(mapString(inbound, "type"), "vless") {
			record["flow"] = user.Flow
		}
	case "trojan", "hysteria2":
		record["password"] = firstNonEmpty(user.Auth, user.UUID)
	case "shadowsocks":
		record["password"] = firstNonEmpty(user.Auth, user.UUID)
		if user.Cipher != "" {
			record["method"] = user.Cipher
		}
	case "tuic":
		record["uuid"] = user.UUID
		record["password"] = firstNonEmpty(user.Auth, user.UUID)
	}
	metadata := map[string]any{}
	if user.Expire != "" {
		metadata["expire"] = user.Expire
	}
	if user.MaxDevices > 0 {
		metadata["max_devices"] = user.MaxDevices
	}
	if len(metadata) > 0 {
		record["xraytool"] = metadata
	}
	return record
}

func mergeUserRecord(existing, replacement map[string]any) map[string]any {
	merged := cloneMap(existing)
	for key, value := range replacement {
		if key == "xraytool" {
			metadata, _ := merged[key].(map[string]any)
			if metadata == nil {
				metadata = make(map[string]any)
			}
			for metadataKey, metadataValue := range value.(map[string]any) {
				metadata[metadataKey] = metadataValue
			}
			merged[key] = metadata
			continue
		}
		merged[key] = value
	}
	return merged
}

func vpnUserFromRecord(record map[string]any) pluginapi.VPNUserConfig {
	result := pluginapi.VPNUserConfig{
		Email:  userEmail(record),
		UUID:   firstNonEmpty(mapString(record, "uuid"), mapString(record, "id")),
		Auth:   mapString(record, "password"),
		Flow:   mapString(record, "flow"),
		Cipher: mapString(record, "method"),
	}
	if metadata, ok := record["xraytool"].(map[string]any); ok {
		result.Expire = mapString(metadata, "expire")
		result.MaxDevices = mapInt(metadata, "max_devices")
	}
	return result
}

func sortedUserEmails(users map[string]pluginapi.VPNUserConfig) []string {
	emails := make([]string, 0, len(users))
	for email := range users {
		emails = append(emails, email)
	}
	sort.Strings(emails)
	return emails
}

func sortedUserRecordEmails(users map[string]map[string]any) []string {
	emails := make([]string, 0, len(users))
	for email := range users {
		emails = append(emails, email)
	}
	sort.Strings(emails)
	return emails
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

var _ pluginapi.Plugin = (*Plugin)(nil)
var _ pluginapi.ServiceProvider = (*Plugin)(nil)
var _ pluginapi.EngineProvider = (*Plugin)(nil)
