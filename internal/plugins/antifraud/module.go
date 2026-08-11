// package antifraud_plugin — module.go
//
// Module is the public entry point for the Anti-Fraud system.
// It owns all internal goroutines and exposes a minimal interface
// to the rest of the application:
//
//	IsBanned(email) bool      — used by subscription handler and syncstates
//	ForceUnban(email)         — used by admin handlers and ExecUnlimit
//	Run(ctx)                  — starts all workers (blocks until ctx cancelled)
package antifraud_plugin

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"xraytool/internal/domain"
)

// BanUpdateSink receives anti-fraud decisions for a local read cache. It is
// intentionally structural and local to this package so the business module
// does not depend on the plugin transport contract.
type BanUpdateSink interface {
	PushBanUpdate(email string, bannedUntil time.Time)
	PushUnban(email string)
}

// banStore is the in-memory cache of active bans.
// It is kept separate from State so that IsBanned() can be called
// from HTTP handlers without locking the IP-tracking mutex.
//
// uniqueIndex on Email in the DB means there's at most one row per user.
type banStore struct {
	mu   sync.RWMutex
	sink BanUpdateSink
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
	sink := b.sink
	b.mu.Unlock()
	if sink != nil {
		sink.PushBanUpdate(email, expiresAt)
	}
}

func (b *banStore) clearBan(email string) {
	b.mu.Lock()
	delete(b.bans, email)
	sink := b.sink
	b.mu.Unlock()
	if sink != nil {
		sink.PushUnban(email)
	}
}

// setSink switches the push destination and replays active bans so a cache
// installed after recovery still has a complete local view.
func (b *banStore) setSink(sink BanUpdateSink) {
	b.mu.Lock()
	b.sink = sink
	bans := make(map[string]time.Time, len(b.bans))
	for email, expiresAt := range b.bans {
		bans[email] = expiresAt
	}
	b.mu.Unlock()
	if sink == nil {
		return
	}
	for email, expiresAt := range bans {
		sink.PushBanUpdate(email, expiresAt)
	}
}

type Config struct {
	Enabled               bool
	DryRun                bool
	LogPath               string
	LogRotationSizeMB     int
	LogRotationMaxAge     string
	IPLimitTTL            string
	BanDuration           string
	SuspiciousIPThreshold int
	ReportToMaster        bool

	IsMaster bool
	// APIKey is retained as the internal name for compatibility. It is the
	// cluster-wide HMAC key for IP identities, not an HTTP API key.
	APIKey               string
	HashSecretConfigured bool
}

// Module is the public API for the anti-fraud component.
type Module struct {
	cfg        *Config
	registry   domain.Registry
	banner     domain.SoftBanner
	loggerCtrl domain.LoggerController
	propagator domain.EventPropagator
	reporter   domain.FraudEventReporter
	log        *slog.Logger
	banStore   *banStore
	state      *State

	// Channels
	eventCh chan event
	ready   chan struct{}
	readyMu sync.Once

	// slave tracking
	slavesMu     sync.Mutex
	activeSlaves map[string]time.Time
}

// NewModule creates a new Module. The module is not started until Run is called.
func NewModule(cfg *Config, registry domain.Registry, banner domain.SoftBanner, loggerCtrl domain.LoggerController, propagator domain.EventPropagator, reporter domain.FraudEventReporter, log *slog.Logger) *Module {
	return &Module{
		cfg:          cfg,
		registry:     registry,
		banner:       banner,
		loggerCtrl:   loggerCtrl,
		propagator:   propagator,
		reporter:     reporter,
		log:          log.With("component", "antifraud"),
		banStore:     newBanStore(),
		state:        newState(cfg.APIKey),
		activeSlaves: make(map[string]time.Time),
		eventCh:      make(chan event, 1000),
		ready:        make(chan struct{}),
	}
}

// IsBanned reports whether the given email is currently under an anti-fraud soft-ban.
// This is safe to call from any goroutine (HTTP handler, syncstates, etc.).
func (m *Module) IsBanned(email string) bool {
	return m.banStore.isBanned(email)
}

// SetBanSink configures a consumer of ban/unban decisions. The module's own
// in-memory store remains authoritative; the sink mirrors decisions into the
// kernel-owned cache used by subscription request hot paths.
func (m *Module) SetBanSink(sink BanUpdateSink) {
	m.banStore.setSink(sink)
}

// SnapshotData holds the state of the antifraud module.
type SnapshotData struct {
	State        map[string][]string `json:"state"`
	ActiveSlaves int                 `json:"active_slaves"`
	HashKeyID    string              `json:"hash_key_id"`
}

// GetSnapshot returns the current state of active IP counts and active slave nodes.
// Used by the CLI / API for diagnostics.
func (m *Module) GetSnapshot() SnapshotData {
	m.slavesMu.Lock()
	now := time.Now()
	activeCount := 0
	for id, lastSeen := range m.activeSlaves {
		if now.Sub(lastSeen) < 5*time.Minute {
			activeCount++
		} else {
			delete(m.activeSlaves, id)
		}
	}
	m.slavesMu.Unlock()

	return SnapshotData{
		State:        m.state.Snapshot(),
		ActiveSlaves: activeCount,
		HashKeyID:    m.state.HashKeyID(),
	}
}

