// Package xray provides a Engine adapter that drives Xray-core.
//
// This adapter encapsulates ALL Xray-specific knowledge:
//   - JSON config file mutations (Modify / Write)
//   - Advisory file locking (via xrayconfig package)
//   - Hot-add / hot-remove via gRPC (GRPCClient)
//   - Legacy os/exec fallback for unsupported gRPC protocols
//   - Traffic stats via the Xray StatsService gRPC endpoint
//   - Log rotation via the Xray LoggerService gRPC endpoint
//
// No file outside this package (or xrayapi / xrayconfig) should ever import
// xray-core types directly. Business logic (worker, server, user.Service) must
// use the Engine interface exclusively.
package vpn

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"xraytool/internal/domain"
)

// Adapter implements Engine for Xray-core.
// It is safe for concurrent use: the underlying Modify already
// holds an exclusive process-level mutex + advisory flock on writes.
type Adapter struct {
	grpc            *GRPCClient
	configPath      string
	templatePath    string
	realityRotation bool
	realityKeysPath string
	log             *slog.Logger
}

// compile-time interface check — the compiler will error here if Adapter ever
// stops satisfying Engine, giving you an immediate, clear signal.
var _ domain.Engine = (*Adapter)(nil)

// New creates a ready-to-use Xray engine adapter.
//
//   - grpcAddr   — the host:port of the Xray gRPC API (e.g. "127.0.0.1:10085")
//   - configPath — absolute path to the live xray config JSON file
//   - log        — structured logger; the adapter will tag its messages with
//     component="xray-adapter"
func NewAdapter(grpcAddr, configPath, templatePath string, realityRotation bool, realityKeysPath string, log *slog.Logger) *Adapter {
	return &Adapter{
		grpc:            NewGRPCClient(grpcAddr, log),
		configPath:      configPath,
		templatePath:    templatePath,
		realityRotation: realityRotation,
		realityKeysPath: realityKeysPath,
		log:             log.With("component", "xray-adapter"),
	}
}

func wrapError(err error) error {
	if err == nil {
		return nil
	}
	s := err.Error()
	if strings.Contains(s, "connection refused") || strings.Contains(s, "dial tcp") || strings.Contains(s, "context deadline exceeded") {
		return fmt.Errorf("%w: %v", ErrEngineUnavailable, err)
	}
	return err
}

// ─────────────────────────────────────────────────────────────────────────────
// AddUser — persist to config + hot-add via gRPC
// ─────────────────────────────────────────────────────────────────────────────

