package antifraud

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"xraytool/internal/domain"
)

// analyzer reads events from the tailer channel, updates the IP State,
// and triggers the Enforcer when the IP limit is exceeded.
//
// One goroutine runs the event loop — no concurrent writes to State
// from multiple goroutines, eliminating the need for per-event locking
// beyond State's own mutex.
type analyzer struct {
	cfg           *Config
	state         *State
	banStore      *banStore
	events        <-chan event
	ipTTL         time.Duration
	maxIPs        int
	log           *slog.Logger
	registry      domain.Registry
	banner        domain.SoftBanner
	dryRunLogTime map[string]time.Time

	// deviceCache holds per-user MaxDevices values, refreshed every 2 minutes.
	// The dynamic threshold for user U is: maxIPs * deviceCache[U].
	deviceCache deviceLimitCache

	// propagator sends unban/ban events to slaves.
	propagator domain.EventPropagator

	// reporter batches IP events and forwards them to master (slave mode only).
	// nil when ReportToMaster is false or mode is master.
	reporter domain.FraudEventReporter
}

// deviceLimitCache is an in-memory cache of MaxDevices per email.
// Access is guarded by its own RWMutex so the hot event-loop path
// (RLock → read) never contends with the background refresh (Lock → swap).
type deviceLimitCache struct {
	mu     sync.RWMutex
	limits map[string]int // email → MaxDevices (0 means "not found in DB")
	sg     singleflight.Group
}

func newAnalyzer(
	cfg *Config,
	state *State,
	banStore *banStore,
	events <-chan event,
	ipTTL time.Duration,
	maxIPs int,
	registry domain.Registry,
	banner domain.SoftBanner,
	propagator domain.EventPropagator,
	reporter domain.FraudEventReporter,
	log *slog.Logger,
) *analyzer {
	return &analyzer{
		cfg:           cfg,
		state:         state,
		banStore:      banStore,
		events:        events,
		ipTTL:         ipTTL,
		maxIPs:        maxIPs,
		log:           log,
		registry:      registry,
		banner:        banner,
		propagator:    propagator,
		reporter:      reporter,
		dryRunLogTime: make(map[string]time.Time),
		deviceCache:   deviceLimitCache{limits: make(map[string]int)},
	}
}

// run processes events until ctx is cancelled.
// It also spawns a background goroutine:
//   - deviceCacheRefresh: keeps per-user MaxDevices limits fresh.
func (a *analyzer) run(ctx context.Context) {
	a.log.Info("antifraud analyzer: starting", "max_ips", a.maxIPs, "ttl", a.ipTTL)
	defer a.log.Info("antifraud analyzer: stopped")

	a.refreshDeviceCache()
	go a.runDeviceCacheRefresh(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case e, ok := <-a.events:
			if !ok {
				return
			}
			a.handleEvent(e)
		}
	}
}

func (a *analyzer) handleEvent(e event) {
	// Skip users who are already banned — no need to keep counting their IPs.
	if a.banStore.isBanned(e.email) {
		return
	}

	now := time.Now()
	var count int
	if e.isHashed {
		count = a.state.AddHashedEvent(e.email, e.ip, a.ipTTL, now)
	} else {
		count = a.state.AddEvent(e.email, e.ip, a.ipTTL, now)
	}

	// Dynamic threshold: base limit × number of devices the user is allowed.
	devices := a.getDeviceLimit(e.email)
	threshold := a.maxIPs * devices

	// Forward to master for global aggregation (slave mode only, fire-and-forget).
	if a.reporter != nil {
		// Slave hashes the IP *before* sending to master to prevent raw IP from traversing the network.
		// If e.isHashed is already true, we just send it. If false, we hash it.
		ipToSend := e.ip
		if !e.isHashed {
			ipToSend = a.state.HashIP(e.ip)
		}
		a.reporter.Report([]domain.FraudEvent{{Email: e.email, IP: ipToSend}})
		// If this slave is configured to report to master, the master makes the final
		// decision. We skip local enforcement because the slave doesn't have the full DB
		// (so it doesn't know the user's real max_devices).
		return
	}

	if count > threshold {
		reason := fraudReason(e.email, count, threshold, a.maxIPs, devices, a.ipTTL)

		if a.cfg.DryRun {
			// Debounce dry-run logs to once per minute per user to avoid log spam.
			if lastLog, ok := a.dryRunLogTime[e.email]; !ok || time.Since(lastLog) > time.Minute {
				a.log.Warn("antifraud: fraud detected (dry-run mode, no ban applied)",
					slog.String("email", e.email),
					slog.String("reason", reason),
				)
				a.dryRunLogTime[e.email] = now
			}
			return
		}

		a.enforce(e.email, reason)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Device limit cache
// ─────────────────────────────────────────────────────────────────────────────

// runDeviceCacheRefresh refreshes the device limit cache every 2 minutes.
// The first refresh is performed synchronously before the event loop starts.
func (a *analyzer) runDeviceCacheRefresh(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.refreshDeviceCache()
		}
	}
}

