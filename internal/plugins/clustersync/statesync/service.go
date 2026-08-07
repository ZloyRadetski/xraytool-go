package statesync

import (
	"context"
	"fmt"
	json "github.com/goccy/go-json"
	"log/slog"
	"sync"
	"time"

	"xraytool/internal/domain"
	vpn "xraytool/internal/plugins/engine_xray"
)

// maxDeltaEvents is the maximum number of events we bundle into a single
// delta request. Beyond this threshold we fall back to full-sync via
// paginated snapshot endpoint.
const maxDeltaEvents = 500

// snapshotChunkSize is the page size used in paginated full-sync requests.
const snapshotChunkSize = 1000

// eventRetention is how long we keep SyncEvents before purging.
const eventRetention = 7 * 24 * time.Hour

// ─────────────────────────────────────────────────────────────────────────────
// Service
// ─────────────────────────────────────────────────────────────────────────────

// Service orchestrates master→slave state synchronization.
//
// On master nodes it:
//   - Pings each slave with the current event_id + hash.
//   - Sends a delta (list of events) when the slave is slightly behind.
//   - Triggers a paginated full-sync when the slave is too far behind or the
//     hashes are irreconcilable.
//   - Runs the SelfHeal cycle to keep the master's Xray in sync with the DB.
//
// On slave nodes it is nil — slaves only expose HTTP handlers.
type Service struct {
	registry      domain.Registry
	engine        domain.Engine
	slaveProvider domain.StateSyncSlaveProvider
	syncMu        sync.Mutex
	log           *slog.Logger
}

func NewService(
	registry domain.Registry,
	engine domain.Engine,
	slaveProvider domain.StateSyncSlaveProvider,
	log *slog.Logger,
) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		registry:      registry,
		engine:        engine,
		slaveProvider: slaveProvider,
		log:           log.With("component", "statesync"),
	}
}

// SetSlaveProvider allows late-binding of the slave provider after construction.
// This is needed to break the circular dependency:
//
//	statesync.Service → slave.stateSyncProvider → statesync.Service
func (s *Service) SetSlaveProvider(p domain.StateSyncSlaveProvider) {
	s.slaveProvider = p
}

// SyncAllSlaves is the main entry point called by the background worker.
// It delegates to the slaveProvider which knows how to talk HTTP to each slave.
func (s *Service) SyncAllSlaves(ctx context.Context, dryRun bool, forceFull bool) ([]domain.SyncResult, error) {
	if s.slaveProvider == nil {
		return nil, fmt.Errorf("syncstates can only run on master node")
	}
	if !s.syncMu.TryLock() {
		return nil, nil // Another sync already running — skip silently.
	}
	defer s.syncMu.Unlock()

	return s.slaveProvider.SyncAllSlaves(ctx, dryRun, forceFull)
}

// SelfHealMasterUUIDs reconciles the master's Xray runtime against the DB.
// Returns true if any changes were applied.
func (s *Service) SelfHealMasterUUIDs(ctx context.Context) (bool, error) {
	if !s.syncMu.TryLock() {
		return false, nil
	}
	defer s.syncMu.Unlock()

	subs, err := s.registry.Subscriptions().FindAll(ctx)
	if err != nil {
		return false, fmt.Errorf("failed to load subscriptions: %w", err)
	}

	blockedMap := s.getBlockedEmails(ctx)

	dbUsers := make([]domain.VPNUserConfig, 0, len(subs))
	for _, sub := range subs {
		if sub.Email == "" || sub.UUID == "" {
			continue
		}
		if sub.Status != "active" || blockedMap[sub.Email] {
			continue
		}
		dbUsers = append(dbUsers, vpn.SubscriptionToVPNUserConfig(sub))
	}

	result, err := s.engine.SyncUsers(ctx, dbUsers, true)
	if err != nil {
		return false, fmt.Errorf("failed to sync master users: %w", err)
	}

	changed := result.Added > 0 || result.Removed > 0
	return changed, nil
}

// AppendEvent writes a sync event to the master's event log.
// Must be called inside the same transaction that mutates subscriptions/bans.
//
// Returns the newly assigned event ID (can be ignored).
func (s *Service) AppendEvent(ctx context.Context, action domain.SyncAction, user domain.VPNUserConfig) (int64, error) {
	payload, err := json.Marshal(user)
	if err != nil {
		return 0, fmt.Errorf("marshal vpn user: %w", err)
	}
	id, err := s.registry.SyncEvents().Append(ctx, action, string(payload))
	if err != nil {
		return 0, fmt.Errorf("append sync event: %w", err)
	}
	return id, nil
}

