package clusterreplication

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"xraytool/internal/domain"
	"xraytool/internal/pluginapi"
)

const (
	eventUserUpsert = "user.upsert"
	eventUserRemove = "user.remove"
	eventResnapshot = "snapshot.required"
	eventArtifact   = "artifact.upsert"
)

type outboxRow struct {
	Revision  int64     `gorm:"column:revision;primaryKey"`
	Kind      string    `gorm:"column:kind"`
	Payload   string    `gorm:"column:payload"`
	Checksum  string    `gorm:"column:checksum"`
	CreatedAt time.Time `gorm:"column:created_at"`
}

func (outboxRow) TableName() string { return "cluster_replication_outbox" }

type desiredUserRow struct {
	Email     string    `gorm:"column:email;primaryKey"`
	Payload   string    `gorm:"column:payload"`
	Revision  int64     `gorm:"column:revision"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

func (desiredUserRow) TableName() string { return "cluster_replication_desired_users" }

type inboxRow struct {
	MasterID  string    `gorm:"column:master_id;primaryKey"`
	Revision  int64     `gorm:"column:revision;primaryKey"`
	Checksum  string    `gorm:"column:checksum"`
	AppliedAt time.Time `gorm:"column:applied_at"`
}

func (inboxRow) TableName() string { return "cluster_replication_inbox" }

type metaRow struct {
	Key       string    `gorm:"column:key;primaryKey"`
	Value     string    `gorm:"column:value"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

func (metaRow) TableName() string { return "cluster_replication_meta" }

type artifactRow struct {
	Kind      string    `gorm:"column:kind;primaryKey"`
	Payload   string    `gorm:"column:payload"`
	Checksum  string    `gorm:"column:checksum"`
	Revision  int64     `gorm:"column:revision"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

func (artifactRow) TableName() string { return "cluster_replication_artifacts" }

type stagingUserRow struct {
	SnapshotID string `gorm:"column:snapshot_id;primaryKey"`
	Email      string `gorm:"column:email;primaryKey"`
	Payload    string `gorm:"column:payload"`
}

func (stagingUserRow) TableName() string { return "cluster_replication_staging_users" }

type replicaRow struct {
	NodeID               string    `gorm:"column:node_id;primaryKey"`
	AcknowledgedRevision int64     `gorm:"column:acknowledged_revision"`
	ConnectedAt          time.Time `gorm:"column:connected_at"`
	LastSeenAt           time.Time `gorm:"column:last_seen_at"`
}

func (replicaRow) TableName() string { return "cluster_replication_replicas" }

type replicaStatsRow struct {
	NodeID    string    `gorm:"column:node_id;primaryKey"`
	Email     string    `gorm:"column:email;primaryKey"`
	Total     int64     `gorm:"column:total"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

func (replicaStatsRow) TableName() string { return "cluster_replication_stats" }

type fraudOutboxRow struct {
	EventID    string    `gorm:"column:event_id;primaryKey"`
	Email      string    `gorm:"column:email"`
	IPHash     string    `gorm:"column:ip_hash"`
	HashKeyID  string    `gorm:"column:hash_key_id"`
	OccurredAt time.Time `gorm:"column:occurred_at"`
	CreatedAt  time.Time `gorm:"column:created_at"`
}

func (fraudOutboxRow) TableName() string { return "cluster_replication_fraud_outbox" }

type fraudInboxRow struct {
	NodeID     string    `gorm:"column:node_id;primaryKey"`
	EventID    string    `gorm:"column:event_id;primaryKey"`
	Email      string    `gorm:"column:email"`
	IPHash     string    `gorm:"column:ip_hash"`
	HashKeyID  string    `gorm:"column:hash_key_id"`
	OccurredAt time.Time `gorm:"column:occurred_at"`
	ReceivedAt time.Time `gorm:"column:received_at"`
}

func (fraudInboxRow) TableName() string { return "cluster_replication_fraud_inbox" }

type Event struct {
	Revision int64
	Kind     string
	Payload  []byte
	Checksum string
}

// FraudEvent is an opaque, already-hashed observation persisted by the
// replication plugin. IP is never a raw address at this boundary.
type FraudEvent struct {
	ID         string    `json:"id"`
	Email      string    `json:"email"`
	IP         string    `json:"ip"`
	OccurredAt time.Time `json:"occurred_at"`
	HashKeyID  string    `json:"hash_key_id,omitempty"`
}

type Store struct{ db *gorm.DB }

func NewStore(db *gorm.DB) *Store { return &Store{db: db} }

func (s *Store) Append(ctx context.Context, kind string, payload []byte) (Event, error) {
	checksum := checksum(kind, payload)
	row := outboxRow{
		Kind:     kind,
		Payload:  base64.StdEncoding.EncodeToString(payload),
		Checksum: checksum,
	}
	if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
		return Event{}, fmt.Errorf("append replication outbox event: %w", err)
	}
	return Event{Revision: row.Revision, Kind: row.Kind, Payload: append([]byte(nil), payload...), Checksum: checksum}, nil
}

func (s *Store) EventsAfter(ctx context.Context, revision int64, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = 500
	}
	var rows []outboxRow
	if err := s.db.WithContext(ctx).Where("revision > ?", revision).Order("revision ASC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("read replication outbox: %w", err)
	}
	events := make([]Event, 0, len(rows))
	for _, row := range rows {
		payload, err := base64.StdEncoding.DecodeString(row.Payload)
		if err != nil {
			return nil, fmt.Errorf("decode replication event %d: %w", row.Revision, err)
		}
		events = append(events, Event{Revision: row.Revision, Kind: row.Kind, Payload: payload, Checksum: row.Checksum})
	}
	return events, nil
}