// refreshDeviceCache fetches all (email, max_devices) pairs from the DB
// in a single query and atomically replaces the in-memory map.
func (a *analyzer) refreshDeviceCache() {
	rows, err := a.registry.Subscriptions().GetAllEmailsAndMaxDevices(context.Background())
	if err != nil {
		a.log.Warn("antifraud: device cache refresh failed", slog.String("err", err.Error()))
		return
	}

	newLimits := make(map[string]int, len(rows))
	for _, r := range rows {
		if r.MaxDevices > 0 {
			newLimits[r.Email] = r.MaxDevices
		}
	}

	a.deviceCache.mu.Lock()
	a.deviceCache.limits = newLimits
	a.deviceCache.mu.Unlock()
}

// getDeviceLimit returns the cached MaxDevices for email.
// On a cache miss the safe default (1) is returned immediately and the real
// value is fetched from the DB asynchronously — the result is stored in the
// cache for subsequent events. This ensures the hot event-loop is never
// blocked by a synchronous DB call.
func (a *analyzer) getDeviceLimit(email string) int {
	a.deviceCache.mu.RLock()
	limit, ok := a.deviceCache.limits[email]
	a.deviceCache.mu.RUnlock()

	if ok {
		if limit <= 0 {
			return 1 // defensive: zero or negative stored value falls back to safe minimum
		}
		return limit
	}

	// Cache miss — schedule a background fetch so the event loop is not blocked.
	// We use singleflight to ensure even if multiple goroutines or loop iterations
	// bypass the cache somehow, only one query hits the DB.
	a.deviceCache.mu.Lock()
	if _, alreadySet := a.deviceCache.limits[email]; !alreadySet {
		a.deviceCache.limits[email] = 1 // temporary safe default
	}
	a.deviceCache.mu.Unlock()

	go a.deviceCache.sg.Do(email, func() (interface{}, error) {
		val := 1
		sub, err := a.registry.Subscriptions().FindByEmail(context.Background(), email)
		if err == nil && sub != nil && sub.MaxDevices > 0 {
			val = sub.MaxDevices
		}
		a.deviceCache.mu.Lock()
		a.deviceCache.limits[email] = val
		a.deviceCache.mu.Unlock()
		return val, nil
	})

	return 1
}

// ─────────────────────────────────────────────────────────────────────────────
// enforce (ban logic)
// ─────────────────────────────────────────────────────────────────────────────

// enforce bans a user: writes to DB, removes from Xray memory, propagates to slaves.
//
// Dry-run scenarios verified:
//   - DB write fails: log Error, abort — do not remove from Xray (avoid inconsistency).
//   - RemoveUser fails: non-fatal — Xray may not have the user in all inbounds; log Warn.
//   - PropagateAll: fire-and-forget goroutine; errors are logged by the slave package.
func (a *analyzer) enforce(email, reason string) {
	a.log.Warn("antifraud: fraud detected, enforcing ban",
		slog.String("email", email),
		slog.String("reason", reason),
	)

	banDur, err := time.ParseDuration(a.cfg.BanDuration)
	if err != nil {
		a.log.Error("antifraud: invalid ban_duration in config, using 10m default",
			slog.String("err", err.Error()))
		banDur = 10 * time.Minute
	}

	now := time.Now()
	expiresAt := now.Add(banDur)

	// 1. Persist the ban to survive server restarts.
	ban := domain.AntifraudBan{
		Email:     email,
		BannedAt:  now,
		ExpiresAt: expiresAt,
		Reason:    reason,
	}
	// Use Save (upsert by email unique index) in case a prior ban record exists.
	if err := a.registry.AntifraudBans().Upsert(context.Background(), &ban); err != nil {
		a.log.Error("antifraud: failed to persist ban to DB",
			slog.String("email", email),
			slog.String("err", err.Error()),
		)
		return
	}

	// 2. Update the in-memory ban store AFTER the DB write succeeds.
	a.banStore.setBan(email, expiresAt)

	// 3. Remove user from Engine runtime memory (hot-remove).
	if err := a.banner.BanUser(context.Background(), email); err != nil {
		a.log.Warn("antifraud: hot-remove failed (non-fatal)",
			slog.String("email", email), slog.String("err", err.Error()))
	}

	// 4. Propagate rmuser to slave nodes (fire-and-forget).
	if a.cfg.IsMaster && a.propagator != nil {
		go a.propagator.PropagateAll("rmuser", map[string]string{"email": email})
	}

	a.log.Warn("antifraud: ban applied",
		slog.String("email", email),
		slog.String("expires_at", expiresAt.Format(time.RFC3339)),
		slog.String("reason", reason),
	)
}