// AppendRemoveEvent writes a "remove" event using only the email field.
func (s *Service) AppendRemoveEvent(ctx context.Context, email string) (int64, error) {
	payload, err := json.Marshal(domain.VPNUserConfig{Email: email})
	if err != nil {
		return 0, fmt.Errorf("marshal remove event: %w", err)
	}
	id, err := s.registry.SyncEvents().Append(ctx, domain.SyncActionRemove, string(payload))
	if err != nil {
		return 0, fmt.Errorf("append remove event: %w", err)
	}
	return id, nil
}

// MasterState returns the current master sync state (event_id + hash).
func (s *Service) MasterState(ctx context.Context) (domain.SyncState, error) {
	state, err := s.registry.SyncEvents().GetState(ctx)
	if err != nil {
		return state, err
	}

	// Migration check: if we have users but no sync state, initialize it
	if state.LastEventID == 0 && state.StateHash == "" {
		users, _ := s.BuildSnapshot(ctx)
		if len(users) > 0 {
			state = domain.SyncState{
				LastEventID: 1,
				StateHash:   "migrated_from_legacy",
			}
			if err := s.registry.SyncEvents().SaveState(ctx, state); err == nil {
				s.log.Info("sync state initialized for migration", "users", len(users))
			}
		}
	}

	return state, nil
}

// BuildDelta returns the ordered list of events since afterID.
// Returns (nil, nil) when the events are no longer available (purged) or too many,
// which signals the caller to fall back to full-sync.
func (s *Service) BuildDelta(ctx context.Context, afterID int64) ([]domain.SyncDeltaEvent, error) {
	events, err := s.registry.SyncEvents().FindSince(ctx, afterID)
	if err != nil {
		return nil, err
	}
	if len(events) == 0 || len(events) > maxDeltaEvents {
		return nil, nil // Too many or none: caller should use full-sync.
	}

	delta := make([]domain.SyncDeltaEvent, len(events))
	for i, ev := range events {
		delta[i] = domain.SyncDeltaEvent{
			ID:      ev.ID,
			Action:  ev.Action,
			Payload: ev.Payload,
		}
	}
	return delta, nil
}

// PurgeOldEvents runs the retention policy. Safe to call periodically on master.
func (s *Service) PurgeOldEvents(ctx context.Context) {
	n, err := s.registry.SyncEvents().PurgeOlderThan(ctx, eventRetention)
	if err != nil {
		s.log.Error("statesync: failed to purge old events", "err", err)
		return
	}
	if n > 0 {
		s.log.Info("statesync: purged old sync events", "count", n)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

func (s *Service) getBlockedEmails(ctx context.Context) map[string]bool {
	blockedMap := make(map[string]bool)
	if s.registry == nil {
		return blockedMap
	}

	// Blocked users (admin block).
	users, err := s.registry.Users().FindAll(ctx)
	if err == nil {
		blockedUserIDs := make(map[string]bool)
		for _, u := range users {
			if u.IsBlocked {
				blockedUserIDs[u.ID] = true
			}
		}

		if len(blockedUserIDs) > 0 {
			subs, err := s.registry.Subscriptions().FindAll(ctx)
			if err == nil {
				for _, sub := range subs {
					if blockedUserIDs[sub.UserID] && sub.Email != "" {
						blockedMap[sub.Email] = true
					}
				}
			}
		}
	}

	// Active antifraud bans.
	bans, err := s.registry.AntifraudBans().FindActive(ctx)
	if err == nil {
		for _, b := range bans {
			blockedMap[b.Email] = true
		}
	}

	return blockedMap
}

// BuildSnapshot constructs the full list of active VPNUserConfig entries.
// The returned slice is the single source of truth for a full-sync snapshot.
func (s *Service) BuildSnapshot(ctx context.Context) ([]domain.VPNUserConfig, error) {
	subs, err := s.registry.Subscriptions().FindAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to load subscriptions: %w", err)
	}

	blockedMap := s.getBlockedEmails(ctx)

	users := make([]domain.VPNUserConfig, 0, len(subs))
	for _, sub := range subs {
		if sub.Email == "" || sub.UUID == "" {
			continue
		}
		if sub.Status != "active" || blockedMap[sub.Email] {
			continue
		}
		users = append(users, vpn.SubscriptionToVPNUserConfig(sub))
	}
	return users, nil
}