func (s *Store) LatestRevision(ctx context.Context) (int64, error) {
	var row outboxRow
	err := s.db.WithContext(ctx).Order("revision DESC").Limit(1).Find(&row).Error
	if err != nil {
		return 0, fmt.Errorf("read latest replication revision: %w", err)
	}
	return row.Revision, nil
}

func (s *Store) OldestRevision(ctx context.Context) (int64, error) {
	var row outboxRow
	err := s.db.WithContext(ctx).Order("revision ASC").Limit(1).Find(&row).Error
	if err != nil {
		return 0, fmt.Errorf("read oldest replication revision: %w", err)
	}
	return row.Revision, nil
}

func (s *Store) GetMeta(ctx context.Context, key string) (string, bool, error) {
	var row metaRow
	err := s.db.WithContext(ctx).Where("key = ?", key).First(&row).Error
	if err == nil {
		return row.Value, true, nil
	}
	if err == gorm.ErrRecordNotFound {
		return "", false, nil
	}
	return "", false, fmt.Errorf("read replication metadata: %w", err)
}

func (s *Store) PutMeta(ctx context.Context, key, value string) error {
	row := metaRow{Key: key, Value: value}
	return s.db.WithContext(ctx).Save(&row).Error
}

func (s *Store) DeleteMeta(ctx context.Context, key string) error {
	return s.db.WithContext(ctx).Where("key = ?", key).Delete(&metaRow{}).Error
}

func (s *Store) AlreadyApplied(ctx context.Context, masterID string, revision int64) (bool, error) {
	var count int64
	if err := s.db.WithContext(ctx).Model(&inboxRow{}).Where("master_id = ? AND revision = ?", masterID, revision).Count(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}

func (s *Store) MarkApplied(ctx context.Context, masterID string, event Event) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&inboxRow{MasterID: masterID, Revision: event.Revision, Checksum: event.Checksum}).Error; err != nil {
			if isUniqueConstraint(err) {
				return nil
			}
			return err
		}
		return tx.Save(&metaRow{Key: "applied_revision", Value: fmt.Sprintf("%d", event.Revision)}).Error
	})
}

func (s *Store) MarkReplicaConnected(ctx context.Context, nodeID string) error {
	var row replicaRow
	err := s.db.WithContext(ctx).Where("node_id = ?", nodeID).First(&row).Error
	if err == gorm.ErrRecordNotFound {
		now := time.Now().UTC()
		return s.db.WithContext(ctx).Create(&replicaRow{NodeID: nodeID, ConnectedAt: now, LastSeenAt: now}).Error
	}
	if err != nil {
		return err
	}
	row.LastSeenAt = time.Now().UTC()
	return s.db.WithContext(ctx).Save(&row).Error
}

func (s *Store) AcknowledgeReplica(ctx context.Context, nodeID string, revision int64) error {
	var row replicaRow
	err := s.db.WithContext(ctx).Where("node_id = ?", nodeID).First(&row).Error
	if err == gorm.ErrRecordNotFound {
		now := time.Now().UTC()
		return s.db.WithContext(ctx).Create(&replicaRow{NodeID: nodeID, AcknowledgedRevision: revision, ConnectedAt: now, LastSeenAt: now}).Error
	}
	if err != nil {
		return err
	}
	if revision > row.AcknowledgedRevision {
		row.AcknowledgedRevision = revision
	}
	row.LastSeenAt = time.Now().UTC()
	return s.db.WithContext(ctx).Save(&row).Error
}