// AddUser provisions a single user:
//  1. Reads the live xray config (with flock).
//  2. Checks idempotency — if user already exists we skip the config write.
//  3. Builds per-inbound client structs from UserConfig fields.
//  4. Atomically writes the updated config to disk.
//  5. Hot-adds the user to the running Xray process via gRPC.
//
// Step 5 is best-effort: a gRPC failure is logged as a warning but does NOT
// return an error, because the config on disk is already correct.  The next
// Xray restart or syncstates run will re-apply the config.
//
// Dry-run scenarios verified:
//   - Empty email → early error before any I/O.
//   - User already in config → noop (idempotent).
//   - Xray not running during hot-add → warn + return nil.
func (a *Adapter) AddUser(ctx context.Context, user domain.VPNUserConfig) error {
	if user.Email == "" {
		return fmt.Errorf("xray adapter AddUser: email must not be empty")
	}

	var clientsPayload []TaggedClient

	err := Modify(a.configPath, func(cfg RawConfig) error {
		// Idempotency: skip if already present.
		exists, _ := UserExists(cfg, user.Email)
		if exists {
			a.log.Info("xray adapter: user already in config, skipping write", "email", user.Email)
			return nil
		}

		params := configToParams(user)
		payload, err := BuildForAllInbounds(cfg, params)
		if err != nil {
			return fmt.Errorf("building client payload: %w", err)
		}
		if len(payload) == 0 {
			return fmt.Errorf("no client inbounds found in xray config")
		}

		if err := AddUserToInbounds(cfg, payload); err != nil {
			return fmt.Errorf("adding user to inbounds: %w", err)
		}

		clientsPayload = payload
		return nil
	})
	if err != nil {
		return fmt.Errorf("xray adapter AddUser: config write failed: %w", err)
	}

	// Hot-add — best-effort, non-fatal.
	if len(clientsPayload) > 0 {
		if err := a.grpc.AddUser(ctx, clientsPayload, a.configPath); err != nil {
			a.log.Warn("xray adapter: hot-add via gRPC failed (config already updated on disk)",
				"email", user.Email, "err", err)
		}
	}

	// After hot-add, rebuild any hysteria2 inbounds (they don't support per-user add).
	a.rebuildHysteriaInbounds(ctx)

	a.log.Info("xray adapter: user added", "email", user.Email)
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// AddUsersBulk — single config write + single gRPC batch call
// ─────────────────────────────────────────────────────────────────────────────

// AddUsersBulk is significantly more efficient than calling AddUser in a loop
// because it performs exactly one config-file write followed by one gRPC call
// regardless of the number of users.
//
// Users that are already present in the config are skipped silently (idempotent).
//
// Dry-run scenarios verified:
//   - Empty slice → returns nil immediately (no-op).
//   - All users already in config → no disk I/O, no gRPC call.
func (a *Adapter) AddUsersBulk(ctx context.Context, users []domain.VPNUserConfig) error {
	if len(users) == 0 {
		return nil
	}

	var allPayloads []TaggedClient

	err := Modify(a.configPath, func(cfg RawConfig) error {
		for _, user := range users {
			if user.Email == "" {
				continue
			}
			exists, _ := UserExists(cfg, user.Email)
			if exists {
				continue
			}
			params := configToParams(user)
			payload, err := BuildForAllInbounds(cfg, params)
			if err != nil {
				// Log but do not abort the entire batch for one broken user.
				a.log.Warn("xray adapter: skipping user in bulk add due to build error",
					"email", user.Email, "err", err)
				continue
			}
			if err := AddUserToInbounds(cfg, payload); err != nil {
				a.log.Warn("xray adapter: skipping user in bulk add due to inbound write error",
					"email", user.Email, "err", err)
				continue
			}
			allPayloads = append(allPayloads, payload...)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("xray adapter AddUsersBulk: config write failed: %w", err)
	}

	// Single gRPC batch call for all users — best-effort.
	if len(allPayloads) > 0 {
		if err := a.grpc.AddUser(ctx, allPayloads, a.configPath); err != nil {
			a.log.Warn("xray adapter: bulk hot-add via gRPC failed (config already updated on disk)",
				"count", len(users), "err", err)
		}
	}

	// After hot-add, rebuild any hysteria2 inbounds (they don't support per-user add).
	a.rebuildHysteriaInbounds(ctx)

	a.log.Info("xray adapter: bulk user add completed", "requested", len(users), "hot_added", len(allPayloads))
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// RemoveUser — remove from config + hot-remove via gRPC
// ─────────────────────────────────────────────────────────────────────────────

// RemoveUser deprovisions a single user:
//  1. Resolves which inbound tags the user belongs to (needed for gRPC hot-remove).
//  2. Removes the user from the JSON config on disk (atomic write).
//  3. Hot-removes the user from the running Xray process via gRPC.
//
// If the user is not found in the config the method returns nil (idempotent).
// gRPC errors during hot-remove are non-fatal (same reasoning as AddUser).
//
// Dry-run scenarios verified:
//   - User not in config → noop, nil returned.
//   - Xray not running → gRPC warns, method still returns nil.
//   - Empty email → early error.
func (a *Adapter) RemoveUser(ctx context.Context, email string) error {
	if email == "" {
		return fmt.Errorf("xray adapter RemoveUser: email must not be empty")
	}

	var tags []string

	err := Modify(a.configPath, func(cfg RawConfig) error {
		exists, _ := UserExists(cfg, email)
		if !exists {
			// Idempotent: already gone.
			return nil
		}
		t, err := InboundTagsForUser(cfg, email)
		if err != nil {
			return fmt.Errorf("resolving inbound tags for %q: %w", email, err)
		}
		tags = t

		return RemoveUserFromAllInbounds(cfg, email)
	})
	if err != nil {
		return fmt.Errorf("xray adapter RemoveUser: config write failed: %w", err)
	}

	// Hot-remove — best-effort.
	if len(tags) > 0 {
		if err := a.grpc.RemoveUser(ctx, email, tags); err != nil {
			a.log.Warn("xray adapter: hot-remove via gRPC failed (config already updated on disk)",
				"email", email, "err", err)
		}
	}

	// After hot-remove, rebuild any hysteria2 inbounds (they don't support per-user remove).
	a.rebuildHysteriaInbounds(ctx)

	a.log.Info("xray adapter: user removed", "email", email)
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// RemoveUsersBulk — single config write + individual gRPC removals
// ─────────────────────────────────────────────────────────────────────────────

// RemoveUsersBulk removes multiple users in a single atomic config write,
// then hot-removes each user individually from the running Xray process.
//
// Individual gRPC failures are logged but do not abort the batch.
//
// Dry-run scenarios verified:
//   - Empty slice → returns nil immediately (no-op).
//   - Mix of present and absent users → only present ones are removed.
func (a *Adapter) RemoveUsersBulk(ctx context.Context, emails []string) error {
	if len(emails) == 0 {
		return nil
	}

	// Build a set for quick membership checks.
	emailSet := make(map[string]struct{}, len(emails))
	for _, e := range emails {
		if e != "" {
			emailSet[e] = struct{}{}
		}
	}

	// tagsByEmail is populated inside Modify so we can hot-remove after the config write.
	tagsByEmail := make(map[string][]string, len(emails))

	err := Modify(a.configPath, func(cfg RawConfig) error {
		// Resolve tags for all users in one pass before modifying the config.
		present := make([]string, 0, len(emails))
		for _, e := range emails {
			if e == "" {
				continue
			}
			exists, _ := UserExists(cfg, e)
			if !exists {
				continue
			}
			present = append(present, e)
		}
		if len(present) == 0 {
			return nil // All already gone — nothing to do.
		}

		tagsMap, err := InboundTagsForUsers(cfg, present)
		if err != nil {
			return fmt.Errorf("resolving inbound tags for bulk remove: %w", err)
		}
		for e, t := range tagsMap {
			tagsByEmail[e] = t
		}

		return RemoveUsersFromAllInbounds(cfg, present)
	})
	if err != nil {
		return fmt.Errorf("xray adapter RemoveUsersBulk: config write failed: %w", err)
	}

	// Hot-remove — best-effort per-user.
	for email, tags := range tagsByEmail {
		if len(tags) == 0 {
			continue
		}
		if err := a.grpc.RemoveUser(ctx, email, tags); err != nil {
			a.log.Warn("xray adapter: hot-remove via gRPC failed in bulk (config already updated on disk)",
				"email", email, "err", err)
		}
	}

	// After hot-remove, rebuild any hysteria2 inbounds (they don't support per-user remove).
	a.rebuildHysteriaInbounds(ctx)

	a.log.Info("xray adapter: bulk user remove completed", "count", len(tagsByEmail))
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// QueryStats — gRPC StatsService
// ─────────────────────────────────────────────────────────────────────────────

// QueryStats fetches per-user traffic counters from the running Xray process
// via the gRPC StatsService.QueryStats endpoint.
//
// The returned slice is never nil — callers can safely range over it.
//
// Dry-run scenarios verified:
//   - Xray not running → error propagated to caller so that the stats worker
//     can decide whether to skip this tick or alert.
//   - Zero users tracked → empty (non-nil) slice returned.
func (a *Adapter) QueryStats(ctx context.Context) ([]domain.TrafficStat, error) {
	raw, err := a.grpc.QueryStats(ctx)
	if err != nil {
		return []domain.TrafficStat{}, fmt.Errorf("xray adapter QueryStats: %w", err)
	}

	out := make([]domain.TrafficStat, 0, len(raw))
	for _, s := range raw {
		out = append(out, domain.TrafficStat{
			Email: s.Email,
			Up:    s.Up,
			Down:  s.Down,
		})
	}
	return out, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// RestartLogger — gRPC LoggerService
// ─────────────────────────────────────────────────────────────────────────────

// RestartLogger delegates to the Xray gRPC LoggerService.RestartLogger endpoint.
// This is the safe, zero-downtime mechanism for log rotation: no user connections
// are interrupted and Xray core is NOT restarted.
func (a *Adapter) RestartLogger(ctx context.Context) error {
	if err := a.grpc.RestartLogger(ctx); err != nil {
		return fmt.Errorf("xray adapter RestartLogger: %w", err)
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// ListUsers
// ─────────────────────────────────────────────────────────────────────────────

func (a *Adapter) ListUsers(_ context.Context) ([]domain.VPNUserConfig, error) {
	cfg, err := Read(a.configPath)
	if err != nil {
		return nil, fmt.Errorf("xray adapter ListUsers: failed to read config: %w", err)
	}
	rawUsers, err := ListUsers(cfg)
	if err != nil {
		return nil, fmt.Errorf("xray adapter ListUsers: failed to list users: %w", err)
	}

	users := make([]domain.VPNUserConfig, 0, len(rawUsers))
	for _, u := range rawUsers {
		authVal := u.GetString("auth")
		if authVal == "" {
			authVal = u.GetString("password")
		}

		var maxDevices int
		if lv, ok := u.GetNumber("limit"); ok {
			maxDevices = int(lv)
		}

		users = append(users, domain.VPNUserConfig{
			Email:      u.Email(),
			UUID:       u.GetString("id"),
			Auth:       authVal,
			Subfile:    u.GetString("subfile"),
			Expire:     u.GetString("expire"),
			MaxDevices: maxDevices,
		})
	}
	return users, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// SetExpire & SetLimit
// ─────────────────────────────────────────────────────────────────────────────

func (a *Adapter) SetExpire(ctx context.Context, email, expire string) error {
	var found bool
	err := Modify(a.configPath, func(cfg RawConfig) error {
		exists, _ := UserExists(cfg, email)
		if !exists {
			return nil
		}
		found = true
		return UpdateStringField(cfg, email, "expire", expire)
	})
	if err != nil {
		return fmt.Errorf("xray adapter SetExpire: config write failed: %w", err)
	}
	if !found {
		return fmt.Errorf("xray adapter SetExpire: user %q not found in config", email)
	}
	a.log.Info("xray adapter: expire updated", "email", email, "expire", expire)
	return nil
}

func (a *Adapter) SetLimit(ctx context.Context, email string, limit float64) error {
	var found bool
	err := Modify(a.configPath, func(cfg RawConfig) error {
		exists, _ := UserExists(cfg, email)
		if !exists {
			return nil
		}
		found = true
		return UpdateNumberField(cfg, email, "limit", limit)
	})
	if err != nil {
		return fmt.Errorf("xray adapter SetLimit: config write failed: %w", err)
	}
	if !found {
		return fmt.Errorf("xray adapter SetLimit: user %q not found in config", email)
	}
	a.log.Info("xray adapter: limit updated", "email", email, "limit", limit)
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// BanUser & UnbanUser (Anti-Fraud)
// ─────────────────────────────────────────────────────────────────────────────

// BanUser performs a soft ban by hot-removing the user from Xray's memory
// without touching the config file.
func (a *Adapter) BanUser(ctx context.Context, email string) error {
	cfg, err := Read(a.configPath)
	if err != nil {
		return fmt.Errorf("xray adapter BanUser: %w", err)
	}
	tags, err := InboundTagsForUser(cfg, email)
	if err != nil {
		return fmt.Errorf("xray adapter BanUser tags: %w", err)
	}
	if len(tags) == 0 {
		return nil // Not in any inbound
	}
	if err := a.grpc.RemoveUser(ctx, email, tags); err != nil {
		return fmt.Errorf("xray adapter BanUser grpc: %w", err)
	}
	a.log.Info("xray adapter: user soft-banned (removed from memory)", "email", email)
	return nil
}

// UnbanUser lifts a soft ban by hot-adding the user back into Xray's memory
// from the config file.
func (a *Adapter) UnbanUser(ctx context.Context, email string) error {
	cfg, err := Read(a.configPath)
	if err != nil {
		return fmt.Errorf("xray adapter UnbanUser: %w", err)
	}

	rawUser, err := FindUser(cfg, email)
	if err != nil || rawUser == nil {
		return fmt.Errorf("xray adapter UnbanUser: user %q not found in config", email)
	}

	// Create a dummy params just to build the payload for AddUserToInbounds.
	// Since we only need to hot-add, we use BuildForAllInbounds
	// based on the raw user data.
	var limit *float64
	if lv, ok := rawUser.GetNumber("limit"); ok {
		limit = &lv
	}
	authVal := rawUser.GetString("auth")
	if authVal == "" {
		authVal = rawUser.GetString("password")
	}

	params := ClientParams{
		Email:   rawUser.Email(),
		UUID:    rawUser.GetString("id"),
		Auth:    authVal,
		Subfile: rawUser.GetString("subfile"),
		Expire:  rawUser.GetString("expire"),
		Limit:   limit,
	}

	payload, err := BuildForAllInbounds(cfg, params)
	if err != nil {
		return fmt.Errorf("xray adapter UnbanUser build payload: %w", err)
	}

	if err := a.grpc.AddUser(ctx, payload, a.configPath); err != nil {
		return fmt.Errorf("xray adapter UnbanUser grpc: %w", err)
	}
	a.log.Info("xray adapter: user unbanned (added to memory)", "email", email)
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// SyncUsers — regenerate config + hot-sync with running xray
// ─────────────────────────────────────────────────────────────────────────────

// SyncUsers regenerates config.json from template + dbUsers, then diffs
// against the running xray process and hot-adds / hot-removes as needed.
// The config on disk is fully regenerated before the diff.
//
//   - removeOrphans=false is the safe default: missing users are added but
//     extra users in xray are never removed.
//   - removeOrphans=true cleans up users from xray that are not in dbUsers.
func (a *Adapter) SyncUsers(ctx context.Context, dbUsers []domain.VPNUserConfig, removeOrphans bool) (*domain.EngineSyncResult, error) {
	result := &domain.EngineSyncResult{}

	// 1. Regenerate config.json from template + DB users.
	if a.templatePath != "" {
		if err := RegenerateConfig(a.templatePath, a.configPath, dbUsers, a.realityRotation, a.realityKeysPath); err != nil {
			return result, fmt.Errorf("xray adapter SyncUsers: regenerate config: %w", err)
		}
		a.log.Info("xray adapter: config regenerated from template", "users", len(dbUsers))
	}

	// 2. Read current users from the live xray process.
	liveUsers, err := a.ListUsers(ctx)
	if err != nil {
		// If we can't read the live config, we can still hot-add everyone.
		a.log.Warn("xray adapter: SyncUsers: failed to list live users, will hot-add all", "err", err)
		liveUsers = nil
	}

	// 3. Build lookup sets.
	dbSet := make(map[string]domain.VPNUserConfig, len(dbUsers))
	for _, u := range dbUsers {
		if u.Email != "" {
			dbSet[u.Email] = u
		}
	}
	liveSet := make(map[string]struct{}, len(liveUsers))
	for _, u := range liveUsers {
		if u.Email != "" {
			liveSet[u.Email] = struct{}{}
		}
	}

	// 4. Hot-add users that are in DB but missing from xray.
	var toAdd []domain.VPNUserConfig
	for email, u := range dbSet {
		if _, ok := liveSet[email]; !ok {
			toAdd = append(toAdd, u)
		}
	}
	if len(toAdd) > 0 {
		if err := a.AddUsersBulk(ctx, toAdd); err != nil {
			a.log.Warn("xray adapter: SyncUsers: bulk hot-add failed", "count", len(toAdd), "err", err)
		} else {
			result.Added = len(toAdd)
		}
	}

	// 5. Hot-remove orphans (only when explicitly requested).
	if removeOrphans {
		var toRemove []string
		for _, u := range liveUsers {
			if _, ok := dbSet[u.Email]; !ok {
				toRemove = append(toRemove, u.Email)
			}
		}
		if len(toRemove) > 0 {
			if err := a.RemoveUsersBulk(ctx, toRemove); err != nil {
				a.log.Warn("xray adapter: SyncUsers: bulk hot-remove failed", "count", len(toRemove), "err", err)
			} else {
				result.Removed = len(toRemove)
			}
		}
	}

	// 6. Rebuild all hysteria2 inbounds to apply the full user list.
	a.rebuildHysteriaInbounds(ctx)

	a.log.Info("xray adapter: sync completed",
		"db_users", len(dbUsers),
		"live_users", len(liveUsers),
		"added", result.Added,
		"removed", result.Removed,
	)
	return result, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// RebuildInbound — hot-rebuild a hysteria2 inbound via remove + add
// ─────────────────────────────────────────────────────────────────────────────

// RebuildInbound hot-rebuilds a single inbound by its tag.
// It reads the current config.json, finds the inbound, serializes it to JSON,
// and calls the gRPC client to remove then re-add it.
// Best-effort: failures are logged but not fatal — the config on disk is correct.
func (a *Adapter) RebuildInbound(ctx context.Context, tag string) error {
	cfg, err := Read(a.configPath)
	if err != nil {
		return fmt.Errorf("xray adapter RebuildInbound: read config: %w", err)
	}

	inbounds, err := cfg.GetInbounds()
	if err != nil {
		return fmt.Errorf("xray adapter RebuildInbound: get inbounds: %w", err)
	}

	var target RawInbound
	found := false
	for _, ib := range inbounds {
		if ib.Tag() == tag {
			target = ib
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("xray adapter RebuildInbound: inbound %q not found in config", tag)
	}

	// Marshal the inbound to JSON as a single object.
	inboundJSON, err := json.Marshal(target)
	if err != nil {
		return fmt.Errorf("xray adapter RebuildInbound: marshal inbound: %w", err)
	}

	a.log.Info("xray adapter: hot-rebuilding inbound", "tag", tag, "protocol", target.Protocol())

	if err := a.grpc.RebuildInbound(ctx, tag, inboundJSON); err != nil {
		a.log.Warn("xray adapter: RebuildInbound via gRPC failed (config already on disk)",
			"tag", tag, "err", err)
		// Non-fatal: config on disk is correct.
	}

	return nil
}

// rebuildHysteriaInbounds reads the current config and hot-rebuilds every
// hysteria/hysteria2/hy2 inbound. Called after AddUser/RemoveUser operations
// because these protocols don't support per-user hot-add/hot-remove.
func (a *Adapter) rebuildHysteriaInbounds(ctx context.Context) {
	cfg, err := Read(a.configPath)
	if err != nil {
		a.log.Warn("xray adapter: rebuildHysteriaInbounds: read config failed", "err", err)
		return
	}

	inbounds, err := cfg.GetInbounds()
	if err != nil {
		a.log.Warn("xray adapter: rebuildHysteriaInbounds: get inbounds failed", "err", err)
		return
	}

	for _, ib := range inbounds {
		if !ib.IsHysteria() {
			continue
		}
		tag := ib.Tag()
		if tag == "" {
			continue
		}

		inboundJSON, err := json.Marshal(ib)
		if err != nil {
			a.log.Warn("xray adapter: rebuildHysteriaInbounds: marshal inbound failed",
				"tag", tag, "err", err)
			continue
		}

		a.log.Info("xray adapter: hot-rebuilding hysteria inbound", "tag", tag)
		if err := a.grpc.RebuildInbound(ctx, tag, inboundJSON); err != nil {
			a.log.Warn("xray adapter: rebuildHysteriaInbounds: gRPC failed",
				"tag", tag, "err", err)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Internal helpers
// ─────────────────────────────────────────────────────────────────────────────

// configToParams converts the engine-agnostic UserConfig into the Xray-specific
// ClientParams struct used by xrayconfig builders.
func configToParams(u domain.VPNUserConfig) ClientParams {
	var limit *float64
	if u.MaxDevices > 0 {
		v := float64(u.MaxDevices)
		limit = &v
	}
	return ClientParams{
		Email:   u.Email,
		UUID:    u.UUID,
		Auth:    u.Auth,
		Subfile: u.Subfile,
		Expire:  u.Expire,
		Flow:    u.Flow,
		Limit:   limit,
	}
}
