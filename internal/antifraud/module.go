// Package antifraud — module.go
//
// Module is the public entry point for the Anti-Fraud system.
// It owns all internal goroutines and exposes a minimal interface
// to the rest of the application:
//
//	IsBanned(email) bool      — used by subscription handler and syncstates
//	ForceUnban(email)         — used by admin handlers and ExecUnlimit
//	Run(ctx)                  — starts all workers (blocks until ctx cancelled)
package antifraud

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"gorm.io/gorm"

	"xraytool/internal/appconfig"
	"xraytool/internal/database"
	"xraytool/internal/xrayapi"
	"xraytool/internal/xrayconfig"
)

// banStore is the in-memory cache of active bans.
// It is kept separate from State so that IsBanned() can be called
// from HTTP handlers without locking the IP-tracking mutex.
//
// uniqueIndex on Email in the DB means there's at most one row per user.
type banStore struct {
	mu   sync.RWMutex
	bans map[string]time.Time // email → expiresAt
}

func newBanStore() *banStore {
	return &banStore{bans: make(map[string]time.Time, 16)}
}

func (b *banStore) isBanned(email string) bool {
	b.mu.RLock()
	exp, ok := b.bans[email]
	b.mu.RUnlock()
	return ok && time.Now().Before(exp)
}

func (b *banStore) setBan(email string, expiresAt time.Time) {
	b.mu.Lock()
	b.bans[email] = expiresAt
	b.mu.Unlock()
}

func (b *banStore) clearBan(email string) {
	b.mu.Lock()
	delete(b.bans, email)
	b.mu.Unlock()
}

// Module orchestrates all Anti-Fraud goroutines.
//
// Call New() to create an instance and Run(ctx) to start it.
// All goroutines are gated on the provided context; cancelling it
// performs a clean shutdown.
type Module struct {
	cfg      *appconfig.Config
	db       *gorm.DB
	state    *State
	banStore *banStore
	log      *slog.Logger
}

// New creates a new Module. The module is not started until Run is called.
func New(cfg *appconfig.Config, db *gorm.DB, log *slog.Logger) *Module {
	return &Module{
		cfg:      cfg,
		db:       db,
		state:    newState(),
		banStore: newBanStore(),
		log:      log.With("component", "antifraud"),
	}
}

// IsBanned reports whether the given email is currently under an anti-fraud soft-ban.
// This is safe to call from any goroutine (HTTP handler, syncstates, etc.).
func (m *Module) IsBanned(email string) bool {
	return m.banStore.isBanned(email)
}

// ForceUnban immediately lifts the ban for an email.
// It removes the record from both the in-memory store and the database.
// This is called when an administrator manually unblocks or modifies a user,
// giving admin actions priority over the Anti-Fraud timer.
//
// Dry-run: if email is not banned, this is a safe no-op.
func (m *Module) ForceUnban(email string) {
	m.banStore.clearBan(email)
	// Best-effort DB cleanup; errors are non-fatal.
	m.db.Where("email = ?", email).Delete(&database.AntifraudBan{})
	m.log.Info("antifraud: ban forcefully lifted (admin action)", slog.String("email", email))
}

// Run starts all Anti-Fraud goroutines and blocks until ctx is cancelled.
// It performs startup recovery (re-applies active bans from DB) before
// starting the main workers.
//
// Goroutine layout:
//   - 1× rotator (log file size watchdog)
//   - 1× tailer  (log tail + parser)
//   - 1× analyzer (IP counting + fraud trigger)
//   - 1× ip state cleaner (TTL eviction)
//   - 1× unban cleaner (DB-driven unban)
func (m *Module) Run(ctx context.Context) {
	ipTTL, err := time.ParseDuration(m.cfg.AntiFraud.IPLimitTTL)
	if err != nil {
		m.log.Error("antifraud: invalid ip_limit_ttl, using 3m", slog.String("err", err.Error()))
		ipTTL = 3 * time.Minute
	}

	m.log.Info("antifraud module: starting",
		slog.Bool("enabled", m.cfg.AntiFraud.Enabled),
		slog.String("log_path", m.cfg.AntiFraud.LogPath),
		slog.Int("max_ips", m.cfg.AntiFraud.MaxIPs),
		slog.String("ip_limit_ttl", m.cfg.AntiFraud.IPLimitTTL),
		slog.String("ban_duration", m.cfg.AntiFraud.BanDuration),
	)

	// Startup recovery: re-apply active bans from the database so that a
	// server restart doesn't give fraudsters a free pass.
	m.recoverBansFromDB()

	// Channel connecting tailer → analyzer.
	// Buffer of 512 smooths out burst reads without blocking the tailer.
	eventCh := make(chan event, 512)

	// Channel for Rotator → Tailer "file renamed, re-open" signal.
	rotateCh := make(chan struct{}, 1)

	rot := newRotator(
		m.cfg.AntiFraud.LogPath,
		m.cfg.AntiFraud.LogRotationSizeMB,
		m.cfg.Xray.APIAddr,
		rotateCh,
		m.log,
	)
	tail := newTailer(m.cfg.AntiFraud.LogPath, eventCh, rotateCh, m.log)
	an := newAnalyzer(m.cfg, m.state, m.banStore, eventCh, ipTTL, m.cfg.AntiFraud.MaxIPs, m.db, m.log)
	uc := newUnbanCleaner(m.cfg, m.banStore, m.db, m.log)

	// Launch workers.
	go rot.run(ctx)
	go tail.run(ctx)
	go an.run(ctx)
	go m.runIPCleaner(ctx, ipTTL)
	go uc.run(ctx)

	<-ctx.Done()
	m.log.Info("antifraud module: shutting down")
}

// runIPCleaner runs the State.Clean ticker.
// Separated from Run to keep the goroutine logic testable.
func (m *Module) runIPCleaner(ctx context.Context, ttl time.Duration) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.state.Clean(ttl)
		}
	}
}

// recoverBansFromDB reads unexpired AntifraudBan records from the database and:
//  1. Loads them into the in-memory banStore.
//  2. Ensures each banned user is removed from Xray runtime memory.
//
// This restores the correct state after a server restart or crash.
func (m *Module) recoverBansFromDB() {
	var activeBans []database.AntifraudBan
	if err := m.db.Where("expires_at > ?", time.Now()).Find(&activeBans).Error; err != nil {
		m.log.Error("antifraud recovery: DB query failed", slog.String("err", err.Error()))
		return
	}

	if len(activeBans) == 0 {
		return
	}

	m.log.Info("antifraud recovery: re-applying active bans from DB", slog.Int("count", len(activeBans)))

	xrayCfg, cfgErr := xrayconfig.Read(m.cfg.Paths.XrayConfig)

	for _, ban := range activeBans {
		m.banStore.setBan(ban.Email, ban.ExpiresAt)

		if cfgErr != nil {
			continue
		}

		tags, _ := xrayconfig.InboundTagsForUser(xrayCfg, ban.Email)
		if len(tags) == 0 {
			continue
		}

		apiClient := xrayapi.NewGRPCClient(m.cfg.Xray.APIAddr)
		if err := apiClient.RemoveUser(ban.Email, tags); err != nil {
			// Non-fatal: Xray may have restarted and the user is already gone.
			m.log.Warn("antifraud recovery: hot-remove failed (non-fatal)",
				slog.String("email", ban.Email), slog.String("err", err.Error()))
		} else {
			m.log.Info("antifraud recovery: banned user removed from Xray runtime",
				slog.String("email", ban.Email))
		}
	}
}
