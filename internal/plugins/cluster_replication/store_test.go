package clusterreplication

import (
	"context"
	"testing"
	"time"

	json "github.com/goccy/go-json"
	"github.com/stretchr/testify/require"

	"xraytool/internal/database"
	"xraytool/internal/domain"
	"xraytool/internal/pluginapi"
	vpn "xraytool/internal/plugins/engine_xray"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	db, err := database.NewConnection(database.Config{
		Driver:     "sqlite",
		SQLitePath: "file:cluster-replication-" + t.Name() + "?mode=memory&cache=shared",
		Silent:     true,
	})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	runner, ok := database.NewPluginDBFactory(db)("cluster_replication").(pluginapi.EmbeddedMigrationRunner)
	require.True(t, ok)
	require.NoError(t, runner.RunEmbeddedMigrations(context.Background(), (&Plugin{}).PluginMigrations()))
	return NewStore(db)
}

func TestStoreAppendsOrderedDurableEvents(t *testing.T) {
	store := newTestStore(t)
	first, err := store.Append(context.Background(), eventUserUpsert, []byte(`{"user":{"email":"one@example.test"}}`))
	require.NoError(t, err)
	second, err := store.Append(context.Background(), eventUserRemove, []byte(`{"email":"one@example.test"}`))
	require.NoError(t, err)
	require.Greater(t, second.Revision, first.Revision)

	events, err := store.EventsAfter(context.Background(), 0, 100)
	require.NoError(t, err)
	require.Equal(t, []int64{first.Revision, second.Revision}, []int64{events[0].Revision, events[1].Revision})
	require.Equal(t, first.Checksum, events[0].Checksum)
}

func TestStoreActivatesSnapshotAtomically(t *testing.T) {
	store := newTestStore(t)
	payload, err := json.Marshal(userPayload{User: domain.VPNUserConfig{Email: "one@example.test", UUID: "uuid-1"}})
	require.NoError(t, err)
	require.NoError(t, store.StageUser(context.Background(), "snapshot-1", "one@example.test", payload))

	users, err := store.ActivateSnapshot(context.Background(), "snapshot-1", 42)
	require.NoError(t, err)
	require.Len(t, users, 1)
	stored, err := store.DesiredUsers(context.Background())
	require.NoError(t, err)
	require.Equal(t, users, stored)
	require.Equal(t, int64(42), currentRevision(context.Background(), store))
	require.True(t, snapshotInitialized(context.Background(), store))
}

type reconcileEngine struct {
	vpn.NoopEngine
	users []domain.VPNUserConfig
	added int
}

func (e *reconcileEngine) AddUser(_ context.Context, user domain.VPNUserConfig) error {
	e.added++
	e.users = append(e.users, user)
	return nil
}

func (e *reconcileEngine) ReconcileUsers(_ context.Context, users []domain.VPNUserConfig) (*domain.EngineSyncResult, error) {
	e.users = append([]domain.VPNUserConfig(nil), users...)
	return &domain.EngineSyncResult{}, nil
}

func TestReconcileUsersUsesForcedDriftRepair(t *testing.T) {
	engine := &reconcileEngine{}
	service := &Service{engine: engine}
	users := []domain.VPNUserConfig{{Email: "snapshot@example.test", UUID: "uuid"}}

	require.NoError(t, service.reconcileUsers(context.Background(), users))
	require.Equal(t, users, engine.users)
}

func TestReconcileSlaveUsesPersistedDesiredProjection(t *testing.T) {
	store := newTestStore(t)
	payload, err := json.Marshal(userPayload{User: domain.VPNUserConfig{Email: "repair@example.test", UUID: "uuid-1"}})
	require.NoError(t, err)
	require.NoError(t, store.UpsertDesiredUser(context.Background(), "repair@example.test", payload, 1))
	engine := &reconcileEngine{}
	service := NewService(nil, engine, store, nil)
	require.NoError(t, service.ReconcileSlave(context.Background()))
	require.Equal(t, []domain.VPNUserConfig{{Email: "repair@example.test", UUID: "uuid-1"}}, engine.users)
}

func TestApplyEventIsIdempotentThroughInbox(t *testing.T) {
	store := newTestStore(t)
	engine := &reconcileEngine{}
	service := NewService(nil, engine, store, nil)
	payload, err := json.Marshal(userPayload{User: domain.VPNUserConfig{Email: "once@example.test", UUID: "uuid-1"}})
	require.NoError(t, err)
	event := Event{Revision: 9, Kind: eventUserUpsert, Payload: payload, Checksum: checksum(eventUserUpsert, payload)}
	require.NoError(t, service.ApplyEvent(context.Background(), "master-a", event, ""))
	require.NoError(t, service.ApplyEvent(context.Background(), "master-a", event, ""))
	require.Equal(t, 1, engine.added)
}

