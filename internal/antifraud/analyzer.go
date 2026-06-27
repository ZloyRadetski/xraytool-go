package antifraud

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"gorm.io/gorm"

	"xraytool/internal/appconfig"
	"xraytool/internal/database"
	"xraytool/internal/slave"
	"xraytool/internal/xrayapi"
	"xraytool/internal/xrayconfig"
)

// analyzer reads events from the tailer channel, updates the IP State,
// and triggers the Enforcer when the IP limit is exceeded.
//
// One goroutine runs the event loop — no concurrent writes to State
// from multiple goroutines, eliminating the need for per-event locking
// beyond State's own mutex.
type analyzer struct {
	cfg      *appconfig.Config
	state    *State
	banStore *banStore
	events   <-chan event
	ipTTL    time.Duration
	maxIPs   int
	log           *slog.Logger
	db            *gorm.DB
	dryRunLogTime map[string]time.Time

	// deviceCache holds per-user MaxDevices values, refreshed every 2 minutes.
	// The dynamic threshold for user U is: maxIPs * deviceCache[U].
	deviceCache deviceLimitCache

	// reporter batches IP events and forwards them to master (slave mode only).
	// nil when ReportToMaster is false or mode is master.
	reporter *slaveReporter
}

// deviceLimitCache is an in-memory cache of MaxDevices per email.
// Access is guarded by its own RWMutex so the hot event-loop path
// (RLock → read) never contends with the background refresh (Lock → swap).
type deviceLimitCache struct {
	mu     sync.RWMutex
	limits map[string]int // email → MaxDevices (0 means "not found in DB")
}

func newAnalyzer(
	cfg *appconfig.Config,
	state *State,
	bs *banStore,
	events <-chan event,
	ipTTL time.Duration,
	maxIPs int,
	db *gorm.DB,
	log *slog.Logger,
) *analyzer {
	a := &analyzer{
		cfg:      cfg,
		state:    state,
		banStore: bs,
		events:   events,
		ipTTL:    ipTTL,
		maxIPs:   maxIPs,
		log:           log,
		db:            db,
		dryRunLogTime: make(map[string]time.Time),
		deviceCache: deviceLimitCache{
			limits: make(map[string]int, 64),
		},
	}

	// Slave mode: set up the reporter that forwards events to master.
	if !cfg.IsMaster() && cfg.AntiFraud.ReportToMaster {
		a.reporter = newSlaveReporter(cfg, log)
	}

	return a
}