// EnqueueFraudEvents persists slave observations before they are sent on the
// gRPC stream. A successful return means a temporary transport failure cannot
// lose the event.
func (s *Store) EnqueueFraudEvents(ctx context.Context, events []pluginapi.FraudEvent) ([]FraudEvent, error) {
	queued := make([]FraudEvent, 0, len(events))
	if len(events) == 0 {
		return queued, nil
	}
	now := time.Now().UTC()
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for index, event := range events {
			if event.Email == "" || event.IP == "" {
				return fmt.Errorf("invalid anti-fraud event")
			}
			occurredAt := event.OccurredAt.UTC()
			if occurredAt.IsZero() {
				occurredAt = now
			}
			queuedEvent := FraudEvent{
				ID:         uuid.New().String(),
				Email:      event.Email,
				IP:         event.IP,
				OccurredAt: occurredAt,
				HashKeyID:  event.HashKeyID,
			}
			// The plugin serializes enqueue calls. Give rows in one batch distinct
			// timestamps so the portable created_at ordering remains deterministic
			// on SQLite and PostgreSQL without a database-specific sequence column.
			createdAt := now.Add(time.Duration(index) * time.Microsecond)
			if err := tx.Create(&fraudOutboxRow{
				EventID:    queuedEvent.ID,
				Email:      queuedEvent.Email,
				IPHash:     queuedEvent.IP,
				HashKeyID:  queuedEvent.HashKeyID,
				OccurredAt: queuedEvent.OccurredAt,
				CreatedAt:  createdAt,
			}).Error; err != nil {
				return fmt.Errorf("enqueue replication anti-fraud event: %w", err)
			}
			queued = append(queued, queuedEvent)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return queued, nil
}

// PendingFraudEvents returns the oldest unacknowledged local observations.
func (s *Store) PendingFraudEvents(ctx context.Context, limit int) ([]FraudEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	var rows []fraudOutboxRow
	if err := s.db.WithContext(ctx).Order("created_at ASC, event_id ASC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("read replication anti-fraud outbox: %w", err)
	}
	result := make([]FraudEvent, 0, len(rows))
	for _, row := range rows {
		result = append(result, FraudEvent{
			ID:         row.EventID,
			Email:      row.Email,
			IP:         row.IPHash,
			OccurredAt: row.OccurredAt.UTC(),
			HashKeyID:  row.HashKeyID,
		})
	}
	return result, nil
}

// AcknowledgeFraudEvents removes only the rows explicitly accepted by the
// master. Unknown IDs are harmless, which makes ACK replay idempotent.
func (s *Store) AcknowledgeFraudEvents(ctx context.Context, eventIDs []string) error {
	ids := nonEmptyStrings(eventIDs)
	if len(ids) == 0 {
		return nil
	}
	if err := s.db.WithContext(ctx).Where("event_id IN ?", ids).Delete(&fraudOutboxRow{}).Error; err != nil {
		return fmt.Errorf("acknowledge replication anti-fraud events: %w", err)
	}
	return nil
}

// UnreceivedFraudEvents filters a retried batch against master-side durable
// receipts. The returned events are safe to submit to the anti-fraud provider.
func (s *Store) UnreceivedFraudEvents(ctx context.Context, nodeID string, events []FraudEvent) ([]FraudEvent, error) {
	if nodeID == "" {
		return nil, fmt.Errorf("replication anti-fraud source node is required")
	}
	ids := make([]string, 0, len(events))
	for _, event := range events {
		if event.ID == "" || event.Email == "" || event.IP == "" {
			return nil, fmt.Errorf("invalid replicated anti-fraud event")
		}
		ids = append(ids, event.ID)
	}
	if len(ids) == 0 {
		return nil, nil
	}
	var rows []fraudInboxRow
	if err := s.db.WithContext(ctx).Where("node_id = ? AND event_id IN ?", nodeID, ids).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("read replication anti-fraud receipts: %w", err)
	}
	received := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		received[row.EventID] = struct{}{}
	}
	result := make([]FraudEvent, 0, len(events))
	for _, event := range events {
		if _, exists := received[event.ID]; !exists {
			result = append(result, event)
		}
	}
	return result, nil
}

