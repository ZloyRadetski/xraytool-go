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
// xray-core types directly. Business logic (lifecycle, API, user management)
// must use the Engine interface exclusively.
package engine_xray

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	json "github.com/goccy/go-json"
	"log/slog"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"

	"xraytool/internal/domain"
)

// Adapter implements Engine for Xray-core.
// It is safe for concurrent use: the underlying Modify already
// holds an exclusive process-level mutex + advisory flock on writes.
type Adapter struct {
	grpc              *GRPCClient
	configPath        string
	templatePath      string
	realityRotation   bool
	realityKeysPath   string
	log               *slog.Logger
	rebuildMu         sync.Mutex // Serializes dynamic inbound rebuild operations
	syncMu            sync.Mutex // Serializes all state modifications (disk and gRPC) to prevent races
	OnConfigModified  func()
	blacklistedAdmins []string
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
func NewAdapter(grpcAddr, configPath, templatePath string, realityRotation bool, realityKeysPath string, blacklistedAdmins []string, log *slog.Logger) *Adapter {
	return &Adapter{
		grpc:              NewGRPCClient(grpcAddr, log),
		configPath:        configPath,
		templatePath:      templatePath,
		realityRotation:   realityRotation,
		realityKeysPath:   realityKeysPath,
		blacklistedAdmins: blacklistedAdmins,
		log:               log.With("component", "xray-adapter"),
	}
}

func (a *Adapter) notifyConfigModified() {
	if a.OnConfigModified != nil {
		a.OnConfigModified()
	}
}

//nolint:unused
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
// Dirty State & Self-Healing Helpers
// ─────────────────────────────────────────────────────────────────────────────

func (a *Adapter) markDirty() {
	dirtyPath := a.configPath + ".dirty"
	if err := os.WriteFile(dirtyPath, []byte("1"), 0644); err != nil {
		a.log.Warn("xray adapter: failed to write dirty state file", "path", dirtyPath, "err", err)
	}
}

func (a *Adapter) isDirty() bool {
	dirtyPath := a.configPath + ".dirty"
	_, err := os.Stat(dirtyPath)
	return err == nil
}

func (a *Adapter) clearDirty() {
	dirtyPath := a.configPath + ".dirty"
	_ = os.Remove(dirtyPath)
}

func (a *Adapter) healDirty(ctx context.Context) {
	if a.isDirty() {
		a.log.Info("xray adapter: dirty state detected, restarting xray to sync with disk")
		err := exec.CommandContext(ctx, "systemctl", "restart", "xray").Run()
		if err != nil {
			a.log.Error("xray adapter: failed to restart xray for healing", "err", err)
		} else {
			a.clearDirty()
			a.log.Info("xray adapter: xray restarted successfully, dirty state cleared")
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// State Hashing Verification Helpers (Step 1)
// ─────────────────────────────────────────────────────────────────────────────

func (a *Adapter) calculateStateHash(dbUsers []domain.VPNUserConfig) (string, error) {
	h := sha256.New()

	// 1. Hash the template file if it exists
	if a.templatePath != "" {
		data, err := os.ReadFile(a.templatePath)
		if err == nil {
			h.Write(data)
		} else {
			h.Write([]byte("template_err"))
		}
	}
	// Replicated hardcoded clients live outside the user-owned template. Hash
	// the overlay so a changed artifact cannot be hidden by the ordinary DB
	// desired-state hash.
	if data, err := os.ReadFile(a.staticClientStatePath()); err == nil {
		h.Write([]byte("static:"))
		h.Write(data)
	}

	// 2. Hash reality rotation settings
	h.Write([]byte(fmt.Sprintf("rot:%t;keys:%s;", a.realityRotation, a.realityKeysPath)))
	if a.realityRotation && a.realityKeysPath != "" {
		data, err := os.ReadFile(a.realityKeysPath)
		if err == nil {
			h.Write(data)
		}
	}

	// 3. Hash blacklisted admins
	for _, admin := range a.blacklistedAdmins {
		h.Write([]byte("admin:" + admin + ";"))
	}

	// 4. Sort users by Email to ensure stable ordering
	sortedUsers := make([]domain.VPNUserConfig, len(dbUsers))
	copy(sortedUsers, dbUsers)
	sort.Slice(sortedUsers, func(i, j int) bool {
		return sortedUsers[i].Email < sortedUsers[j].Email
	})

	// 5. Hash each user's fields
	for _, u := range sortedUsers {
		h.Write([]byte(fmt.Sprintf("u:%s|%s|%s|%s|%s|%d;", u.Email, u.UUID, u.Auth, u.Subfile, u.Expire, u.MaxDevices)))
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

func (a *Adapter) invalidateHash() {
	hashPath := a.configPath + ".hash"
	_ = os.Remove(hashPath)
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
	a.syncMu.Lock()
	defer a.syncMu.Unlock()
	return a.addUserLocked(ctx, user)
}

func (a *Adapter) addUserLocked(ctx context.Context, user domain.VPNUserConfig) error {
	_ = context.WithoutCancel(ctx)
	if user.Email == "" {
		return fmt.Errorf("xray adapter AddUser: email must not be empty")
	}

	a.healDirty(ctx)

	oldCfg, _ := Read(a.configPath)

	var clientsPayload []TaggedClient

	err := Modify(a.configPath, func(cfg RawConfig) error {
		params := configToParams(user)
		payload, err := BuildForAllInbounds(cfg, params)
		if err != nil {
			return fmt.Errorf("building client payload: %w", err)
		}
		if len(payload) == 0 {
			return fmt.Errorf("no client inbounds found in xray config")
		}
		missing, err := MissingClientPayload(cfg, payload)
		if err != nil {
			return fmt.Errorf("finding missing client inbounds: %w", err)
		}
		if len(missing) == 0 {
			a.log.Info("xray adapter: user already present in every client inbound", "email", user.Email)
			return nil
		}

		if err := AddUserToInbounds(cfg, missing); err != nil {
			return fmt.Errorf("adding user to inbounds: %w", err)
		}

		clientsPayload = missing
		return nil
	})
	if err != nil {
		return fmt.Errorf("xray adapter AddUser: config write failed: %w", err)
	}

	// Hot-add — best-effort, non-fatal.
	if len(clientsPayload) > 0 {
		if err := a.grpc.AddUser(ctx, clientsPayload, a.configPath); err != nil {
			a.markDirty()
			a.log.Warn("xray adapter: hot-add via gRPC failed (config already updated on disk)",
				"email", user.Email, "err", err)
			a.rebuildInboundTags(ctx, clientsPayload, nil)
		}
	}

	// After hot-add, rebuild any hysteria2 inbounds (they don't support per-user add).
	a.rebuildHysteriaInbounds(ctx, oldCfg)

	a.invalidateHash()

	a.log.Info("xray adapter: user added", "email", user.Email)
	a.notifyConfigModified()
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
	a.syncMu.Lock()
	defer a.syncMu.Unlock()
	return a.addUsersBulkLocked(ctx, users)
}

func (a *Adapter) addUsersBulkLocked(ctx context.Context, users []domain.VPNUserConfig) error {
	_ = context.WithoutCancel(ctx)
	if len(users) == 0 {
		return nil
	}

	a.healDirty(ctx)

	oldCfg, _ := Read(a.configPath)

	var allPayloads []TaggedClient

	err := Modify(a.configPath, func(cfg RawConfig) error {
		for _, user := range users {
			if user.Email == "" {
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
			missing, err := MissingClientPayload(cfg, payload)
			if err != nil {
				a.log.Warn("xray adapter: skipping user in bulk add due to inbound lookup error",
					"email", user.Email, "err", err)
				continue
			}
			if len(missing) == 0 {
				continue
			}
			if err := AddUserToInbounds(cfg, missing); err != nil {
				a.log.Warn("xray adapter: skipping user in bulk add due to inbound write error",
					"email", user.Email, "err", err)
				continue
			}
			allPayloads = append(allPayloads, missing...)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("xray adapter AddUsersBulk: config write failed: %w", err)
	}

	// Single gRPC batch call for all users — best-effort.
	if len(allPayloads) > 0 {
		if err := a.grpc.AddUser(ctx, allPayloads, a.configPath); err != nil {
			a.markDirty()
			a.log.Warn("xray adapter: bulk hot-add via gRPC failed (config already updated on disk)",
				"count", len(users), "err", err)
			a.rebuildInboundTags(ctx, allPayloads, nil)
		}
	}

	// After hot-add, rebuild any hysteria2 inbounds (they don't support per-user add).
	a.rebuildHysteriaInbounds(ctx, oldCfg)

	a.invalidateHash()

	a.log.Info("xray adapter: bulk user add completed", "requested", len(users), "hot_added", len(allPayloads))
	a.notifyConfigModified()
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
	a.syncMu.Lock()
	defer a.syncMu.Unlock()
	return a.removeUserLocked(ctx, email)
}

func (a *Adapter) removeUserLocked(ctx context.Context, email string) error {
	_ = context.WithoutCancel(ctx)
	if email == "" {
		return fmt.Errorf("xray adapter RemoveUser: email must not be empty")
	}

	protected := a.getProtectedTemplateUsers(nil)
	if !shouldBypassProtection(ctx) && protected[email] {
		a.log.Info("xray adapter: skipping RemoveUser for protected template user", "email", email)
		return nil
	}

	a.healDirty(ctx)

	oldCfg, _ := Read(a.configPath)

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
			a.markDirty()
			a.log.Warn("xray adapter: hot-remove via gRPC failed (config already updated on disk)",
				"email", email, "err", err)
		}
	}

	// After hot-remove, rebuild any hysteria2 inbounds (they don't support per-user remove).
	a.rebuildHysteriaInbounds(ctx, oldCfg)

	a.invalidateHash()

	a.log.Info("xray adapter: user removed", "email", email)
	a.notifyConfigModified()
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
	a.syncMu.Lock()
	defer a.syncMu.Unlock()
	return a.removeUsersBulkLocked(ctx, emails)
}

func (a *Adapter) removeUsersBulkLocked(ctx context.Context, emails []string) error {
	_ = context.WithoutCancel(ctx)
	if len(emails) == 0 {
		return nil
	}

	protected := a.getProtectedTemplateUsers(nil)
	var nonProtected []string
	for _, e := range emails {
		if e != "" && (shouldBypassProtection(ctx) || !protected[e]) {
			nonProtected = append(nonProtected, e)
		} else if e != "" {
			a.log.Info("xray adapter: skipping RemoveUsersBulk for protected template user", "email", e)
		}
	}
	emails = nonProtected
	if len(emails) == 0 {
		return nil
	}

	a.healDirty(ctx)

	oldCfg, _ := Read(a.configPath)

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
			a.markDirty()
			a.log.Warn("xray adapter: hot-remove via gRPC failed in bulk (config already updated on disk)",
				"email", email, "err", err)
		}
	}

	// After hot-remove, rebuild any hysteria2 inbounds (they don't support per-user remove).
	a.rebuildHysteriaInbounds(ctx, oldCfg)

	a.invalidateHash()

	a.log.Info("xray adapter: bulk user remove completed", "count", len(tagsByEmail))
	a.notifyConfigModified()
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
	a.syncMu.Lock()
	defer a.syncMu.Unlock()
	//nolint:ineffassign
	_ = context.WithoutCancel(ctx) //nolint:ineffassign //nolint:staticcheck //nolint:staticcheck //nolint:staticcheck
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
	a.invalidateHash()
	a.log.Info("xray adapter: expire updated", "email", email, "expire", expire)
	a.notifyConfigModified()
	return nil
}

func (a *Adapter) SetLimit(ctx context.Context, email string, limit float64) error {
	a.syncMu.Lock()
	defer a.syncMu.Unlock()
	//nolint:ineffassign
	_ = context.WithoutCancel(ctx) //nolint:ineffassign //nolint:staticcheck //nolint:staticcheck //nolint:staticcheck
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
	a.invalidateHash()
	a.log.Info("xray adapter: limit updated", "email", email, "limit", limit)
	a.notifyConfigModified()
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// BanUser & UnbanUser (Anti-Fraud)
// ─────────────────────────────────────────────────────────────────────────────

// BanUser performs a soft ban by hot-removing the user from Xray's memory
// without touching the config file.
func (a *Adapter) BanUser(ctx context.Context, email string) error {
	a.syncMu.Lock()
	defer a.syncMu.Unlock()

	_ = context.WithoutCancel(ctx)
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
		a.markDirty()
		return fmt.Errorf("xray adapter BanUser grpc: %w", err)
	}
	a.invalidateHash()
	a.log.Info("xray adapter: user soft-banned (removed from memory)", "email", email)
	return nil
}

// UnbanUser lifts a soft ban by hot-adding the user back into Xray's memory
// from the config file.
func (a *Adapter) UnbanUser(ctx context.Context, email string) error {
	a.syncMu.Lock()
	defer a.syncMu.Unlock()

	_ = context.WithoutCancel(ctx)
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
		a.markDirty()
		return fmt.Errorf("xray adapter UnbanUser grpc: %w", err)
	}
	a.invalidateHash()
	a.log.Info("xray adapter: user unbanned (restored memory)", "email", email)
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
	a.syncMu.Lock()
	defer a.syncMu.Unlock()
	return a.syncUsersLocked(ctx, dbUsers, removeOrphans)
}

// ReconcileUsers deliberately bypasses the cached desired-state hash. Slave
// replication uses it to repair an edited generated config even when the
// desired users in the database have not changed.
func (a *Adapter) ReconcileUsers(ctx context.Context, dbUsers []domain.VPNUserConfig) (*domain.EngineSyncResult, error) {
	a.syncMu.Lock()
	defer a.syncMu.Unlock()
	a.invalidateHash()
	return a.syncUsersLocked(ctx, dbUsers, true)
}

func (a *Adapter) syncUsersLocked(ctx context.Context, dbUsers []domain.VPNUserConfig, removeOrphans bool) (*domain.EngineSyncResult, error) {
	_ = context.WithoutCancel(ctx)
	result := &domain.EngineSyncResult{}

	a.healDirty(ctx)

	// Calculate desired state hash
	dbHash, err := a.calculateStateHash(dbUsers)
	if err == nil {
		hashPath := a.configPath + ".hash"
		if localHashBytes, err := os.ReadFile(hashPath); err == nil {
			if string(localHashBytes) == dbHash && !a.isDirty() {
				a.log.Info("xray adapter: state hash matches local hash, skipping sync")
				return &domain.EngineSyncResult{Added: 0, Removed: 0}, nil
			}
		}
	}

	// 1. Read current users from the live xray process (before overwriting configPath).
	liveUsers, err := a.ListUsers(ctx)
	if err != nil {
		a.log.Warn("xray adapter: SyncUsers: failed to list live users, will hot-add all", "err", err)
		liveUsers = nil
	}

	oldCfg, _ := Read(a.configPath)

	dbEmails := make(map[string]bool, len(dbUsers))
	for _, u := range dbUsers {
		if u.Email != "" {
			dbEmails[u.Email] = true
		}
	}

	// Load static template users to protect them from being marked as orphans
	templateSet := a.getProtectedTemplateUsers(dbEmails)

	// 2. Regenerate config.json from template + DB users.
	if a.templatePath != "" {
		if err := RegenerateConfig(a.templatePath, a.configPath, dbUsers, a.realityRotation, a.realityKeysPath, a.blacklistedAdmins); err != nil {
			return result, fmt.Errorf("xray adapter SyncUsers: regenerate config: %w", err)
		}
		if err := a.restoreStaticClientsToGeneratedConfig(); err != nil {
			return result, fmt.Errorf("xray adapter SyncUsers: restore replicated static clients: %w", err)
		}
		a.log.Info("xray adapter: config regenerated from template", "users", len(dbUsers))
	} else if len(dbUsers) > 0 {
		// Installations without a template keep their current config as the
		// desired shape. In that mode addUsersBulkLocked performs the same
		// per-inbound repair directly against the current config.
		if err := a.addUsersBulkLocked(ctx, dbUsers); err != nil {
			a.log.Warn("xray adapter: SyncUsers: bulk hot-add failed", "count", len(dbUsers), "err", err)
		} else {
			result.Added = len(dbUsers)
		}
	}

	// 3. Read the regenerated desired config and compare it to the pre-sync
	// configuration by (inbound tag, email). A single email-level set is not
	// sufficient: the user may exist in one inbound and still be absent from
	// another one.
	desiredCfg, err := Read(a.configPath)
	if err != nil {
		return result, fmt.Errorf("xray adapter SyncUsers: read regenerated config: %w", err)
	}
	var toAdd []TaggedClient
	toUpdateRemove := make(map[string][]string)
	if a.templatePath != "" {
		desiredPayload, err := DesiredUserClientPayload(desiredCfg, dbEmails)
		if err != nil {
			return result, fmt.Errorf("xray adapter SyncUsers: read desired inbound clients: %w", err)
		}
		toAdd, toUpdateRemove, err = DiffClientPayload(oldCfg, desiredPayload)
		if err != nil {
			return result, fmt.Errorf("xray adapter SyncUsers: diff inbound clients: %w", err)
		}
	}

	// 4. Build lookup sets for orphan handling. The values are deliberately
	// still one-per-email here; orphan removal is about ownership, not inbound
	// membership, which was handled by the tag-aware diff above.
	dbSet := make(map[string]domain.VPNUserConfig, len(dbUsers))
	for _, u := range dbUsers {
		if u.Email != "" {
			dbSet[u.Email] = u
		}
	}

	// Apply changed clients first by removing the old client from only the
	// affected inbound, then adding the exact generated client to that inbound.
	// When an Xray API operation reports a partial failure, rebuild precisely the
	// involved inbound from the just-written config instead of leaving it stale.
	repairRemovals := make(map[string][]string)
	for email, tags := range toUpdateRemove {
		if err := a.grpc.RemoveUser(ctx, email, tags); err != nil {
			a.markDirty()
			a.log.Warn("xray adapter: SyncUsers: hot-remove for inbound update failed", "email", email, "tags", tags, "err", err)
			repairRemovals[email] = append(repairRemovals[email], tags...)
		}
	}
	if len(toAdd) > 0 {
		if err := a.grpc.AddUser(ctx, toAdd, a.configPath); err != nil {
			a.markDirty()
			a.log.Warn("xray adapter: SyncUsers: hot-add for inbound reconciliation failed", "clients", len(toAdd), "err", err)
			a.rebuildInboundTags(ctx, toAdd, repairRemovals)
		} else {
			seen := make(map[string]bool, len(toAdd))
			for _, tagged := range toAdd {
				if email := tagged.Client.Email(); email != "" && !seen[email] {
					seen[email] = true
					result.Added++
				}
			}
		}
	} else if len(repairRemovals) > 0 {
		a.rebuildInboundTags(ctx, nil, repairRemovals)
	}

	// 5. Hot-remove orphans (only when explicitly requested).
	if removeOrphans {
		var toRemove []string
		for _, u := range liveUsers {
			if _, ok := dbSet[u.Email]; !ok {
				if !templateSet[u.Email] {
					toRemove = append(toRemove, u.Email)
				}
			}
		}
		if len(toRemove) > 0 {
			// Safety threshold: Circuit Breaker
			threshold := int(float64(len(liveUsers)) * 0.3)
			if threshold < 10 {
				threshold = 10
			}
			if len(toRemove) > threshold {
				a.log.Error("CRITICAL: SyncUsers safety threshold exceeded! Too many orphans to remove. Database might be corrupted or empty.",
					"live_count", len(liveUsers),
					"to_remove_count", len(toRemove),
					"threshold", threshold,
				)
				return result, fmt.Errorf("sync aborted: safety threshold exceeded (want to remove %d out of %d users, threshold is %d)",
					len(toRemove), len(liveUsers), threshold)
			}

			oldTags, tagsErr := InboundTagsForUsers(oldCfg, toRemove)
			if tagsErr != nil {
				return result, fmt.Errorf("xray adapter SyncUsers: resolve orphan inbound tags: %w", tagsErr)
			}
			failedRemovals := make(map[string][]string)
			for email, tags := range oldTags {
				if err := a.grpc.RemoveUser(ctx, email, tags); err != nil {
					a.markDirty()
					a.log.Warn("xray adapter: SyncUsers: hot-remove orphan failed", "email", email, "tags", tags, "err", err)
					failedRemovals[email] = append(failedRemovals[email], tags...)
				}
			}
			if len(failedRemovals) > 0 {
				a.rebuildInboundTags(ctx, nil, failedRemovals)
			} else {
				result.Removed = len(toRemove)
			}
		}
	}

	// 6. Rebuild all hysteria2 inbounds to apply the full user list.
	a.rebuildHysteriaInbounds(ctx, oldCfg)

	// Save the new successfully applied state hash
	if err == nil {
		hashPath := a.configPath + ".hash"
		if err := os.WriteFile(hashPath, []byte(dbHash), 0644); err != nil {
			a.log.Warn("xray adapter: failed to write state hash", "err", err)
		}
	}

	a.log.Info("xray adapter: sync completed",
		"db_users", len(dbUsers),
		"live_users", len(liveUsers),
		"added", result.Added,
		"removed", result.Removed,
	)
	a.notifyConfigModified()
	return result, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// RebuildInbound — hot-rebuild a hysteria2 inbound via remove + add
// ─────────────────────────────────────────────────────────────────────────────

// RebuildInbound hot-rebuilds a single inbound by its tag.
// It reads the current config.json, finds the inbound, serializes it to JSON,
// and calls the gRPC client to remove then re-add it.
func (a *Adapter) RebuildInbound(ctx context.Context, tag string) error {
	a.syncMu.Lock()
	defer a.syncMu.Unlock()
	return a.rebuildInboundLocked(ctx, tag)
}

func (a *Adapter) rebuildInboundLocked(ctx context.Context, tag string) error {
	_ = context.WithoutCancel(ctx)

	cfg, err := Read(a.configPath)
	if err != nil {
		return fmt.Errorf("xray adapter RebuildInbound: read config: %w", err)
	}

	inbounds, err := cfg.GetInbounds()
	if err != nil {
		return fmt.Errorf("xray adapter RebuildInbound: get inbounds: %w", err)
	}

	var found RawInbound
	for _, ib := range inbounds {
		if ib.Tag() == tag {
			found = ib
			break
		}
	}

	if found == nil {
		return fmt.Errorf("xray adapter RebuildInbound: inbound %q not found in config", tag)
	}

	inboundJSON, err := json.Marshal(found)
	if err != nil {
		return fmt.Errorf("xray adapter RebuildInbound: marshal inbound: %w", err)
	}

	if err := a.grpc.RebuildInbound(ctx, tag, inboundJSON); err != nil {
		a.markDirty()
		a.log.Warn("xray adapter: RebuildInbound via gRPC failed (config already on disk)",
			"tag", tag, "err", err)
	}

	return nil
}

// rebuildInboundTags is the last-resort repair path for a partial Xray gRPC
// result. The config on disk is already the desired state, so rebuilding only
// the affected inbounds makes the runtime match it without restarting Xray or
// disturbing unrelated inbounds.
func (a *Adapter) rebuildInboundTags(ctx context.Context, additions []TaggedClient, removals map[string][]string) {
	tagSet := make(map[string]bool, len(additions))
	for _, tagged := range additions {
		if tagged.Tag != "" {
			tagSet[tagged.Tag] = true
		}
	}
	for _, tags := range removals {
		for _, tag := range tags {
			if tag != "" {
				tagSet[tag] = true
			}
		}
	}

	tags := make([]string, 0, len(tagSet))
	for tag := range tagSet {
		tags = append(tags, tag)
	}
	sort.Strings(tags)
	for _, tag := range tags {
		if err := a.rebuildInboundLocked(ctx, tag); err != nil {
			a.log.Warn("xray adapter: failed to rebuild inbound after partial gRPC sync", "tag", tag, "err", err)
		}
	}
}

// rebuildHysteriaInbounds reads the current config and hot-rebuilds every
// hysteria/hysteria2/hy2 inbound if it has changed compared to oldCfg.
// Called after AddUser/RemoveUser operations because these protocols don't
// support per-user hot-add/hot-remove.
func (a *Adapter) rebuildHysteriaInbounds(ctx context.Context, oldCfg RawConfig) {
	a.rebuildMu.Lock()
	defer a.rebuildMu.Unlock()

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

	// Helper to find inbound by tag in oldCfg
	getOldInbound := func(tag string) (RawInbound, bool) {
		if oldCfg == nil {
			return nil, false
		}
		oldInbounds, err := oldCfg.GetInbounds()
		if err != nil {
			return nil, false
		}
		for _, ib := range oldInbounds {
			if ib.Tag() == tag {
				return ib, true
			}
		}
		return nil, false
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

		// Check if the inbound is unchanged compared to oldCfg
		if oldIb, ok := getOldInbound(tag); ok {
			oldJSON, err := json.Marshal(oldIb)
			if err == nil && string(inboundJSON) == string(oldJSON) {
				a.log.Debug("xray adapter: hysteria inbound unchanged, skipping rebuild", "tag", tag)
				continue
			}
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

func (a *Adapter) SyncRealityKeys(ctx context.Context, keysBytes []byte) error {
	a.syncMu.Lock()
	defer a.syncMu.Unlock()
	return a.syncRealityKeysLocked(ctx, keysBytes)
}

func (a *Adapter) syncRealityKeysLocked(ctx context.Context, keysBytes []byte) error {
	_ = context.WithoutCancel(ctx)

	// 1. Unmarshal keys
	var keys RealityKeys
	if err := json.Unmarshal(keysBytes, &keys); err != nil {
		return fmt.Errorf("sync reality keys: unmarshal: %w", err)
	}
	if keys.PrivateKey == "" || keys.PublicKey == "" || len(keys.ShortIDs) == 0 {
		return fmt.Errorf("sync reality keys: invalid key structure")
	}

	// 2. Modify config.json on disk to inject these keys
	err := Modify(a.configPath, func(cfg RawConfig) error {
		return injectRealityKeys(cfg, &keys)
	})
	if err != nil {
		return fmt.Errorf("sync reality keys: modify config on disk: %w", err)
	}

	// 3. Find all VLESS/xhttp/splithttp inbounds that use reality and rebuild them
	cfg, err := Read(a.configPath)
	if err != nil {
		return fmt.Errorf("sync reality keys: read config: %w", err)
	}

	inbounds, err := cfg.GetInbounds()
	if err != nil {
		return fmt.Errorf("sync reality keys: get inbounds: %w", err)
	}

	for _, ib := range inbounds {
		proto := ib.Protocol()
		if proto != "vless" && proto != "xhttp" && proto != "splithttp" {
			continue
		}

		rawStream, ok := ib["streamSettings"]
		if !ok {
			continue
		}

		var stream map[string]json.RawMessage
		if err := json.Unmarshal(rawStream, &stream); err != nil {
			continue
		}

		rawSec, ok := stream["security"]
		if !ok {
			continue
		}
		var sec string
		if err := json.Unmarshal(rawSec, &sec); err != nil || sec != "reality" {
			continue
		}

		// Rebuild this inbound
		tag := ib.Tag()
		if tag != "" {
			if err := a.rebuildInboundLocked(ctx, tag); err != nil {
				a.log.Warn("sync reality keys: failed to rebuild inbound", "tag", tag, "err", err)
			}
		}
	}

	a.invalidateHash()
	a.notifyConfigModified()
	return nil
}

type bypassKey struct{}

func WithBypassProtection(ctx context.Context, bypass bool) context.Context {
	return context.WithValue(ctx, bypassKey{}, bypass)
}

func shouldBypassProtection(ctx context.Context) bool {
	val, ok := ctx.Value(bypassKey{}).(bool)
	return ok && val
}

func (a *Adapter) getProtectedTemplateUsers(dbEmails map[string]bool) map[string]bool {
	protected := make(map[string]bool)
	blacklist := make(map[string]bool)
	for _, email := range a.blacklistedAdmins {
		if email != "" {
			blacklist[email] = true
		}
	}

	if a.templatePath != "" {
		templateCfg, err := Read(a.templatePath)
		if err == nil {
			if templateUsers, err := ListUsers(templateCfg); err == nil {
				for _, u := range templateUsers {
					email := u.Email()
					if email != "" && !blacklist[email] {
						if dbEmails == nil || !dbEmails[email] {
							protected[email] = true
						}
					}
				}
			}
		}
	}
	// Both direct-config static clients and replicated template overlays are
	// persisted in the same sidecar. They must be protected from orphan cleanup
	// even though the overlay never changes the template.
	if staticClients, err := a.readStaticClientState(); err == nil {
		for _, clients := range staticClients {
			for _, client := range clients {
				email := client.Email()
				if email != "" && !blacklist[email] && (dbEmails == nil || !dbEmails[email]) {
					protected[email] = true
				}
			}
		}
	}
	return protected
}