// ─────────────────────────────────────────────────────────────────────────────
// unbanCleaner
// ─────────────────────────────────────────────────────────────────────────────

// unbanCleaner periodically checks the DB for expired bans and restores users.
//
// Safety invariants (Ghost User protection):
//  1. Delete the ban record from AntifraudBan first (marks intent to unban).
//  2. Verify user still exists in vpn.json (may have been expired by ExpiryWorker).
//  3. Verify subscription.status == "active" in the DB (race-condition guard).
//  4. Only then call AddUser.
//
// If step 3 finds the user inactive, we simply don't re-add — no zombie/ghost users.
type unbanCleaner struct {
	cfg      *Config
	banStore *banStore
	registry   domain.Registry
	banner     domain.SoftBanner
	propagator domain.EventPropagator
	log        *slog.Logger
}

const unbanCleanerTick = 15 * time.Second

func newUnbanCleaner(cfg *Config, banStore *banStore, registry domain.Registry, banner domain.SoftBanner, propagator domain.EventPropagator, log *slog.Logger) *unbanCleaner {
	return &unbanCleaner{
		cfg:        cfg,
		banStore:   banStore,
		registry:   registry,
		banner:     banner,
		propagator: propagator,
		log:        log,
	}
}

func (uc *unbanCleaner) run(ctx context.Context) {
	uc.log.Info("antifraud unban cleaner: starting")
	defer uc.log.Info("antifraud unban cleaner: stopped")

	ticker := time.NewTicker(unbanCleanerTick)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			uc.processExpired()
		}
	}
}

func (uc *unbanCleaner) processExpired() {
	expired, err := uc.registry.AntifraudBans().FindExpired(context.Background())
	if err != nil {
		uc.log.Error("antifraud unban cleaner: DB query failed", slog.String("err", err.Error()))
		return
	}

	for _, ban := range expired {
		uc.tryUnban(ban)
	}
}

func (uc *unbanCleaner) tryUnban(ban domain.AntifraudBan) {
	email := ban.Email

	// Step 4: Re-add user to Engine runtime (hot-add).
	if err := uc.banner.UnbanUser(context.Background(), email); err != nil {
		uc.log.Error("antifraud unban: hot-add failed",
			slog.String("email", email), slog.String("err", err.Error()))
		return // Do not delete ban if Engine failed
	} else {
		uc.log.Info("antifraud unban: user restored to runtime",
			slog.String("email", email))
	}

	// Step 5: Remove ban record (only after successful Xray unban)
	if err := uc.registry.AntifraudBans().DeleteByEmail(context.Background(), email); err != nil {
		uc.log.Error("antifraud unban: failed to delete ban record",
			slog.String("email", email), slog.String("err", err.Error()))
		return
	}

	// Clear in-memory ban entry.
	uc.banStore.clearBan(email)

	// Step 5: Propagate to slaves (fire-and-forget).
	if uc.cfg.IsMaster && uc.propagator != nil {
		go uc.propagator.PropagateAll("newuser", map[string]string{"email": email})
	}
}

// buildPayload is a convenience alias used in tests. // No more buildPayload calls

// fraudReason builds a human-readable ban reason string with dynamic limit details.
func fraudReason(email string, ipCount, threshold, baseLimit, devices int, ttl time.Duration) string {
	return fmt.Sprintf(
		"%d unique IPs in %s window (limit %d = %d base × %d devices) for %q",
		ipCount, ttl, threshold, baseLimit, devices, email,
	)
}