// run processes events until ctx is cancelled.
// It also spawns two background goroutines:
//   - deviceCacheRefresh: keeps per-user MaxDevices limits fresh.
//   - slaveReporter.run: batches and forwards events to master (slave-only).
func (a *analyzer) run(ctx context.Context) {
	a.log.Info("antifraud analyzer: starting", "max_ips", a.maxIPs, "ttl", a.ipTTL)
	defer a.log.Info("antifraud analyzer: stopped")

	// Warm up the device cache before processing the first event.
	a.refreshDeviceCache()
	go a.runDeviceCacheRefresh(ctx)

	if a.reporter != nil {
		go a.reporter.run(ctx)
	}

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
	count := a.state.AddEvent(e.email, e.ip, a.ipTTL, now)

	// Dynamic threshold: base limit × number of devices the user is allowed.
	devices := a.getDeviceLimit(e.email)
	threshold := a.maxIPs * devices

	// Forward to master for global aggregation (slave mode only, fire-and-forget).
	if a.reporter != nil {
		a.reporter.add(e)
	}

	if count > threshold {
		reason := fraudReason(e.email, count, threshold, a.maxIPs, devices, a.ipTTL)

		if a.cfg.AntiFraud.DryRun {
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
	var rows []struct {
		Email      string
		MaxDevices int
	}
	if err := a.db.Model(&database.Subscription{}).
		Select("email, max_devices").
		Scan(&rows).Error; err != nil {
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
// On a cache miss (new user), it falls back to a single DB query and caches the result.
// Returns 1 as a safe fallback if the user is not found.
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

	// Cache miss — point query for newly created users not yet in the bulk cache.
	var sub database.Subscription
	val := 1
	if err := a.db.Select("max_devices").Where("email = ?", email).First(&sub).Error; err == nil && sub.MaxDevices > 0 {
		val = sub.MaxDevices
	}

	a.deviceCache.mu.Lock()
	a.deviceCache.limits[email] = val
	a.deviceCache.mu.Unlock()

	return val
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

	banDur, err := time.ParseDuration(a.cfg.AntiFraud.BanDuration)
	if err != nil {
		a.log.Error("antifraud: invalid ban_duration in config, using 10m default",
			slog.String("err", err.Error()))
		banDur = 10 * time.Minute
	}

	now := time.Now()
	expiresAt := now.Add(banDur)

	// 1. Persist the ban to survive server restarts.
	ban := database.AntifraudBan{
		Email:     email,
		BannedAt:  now,
		ExpiresAt: expiresAt,
		Reason:    reason,
	}
	// Use Save (upsert by email unique index) in case a prior ban record exists.
	if result := a.db.Where(database.AntifraudBan{Email: email}).
		Assign(database.AntifraudBan{BannedAt: now, ExpiresAt: expiresAt, Reason: reason}).
		FirstOrCreate(&ban); result.Error != nil {
		a.log.Error("antifraud: failed to persist ban to DB",
			slog.String("email", email),
			slog.String("err", result.Error.Error()),
		)
		return
	}

	// 2. Update the in-memory ban store AFTER the DB write succeeds.
	a.banStore.setBan(email, expiresAt)

	// 3. Remove user from Xray runtime memory (hot-remove).
	// We need the inbound tags from the on-disk config (not touched by antifraud).
	xrayCfg, err := xrayconfig.Read(a.cfg.Paths.XrayConfig)
	if err != nil {
		a.log.Error("antifraud: cannot read xray config for hot-remove",
			slog.String("email", email), slog.String("err", err.Error()))
		// Non-fatal: user will reconnect but subscription page shows dummy.
	} else {
		tags, _ := xrayconfig.InboundTagsForUser(xrayCfg, email)
		if len(tags) > 0 {
			apiClient := xrayapi.NewGRPCClient(a.cfg.Xray.APIAddr)
			if err := apiClient.RemoveUser(email, tags); err != nil {
				a.log.Warn("antifraud: hot-remove failed (non-fatal)",
					slog.String("email", email), slog.String("err", err.Error()))
			}
		}
	}

	// 4. Propagate rmuser to slave nodes (fire-and-forget).
	if a.cfg.IsMaster() {
		go func() {
			client := slave.NewClient(
				a.cfg.SlaveAPI.ConnectTimeout,
				a.cfg.SlaveAPI.RequestTimeout,
				a.cfg.SlaveAPI.RemotePath,
			)
			reg := slave.NewRegistry(a.cfg.Paths.ServersJSON, client)
			reg.PropagateAll("rmuser", map[string]string{"email": email})
		}()
	}

	a.log.Warn("antifraud: ban applied",
		slog.String("email", email),
		slog.String("expires_at", expiresAt.Format(time.RFC3339)),
		slog.String("reason", reason),
	)
}

// ─────────────────────────────────────────────────────────────────────────────
// slaveReporter — forwards IP events to master in batches (slave mode only)
// ─────────────────────────────────────────────────────────────────────────────

// SlaveIPEvent is a single IP observation forwarded from a slave to master.
// Exported so that handlers_internal.go can decode the incoming JSON.
type SlaveIPEvent struct {
	Email string `json:"email"`
	IP    string `json:"ip"`
}

// slaveReporter accumulates events and flushes them to master every 5 seconds.
// It is nil on the master node.
type slaveReporter struct {
	mu   sync.Mutex
	buf  []SlaveIPEvent
	cfg  *appconfig.Config
	log  *slog.Logger
}

func newSlaveReporter(cfg *appconfig.Config, log *slog.Logger) *slaveReporter {
	return &slaveReporter{
		buf: make([]SlaveIPEvent, 0, 64),
		cfg: cfg,
		log: log,
	}
}

// add enqueues an event for the next flush. Thread-safe.
func (r *slaveReporter) add(e event) {
	r.mu.Lock()
	r.buf = append(r.buf, SlaveIPEvent{Email: e.email, IP: e.ip})
	r.mu.Unlock()
}

// run flushes the buffer every 5 seconds until ctx is cancelled.
func (r *slaveReporter) run(ctx context.Context) {
	r.log.Info("antifraud slave reporter: starting")
	defer r.log.Info("antifraud slave reporter: stopped")

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			r.flush() // drain remaining events on shutdown
			return
		case <-ticker.C:
			r.flush()
		}
	}
}

// flush drains the buffer and sends it to master.
// No-op if the buffer is empty.
func (r *slaveReporter) flush() {
	r.mu.Lock()
	if len(r.buf) == 0 {
		r.mu.Unlock()
		return
	}
	batch := r.buf
	r.buf = make([]SlaveIPEvent, 0, 64) // reset for next window
	r.mu.Unlock()

	payload, err := json.Marshal(struct {
		Events []SlaveIPEvent `json:"events"`
	}{Events: batch})
	if err != nil {
		r.log.Error("antifraud slave reporter: failed to marshal batch", slog.String("err", err.Error()))
		return
	}

	if r.cfg.MasterAPI.URL == "" {
		r.log.Warn("antifraud slave reporter: master_api.url is not configured, cannot forward events")
		return
	}

	entry := slave.Entry{
		URL:      r.cfg.MasterAPI.URL,
		APIKey:   r.cfg.MasterAPI.APIKey,
		Insecure: r.cfg.MasterAPI.Insecure,
	}

	client := slave.NewClient(
		r.cfg.SlaveAPI.ConnectTimeout,
		r.cfg.SlaveAPI.RequestTimeout,
		"", // remote path is fully defined by MasterAPI.URL
	)

	_, err = client.Call(entry, "antifraud-events", map[string]string{"payload": string(payload)})
	if err != nil {
		r.log.Warn("antifraud slave reporter: failed to reach master", slog.String("err", err.Error()))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// unbanCleaner
// ─────────────────────────────────────────────────────────────────────────────

// unbanCleaner periodically checks the DB for expired bans and restores users.
//
// Safety invariants (Ghost User protection):
//  1. Delete the ban record from AntifraudBan first (marks intent to unban).
//  2. Verify user still exists in xrayconfig.json (may have been expired by ExpiryWorker).
//  3. Verify subscription.status == "active" in the DB (race-condition guard).
//  4. Only then call AddUser.
//
// If step 3 finds the user inactive, we simply don't re-add — no zombie/ghost users.
type unbanCleaner struct {
	cfg      *appconfig.Config
	banStore *banStore
	db       *gorm.DB
	log      *slog.Logger
	tick     time.Duration
}

const unbanCleanerTick = 15 * time.Second

func newUnbanCleaner(cfg *appconfig.Config, bs *banStore, db *gorm.DB, log *slog.Logger) *unbanCleaner {
	return &unbanCleaner{
		cfg:      cfg,
		banStore: bs,
		db:       db,
		log:      log,
		tick:     unbanCleanerTick,
	}
}

func (uc *unbanCleaner) run(ctx context.Context) {
	uc.log.Info("antifraud unban cleaner: starting")
	defer uc.log.Info("antifraud unban cleaner: stopped")

	ticker := time.NewTicker(uc.tick)
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
	var expired []database.AntifraudBan
	if err := uc.db.Where("expires_at <= ?", time.Now()).Find(&expired).Error; err != nil {
		uc.log.Error("antifraud unban cleaner: DB query failed", slog.String("err", err.Error()))
		return
	}

	for _, ban := range expired {
		uc.tryUnban(ban)
	}
}

func (uc *unbanCleaner) tryUnban(ban database.AntifraudBan) {
	email := ban.Email

	// Step 1: Remove ban record (intent declared first).
	if err := uc.db.Delete(&ban).Error; err != nil {
		uc.log.Error("antifraud unban: failed to delete ban record",
			slog.String("email", email), slog.String("err", err.Error()))
		return
	}

	// Clear in-memory ban entry.
	uc.banStore.clearBan(email)

	// Step 2: Check if user still exists in xrayconfig.json.
	xrayCfg, err := xrayconfig.Read(uc.cfg.Paths.XrayConfig)
	if err != nil {
		uc.log.Error("antifraud unban: cannot read xray config",
			slog.String("email", email), slog.String("err", err.Error()))
		return
	}

	exists, _ := xrayconfig.UserExists(xrayCfg, email)
	if !exists {
		// ExpiryWorker already removed this user from the config — no need to re-add.
		uc.log.Info("antifraud unban: user not in xray config, skipping re-add (likely expired)",
			slog.String("email", email))
		return
	}

	// Step 3: Ghost User protection — verify subscription is still active in the DB.
	var sub database.Subscription
	if err := uc.db.Where("email = ?", email).
		Order("created_at desc").First(&sub).Error; err != nil {
		uc.log.Warn("antifraud unban: subscription not found in DB, skipping",
			slog.String("email", email))
		return
	}

	if sub.Status != "active" {
		uc.log.Info("antifraud unban: subscription no longer active, skipping re-add",
			slog.String("email", email), slog.String("status", sub.Status))
		return
	}

	// Step 4: Re-add user to Xray runtime (hot-add). Re-use the config read in Step 2.
	user, _ := xrayconfig.FindUser(xrayCfg, email)
	if user == nil {
		uc.log.Warn("antifraud unban: user disappeared from config between checks",
			slog.String("email", email))
		return
	}

	payload, err := xrayconfig.BuildForAllInbounds(xrayCfg, xrayconfig.ClientParams{
		Email:   email,
		UUID:    user.GetString("id"),
		Auth:    user.GetString("auth"),
		Subfile: user.GetString("subfile"),
		Expire:  user.GetString("expire"),
	})
	if err != nil {
		uc.log.Error("antifraud unban: failed to build payload",
			slog.String("email", email), slog.String("err", err.Error()))
		return
	}

	apiClient := xrayapi.NewGRPCClient(uc.cfg.Xray.APIAddr)
	if err := apiClient.AddUser(payload, uc.cfg.Paths.XrayConfig); err != nil {
		uc.log.Error("antifraud unban: hot-add failed",
			slog.String("email", email), slog.String("err", err.Error()))
		// Non-fatal: the user's subscription page will still show real nodes
		// (ban is cleared). They'll reconnect on the next Xray config sync or restart.
	} else {
		uc.log.Info("antifraud unban: user restored to Xray runtime",
			slog.String("email", email))
	}

	// Step 5: Propagate to slaves (fire-and-forget).
	if uc.cfg.IsMaster() {
		go func() {
			client := slave.NewClient(
				uc.cfg.SlaveAPI.ConnectTimeout,
				uc.cfg.SlaveAPI.RequestTimeout,
				uc.cfg.SlaveAPI.RemotePath,
			)
			reg := slave.NewRegistry(uc.cfg.Paths.ServersJSON, client)

			limitF := float64(sub.MaxDevices)
			subfile := ""
			if sub.Metadata != nil {
				if sf, ok := sub.Metadata["subfile"].(string); ok {
					subfile = sf
				}
			}
			expireVal := ""
			if sub.EndsAt != nil {
				expireVal = sub.EndsAt.Format("02.01.2006")
			}
			// Take auth from xrayCfg (already read) to properly support Hysteria2 inbounds.
			authVal := ""
			if user != nil {
				authVal = user.GetString("auth")
			}

			slaveParams := map[string]string{
				"email":   sub.Email,
				"uuid":    sub.XrayUUID,
				"subfile": subfile,
				"expire":  expireVal,
				"auth":    authVal,
				"limit":   fmt.Sprintf("%.0f", limitF),
			}
			reg.PropagateAll("newuser", slaveParams)
		}()
	}
}

// buildPayload is a convenience alias used in tests.
func buildPayload(cfg *appconfig.Config, xrayCfg xrayconfig.RawConfig, email string) ([]xrayconfig.TaggedClient, error) {
	user, _ := xrayconfig.FindUser(xrayCfg, email)
	if user == nil {
		return nil, fmt.Errorf("user %q not found in config", email)
	}
	return xrayconfig.BuildForAllInbounds(xrayCfg, xrayconfig.ClientParams{
		Email:   email,
		UUID:    user.GetString("id"),
		Auth:    user.GetString("auth"),
		Subfile: user.GetString("subfile"),
		Expire:  user.GetString("expire"),
	})
}

// fraudReason builds a human-readable ban reason string with dynamic limit details.
func fraudReason(email string, ipCount, threshold, baseLimit, devices int, ttl time.Duration) string {
	return fmt.Sprintf(
		"%d unique IPs in %s window (limit %d = %d base × %d devices) for %q",
		ipCount, ttl, threshold, baseLimit, devices, email,
	)
}