// IngestEvents is the master-side integration point for slave IP aggregation.
// cluster_replication calls it only after receiving an authenticated gRPC
// frame; a successful return means the analyzer has processed the batch.
func (m *Module) IngestEvents(ctx context.Context, slaveID string, events []domain.FraudEvent) error {
	select {
	case <-m.ready:
	default:
		return fmt.Errorf("antifraud processor is not ready")
	}

	for _, incoming := range events {
		if incoming.Email == "" || incoming.IP == "" {
			continue
		}
		occurredAt := incoming.OccurredAt.UTC()
		if occurredAt.IsZero() {
			occurredAt = time.Now().UTC()
		}
		completion := make(chan error, 1)
		select {
		case m.eventCh <- event{
			email:      incoming.Email,
			ip:         incoming.IP,
			isHashed:   true,
			occurredAt: occurredAt,
			done:       completion,
		}:
		case <-ctx.Done():
			return ctx.Err()
		default:
			return fmt.Errorf("antifraud event queue is full")
		}
		select {
		case err := <-completion:
			if err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	if slaveID != "" {
		m.slavesMu.Lock()
		m.activeSlaves[slaveID] = time.Now()
		m.slavesMu.Unlock()
	}
	return nil
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
	m.registry.AntifraudBans().DeleteByEmail(context.Background(), email) //nolint:errcheck
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
	ipTTL, err := time.ParseDuration(m.cfg.IPLimitTTL)
	if err != nil {
		m.log.Error("antifraud: invalid ip_limit_ttl, using 3m", slog.String("err", err.Error()))
		ipTTL = 3 * time.Minute
	}

	m.log.Info("antifraud module: starting",
		slog.Bool("enabled", m.cfg.Enabled),
		slog.String("log_path", m.cfg.LogPath),
		slog.Int("max_ips", m.cfg.SuspiciousIPThreshold),
		slog.String("ip_limit_ttl", m.cfg.IPLimitTTL),
		slog.String("ban_duration", m.cfg.BanDuration),
		slog.String("ip_hash_key_id", m.state.HashKeyID()),
	)

	// Startup recovery: re-apply active bans from the database so that a
	// server restart doesn't give fraudsters a free pass.
	m.recoverBansFromDB()

	// Channel connecting tailer → analyzer.
	// Buffer of 512 smooths out burst reads without blocking the tailer.
	// The same channel is reused for slave-originated events (see IngestEvents).
	eventCh := m.eventCh

	// Channel for Rotator → Tailer "file renamed, re-open" signal.
	rotateCh := make(chan struct{}, 1)

	var maxAge time.Duration
	if m.cfg.LogRotationMaxAge != "" {
		if parsed, err := time.ParseDuration(m.cfg.LogRotationMaxAge); err == nil {
			maxAge = parsed
		} else {
			m.log.Warn("antifraud: invalid log_rotation_max_age, using default 5m", "err", err)
			maxAge = 5 * time.Minute
		}
	} else {
		maxAge = 5 * time.Minute
	}

	rot := newRotator(
		m.cfg.LogPath,
		m.cfg.LogRotationSizeMB,
		maxAge,
		m.loggerCtrl,
		rotateCh,
		m.log,
	)
	tail := newTailer(m.cfg.LogPath, eventCh, rotateCh, m.log)
	an := newAnalyzer(
		m.cfg,
		m.state,
		m.banStore,
		m.eventCh,
		ipTTL,
		m.cfg.SuspiciousIPThreshold,
		m.registry,
		m.banner,
		m.propagator,
		m.reporter,
		m.log,
	)
	uc := newUnbanCleaner(m.cfg, m.banStore, m.registry, m.banner, m.propagator, m.log)

	// The analyzer queue is allocated before Run. Mark it ready immediately
	// before launch so replication can retry rather than silently losing an
	// event during startup.
	m.readyMu.Do(func() { close(m.ready) })

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
	activeBans, err := m.registry.AntifraudBans().FindActive(context.Background())
	if err != nil {
		m.log.Error("antifraud recovery: DB query failed", slog.String("err", err.Error()))
		return
	}

	if len(activeBans) == 0 {
		return
	}

	m.log.Info("antifraud recovery: re-applying active bans from DB", slog.Int("count", len(activeBans)))

	for _, ban := range activeBans {
		m.banStore.setBan(ban.Email, ban.ExpiresAt)

		if err := m.banner.BanUser(context.Background(), ban.Email); err != nil {
			m.log.Warn("antifraud recovery: hot-remove failed (non-fatal)",
				slog.String("email", ban.Email), slog.String("err", err.Error()))
		} else {
			m.log.Info("antifraud recovery: banned user removed from runtime",
				slog.String("email", ban.Email))
		}
	}
}