// MarkFraudReceived writes idempotency receipts only after the anti-fraud
// provider has accepted the events. A failed provider call therefore leaves
// the slave queue intact for retry.
func (s *Store) MarkFraudReceived(ctx context.Context, nodeID string, events []FraudEvent) error {
	if nodeID == "" {
		return fmt.Errorf("replication anti-fraud source node is required")
	}
	now := time.Now().UTC()
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, event := range events {
			if event.ID == "" || event.Email == "" || event.IP == "" {
				return fmt.Errorf("invalid replicated anti-fraud event")
			}
			occurredAt := event.OccurredAt.UTC()
			if occurredAt.IsZero() {
				occurredAt = now
			}
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&fraudInboxRow{
				NodeID:     nodeID,
				EventID:    event.ID,
				Email:      event.Email,
				IPHash:     event.IP,
				HashKeyID:  event.HashKeyID,
				OccurredAt: occurredAt,
				ReceivedAt: now,
			}).Error; err != nil {
				return fmt.Errorf("record replication anti-fraud receipt: %w", err)
			}
		}
		return nil
	})
}

// PruneAcknowledged removes journal rows only when every currently permitted
// slave has acknowledged them. A newly added node receives a snapshot, while a
// removed or offline known node intentionally keeps retention from advancing.
func (s *Store) PruneAcknowledged(ctx context.Context, allowedNodes []string) (int64, error) {
	if len(allowedNodes) == 0 {
		return 0, nil
	}
	minimum := int64(-1)
	for _, nodeID := range allowedNodes {
		var row replicaRow
		err := s.db.WithContext(ctx).Where("node_id = ?", nodeID).First(&row).Error
		if err == gorm.ErrRecordNotFound {
			return 0, nil
		}
		if err != nil {
			return 0, err
		}
		if minimum < 0 || row.AcknowledgedRevision < minimum {
			minimum = row.AcknowledgedRevision
		}
	}
	if minimum <= 0 {
		return 0, nil
	}
	result := s.db.WithContext(ctx).Where("revision <= ?", minimum).Delete(&outboxRow{})
	return result.RowsAffected, result.Error
}

func (s *Store) ReplaceReplicaStats(ctx context.Context, nodeID string, points []statsPoint) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("node_id = ?", nodeID).Delete(&replicaStatsRow{}).Error; err != nil {
			return err
		}
		for _, point := range points {
			if point.Email == "" || point.Total < 0 {
				return fmt.Errorf("invalid replicated statistics point")
			}
			if err := tx.Create(&replicaStatsRow{NodeID: nodeID, Email: point.Email, Total: point.Total}).Error; err != nil {
				return err
			}
		}
		var replica replicaRow
		err := tx.Where("node_id = ?", nodeID).First(&replica).Error
		now := time.Now().UTC()
		if err == gorm.ErrRecordNotFound {
			return tx.Create(&replicaRow{NodeID: nodeID, ConnectedAt: now, LastSeenAt: now}).Error
		}
		if err != nil {
			return err
		}
		replica.LastSeenAt = now
		return tx.Save(&replica).Error
	})
}

func (s *Store) CollectReplicaStats(ctx context.Context, allowedNodes []string, maxAge time.Duration) ([]domain.SlaveUserTotal, domain.SlaveReport) {
	report := domain.SlaveReport{Enabled: len(allowedNodes) > 0, TotalServers: len(allowedNodes)}
	if len(allowedNodes) == 0 {
		return nil, report
	}
	combined := make(map[string]int64)
	now := time.Now().UTC()
	for _, nodeID := range allowedNodes {
		var replica replicaRow
		replicaErr := s.db.WithContext(ctx).Where("node_id = ?", nodeID).First(&replica).Error
		if replicaErr != nil && replicaErr != gorm.ErrRecordNotFound {
			report.FailedServers++
			continue
		}
		var rows []replicaStatsRow
		if err := s.db.WithContext(ctx).Where("node_id = ?", nodeID).Find(&rows).Error; err != nil {
			report.FailedServers++
			continue
		}

		// Freshness is operational health, not an accounting deletion signal.
		// A slave reports cumulative counters, so discarding its last successful
		// snapshot just because a connection is temporarily down makes a user's
		// subscription traffic appear to move backwards. Retain those last-known
		// totals while flagging the node as failed/stale in the report.
		if replicaErr == nil && (maxAge <= 0 || now.Sub(replica.LastSeenAt) <= maxAge) {
			report.OKServers++
		} else {
			report.FailedServers++
		}
		for _, row := range rows {
			combined[row.Email] += row.Total
		}
	}
	result := make([]domain.SlaveUserTotal, 0, len(combined))
	for email, total := range combined {
		result = append(result, domain.SlaveUserTotal{Email: email, Slave: total})
	}
	return result, report
}

