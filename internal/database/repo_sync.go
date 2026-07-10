package database

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"xraytool/internal/domain"
)

// ─────────────────────────────────────────────────────────────────────────────
// gormRegistry: SyncEvents accessor
// ─────────────────────────────────────────────────────────────────────────────

func (r *gormRegistry) SyncEvents() domain.SyncEventRepository {
	return &gormSyncEventRepo{db: r.db}
}

// ─────────────────────────────────────────────────────────────────────────────
// gormSyncEventRepo
// ─────────────────────────────────────────────────────────────────────────────

type gormSyncEventRepo struct {
	db *gorm.DB
}

// Append inserts a new SyncEvent and atomically updates SyncState (id=1)
// with a rolling SHA-256 hash: new_hash = sha256(old_hash | event_id | action | payload).
//
// Both operations happen in a single transaction so that SyncState.LastEventID
// is always consistent with the events actually stored.
func (r *gormSyncEventRepo) Append(ctx context.Context, action domain.SyncAction, payload string) (int64, error) {
	var newID int64

	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 1. Insert the event.
		ev := SyncEvent{
			Action:  string(action),
			Payload: payload,
		}
		if err := tx.Create(&ev).Error; err != nil {
			return fmt.Errorf("sync_event insert: %w", err)
		}
		newID = ev.ID

		// 2. Load (or initialise) the current SyncState row.
		var state SyncState
		res := tx.First(&state, 1)
		if res.Error != nil && res.Error != gorm.ErrRecordNotFound {
			return fmt.Errorf("sync_state read: %w", res.Error)
		}

		// 3. Compute rolling hash: sha256(old_hash + event_id + action + payload).
		h := sha256.New()
		h.Write([]byte(state.StateHash))
		h.Write([]byte(fmt.Sprintf("|%d|%s|%s", newID, action, payload)))
		newHash := hex.EncodeToString(h.Sum(nil))

		// 4. Upsert SyncState (OnConflict: update both columns).
		newState := SyncState{
			ID:          1,
			LastEventID: newID,
			StateHash:   newHash,
			UpdatedAt:   time.Now(),
		}
		if err := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "id"}},
			DoUpdates: clause.AssignmentColumns([]string{"last_event_id", "state_hash", "updated_at"}),
		}).Create(&newState).Error; err != nil {
			return fmt.Errorf("sync_state upsert: %w", err)
		}

		return nil
	})
	if err != nil {
		return 0, err
	}
	return newID, nil
}

// GetState returns the current SyncState. If no row exists yet (fresh install),
// it returns a zero-value state (LastEventID=0, StateHash="") without error.
func (r *gormSyncEventRepo) GetState(ctx context.Context) (domain.SyncState, error) {
	var state SyncState
	err := r.db.WithContext(ctx).First(&state, 1).Error
	if err == gorm.ErrRecordNotFound {
		return domain.SyncState{}, nil
	}
	if err != nil {
		return domain.SyncState{}, fmt.Errorf("sync_state read: %w", err)
	}
	return domain.SyncState{
		LastEventID: state.LastEventID,
		StateHash:   state.StateHash,
		UpdatedAt:   state.UpdatedAt,
	}, nil
}

// SaveState upserts the SyncState row (id always = 1).
// Used by slave nodes after applying a delta or a full-sync snapshot.
func (r *gormSyncEventRepo) SaveState(ctx context.Context, s domain.SyncState) error {
	row := SyncState{
		ID:          1,
		LastEventID: s.LastEventID,
		StateHash:   s.StateHash,
		UpdatedAt:   time.Now(),
	}
	return r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "id"}},
		DoUpdates: clause.AssignmentColumns([]string{"last_event_id", "state_hash", "updated_at"}),
	}).Create(&row).Error
}

// FindSince returns all SyncEvents with ID > afterID ordered by ID ascending.
// The caller should pass reasonable limits (master already caps delta size).
func (r *gormSyncEventRepo) FindSince(ctx context.Context, afterID int64) ([]domain.SyncEvent, error) {
	var rows []SyncEvent
	err := r.db.WithContext(ctx).
		Where("id > ?", afterID).
		Order("id asc").
		Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("sync_events find_since: %w", err)
	}

	out := make([]domain.SyncEvent, len(rows))
	for i, row := range rows {
		out[i] = domain.SyncEvent{
			ID:        row.ID,
			Action:    domain.SyncAction(row.Action),
			Payload:   row.Payload,
			CreatedAt: row.CreatedAt,
		}
	}
	return out, nil
}

// PurgeOlderThan deletes SyncEvents older than age (e.g. 7*24*time.Hour).
// Safe to call only on the master node.
func (r *gormSyncEventRepo) PurgeOlderThan(ctx context.Context, age time.Duration) (int64, error) {
	cutoff := time.Now().Add(-age)
	res := r.db.WithContext(ctx).
		Where("created_at < ?", cutoff).
		Delete(&SyncEvent{})
	return res.RowsAffected, res.Error
}