func TestOutboxRetentionWaitsForEveryAllowedReplicaAcknowledgement(t *testing.T) {
	store := newTestStore(t)
	event, err := store.Append(context.Background(), eventUserRemove, []byte(`{"email":"retention@example.test"}`))
	require.NoError(t, err)

	require.NoError(t, store.AcknowledgeReplica(context.Background(), "slave-a", event.Revision))
	removed, err := store.PruneAcknowledged(context.Background(), []string{"slave-a", "slave-b"})
	require.NoError(t, err)
	require.Zero(t, removed)

	require.NoError(t, store.AcknowledgeReplica(context.Background(), "slave-b", event.Revision))
	removed, err = store.PruneAcknowledged(context.Background(), []string{"slave-a", "slave-b"})
	require.NoError(t, err)
	require.EqualValues(t, 1, removed)
	events, err := store.EventsAfter(context.Background(), 0, 10)
	require.NoError(t, err)
	require.Empty(t, events)
}

func TestStoreAggregatesReportedSlaveStatistics(t *testing.T) {
	store := newTestStore(t)
	require.NoError(t, store.ReplaceReplicaStats(context.Background(), "slave-a", []statsPoint{
		{Email: "traffic@example.test", Total: 100},
	}))
	require.NoError(t, store.ReplaceReplicaStats(context.Background(), "slave-b", []statsPoint{
		{Email: "traffic@example.test", Total: 50},
	}))
	totals, report := store.CollectReplicaStats(context.Background(), []string{"slave-a", "slave-b"}, time.Minute)
	require.Equal(t, domain.SlaveReport{Enabled: true, TotalServers: 2, OKServers: 2}, report)
	require.Equal(t, []domain.SlaveUserTotal{{Email: "traffic@example.test", Slave: 150}}, totals)
}

func TestStorePersistsFraudEventsUntilExplicitAcknowledgement(t *testing.T) {
	store := newTestStore(t)
	occurredAt := time.Now().UTC().Add(-time.Second)
	queued, err := store.EnqueueFraudEvents(context.Background(), []pluginapi.FraudEvent{
		{Email: "user@example.test", IP: "hash-one", OccurredAt: occurredAt},
		{Email: "user@example.test", IP: "hash-two", OccurredAt: occurredAt},
	})
	require.NoError(t, err)
	require.Len(t, queued, 2)
	require.NotEmpty(t, queued[0].ID)
	require.NotEqual(t, queued[0].ID, queued[1].ID)

	pending, err := store.PendingFraudEvents(context.Background(), 10)
	require.NoError(t, err)
	require.Equal(t, queued, pending)

	require.NoError(t, store.AcknowledgeFraudEvents(context.Background(), []string{queued[0].ID, queued[0].ID, ""}))
	pending, err = store.PendingFraudEvents(context.Background(), 10)
	require.NoError(t, err)
	require.Equal(t, []FraudEvent{queued[1]}, pending)
}

func TestStoreFraudReceiptsAreNodeScopedAndIdempotent(t *testing.T) {
	store := newTestStore(t)
	events := []FraudEvent{{
		ID:         "event-1",
		Email:      "user@example.test",
		IP:         "stable-ip-hash",
		OccurredAt: time.Now().UTC(),
	}}

	pending, err := store.UnreceivedFraudEvents(context.Background(), "slave-a", events)
	require.NoError(t, err)
	require.Equal(t, events, pending)
	require.NoError(t, store.MarkFraudReceived(context.Background(), "slave-a", pending))
	require.NoError(t, store.MarkFraudReceived(context.Background(), "slave-a", pending))

	pending, err = store.UnreceivedFraudEvents(context.Background(), "slave-a", events)
	require.NoError(t, err)
	require.Empty(t, pending)
	pending, err = store.UnreceivedFraudEvents(context.Background(), "slave-b", events)
	require.NoError(t, err)
	require.Equal(t, events, pending)
}

func TestPluginFraudEventsPersistWhileSlaveIsDisconnected(t *testing.T) {
	store := newTestStore(t)
	plugin := &Plugin{
		config:  Config{Mode: "slave"},
		service: &Service{store: store},
	}

	require.NoError(t, plugin.ReportFraudEvents(context.Background(), []pluginapi.FraudEvent{{
		Email:      "user@example.test",
		IP:         "stable-ip-hash",
		OccurredAt: time.Now().UTC(),
		HashKeyID:  "key-id",
	}}))

	pending, err := store.PendingFraudEvents(context.Background(), 10)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, "stable-ip-hash", pending[0].IP)
	require.Equal(t, "key-id", pending[0].HashKeyID)
}