func (s *Store) UpsertDesiredUser(ctx context.Context, email string, payload []byte, revision int64) error {
	return s.db.WithContext(ctx).Save(&desiredUserRow{
		Email:    email,
		Payload:  base64.StdEncoding.EncodeToString(payload),
		Revision: revision,
	}).Error
}

func (s *Store) RemoveDesiredUser(ctx context.Context, email string) error {
	return s.db.WithContext(ctx).Where("email = ?", email).Delete(&desiredUserRow{}).Error
}

func (s *Store) DesiredUsers(ctx context.Context) ([][]byte, error) {
	var rows []desiredUserRow
	if err := s.db.WithContext(ctx).Order("email ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	result := make([][]byte, 0, len(rows))
	for _, row := range rows {
		payload, err := base64.StdEncoding.DecodeString(row.Payload)
		if err != nil {
			return nil, err
		}
		result = append(result, payload)
	}
	return result, nil
}

func (s *Store) StageUser(ctx context.Context, snapshotID, email string, payload []byte) error {
	return s.db.WithContext(ctx).Save(&stagingUserRow{SnapshotID: snapshotID, Email: email, Payload: base64.StdEncoding.EncodeToString(payload)}).Error
}

func (s *Store) ActivateSnapshot(ctx context.Context, snapshotID string, revision int64) ([][]byte, error) {
	var payloads [][]byte
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var staged []stagingUserRow
		if err := tx.Where("snapshot_id = ?", snapshotID).Order("email ASC").Find(&staged).Error; err != nil {
			return err
		}
		payloads = make([][]byte, 0, len(staged))
		if err := tx.Session(&gorm.Session{AllowGlobalUpdate: true}).Delete(&desiredUserRow{}).Error; err != nil {
			return err
		}
		for _, row := range staged {
			payload, err := base64.StdEncoding.DecodeString(row.Payload)
			if err != nil {
				return err
			}
			payloads = append(payloads, payload)
			if err := tx.Create(&desiredUserRow{Email: row.Email, Payload: row.Payload, Revision: revision}).Error; err != nil {
				return err
			}
		}
		if err := tx.Where("snapshot_id = ?", snapshotID).Delete(&stagingUserRow{}).Error; err != nil {
			return err
		}
		if err := tx.Save(&metaRow{Key: "applied_revision", Value: fmt.Sprintf("%d", revision)}).Error; err != nil {
			return err
		}
		return tx.Save(&metaRow{Key: "snapshot_initialized", Value: "true"}).Error
	})
	return payloads, err
}

func (s *Store) PutArtifact(ctx context.Context, kind string, payload []byte, revision int64) error {
	return s.db.WithContext(ctx).Save(&artifactRow{
		Kind:     kind,
		Payload:  base64.StdEncoding.EncodeToString(payload),
		Checksum: checksum(kind, payload),
		Revision: revision,
	}).Error
}

func (s *Store) DeleteArtifact(ctx context.Context, kind string) error {
	return s.db.WithContext(ctx).Where("kind = ?", kind).Delete(&artifactRow{}).Error
}

func (s *Store) Artifacts(ctx context.Context) ([]Event, error) {
	var rows []artifactRow
	if err := s.db.WithContext(ctx).Order("kind ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	artifacts := make([]Event, 0, len(rows))
	for _, row := range rows {
		payload, err := base64.StdEncoding.DecodeString(row.Payload)
		if err != nil {
			return nil, err
		}
		artifacts = append(artifacts, Event{Revision: row.Revision, Kind: row.Kind, Payload: payload, Checksum: row.Checksum})
	}
	return artifacts, nil
}

func checksum(kind string, payload []byte) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte(kind))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write(payload)
	return hex.EncodeToString(hash.Sum(nil))
}

func isUniqueConstraint(err error) bool {
	// SQLite and PostgreSQL use different concrete error types. The duplicate
	// marker is stable enough here because it is only used for idempotent inbox
	// delivery; any other error is still returned to the caller.
	message := err.Error()
	return contains(message, "UNIQUE constraint failed") || contains(message, "duplicate key")
}

func contains(value, needle string) bool {
	for index := 0; index+len(needle) <= len(value); index++ {
		if value[index:index+len(needle)] == needle {
			return true
		}
	}
	return false
}

func nonEmptyStrings(values []string) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}
