package clusterreplication

import (
	"context"
	"sync"
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

type snapshotSubscriptionRepository struct {
	domain.SubscriptionRepository
	items []domain.Subscription
}

func (r snapshotSubscriptionRepository) FindAll(context.Context) ([]domain.Subscription, error) {
	return append([]domain.Subscription(nil), r.items...), nil
}

type snapshotUserRepository struct{ domain.UserRepository }

func (snapshotUserRepository) FindAll(context.Context) ([]domain.User, error) { return nil, nil }

type snapshotBanRepository struct{ domain.AntifraudBanRepository }

func (snapshotBanRepository) FindActive(context.Context) ([]domain.AntifraudBan, error) {
	return nil, nil
}

type snapshotRegistry struct {
	domain.Registry
	subscriptions domain.SubscriptionRepository
}

func (r snapshotRegistry) Subscriptions() domain.SubscriptionRepository { return r.subscriptions }
func (snapshotRegistry) Users() domain.UserRepository                   { return snapshotUserRepository{} }
func (snapshotRegistry) AntifraudBans() domain.AntifraudBanRepository {
	return snapshotBanRepository{}
}

type templateSnapshotEngine struct {
	vpn.NoopEngine
	users   []domain.VPNUserConfig
	managed []domain.VPNUserConfig
}

func (e *templateSnapshotEngine) TemplateUserSnapshot(_ context.Context, managed []domain.VPNUserConfig) ([]domain.VPNUserConfig, error) {
	e.managed = append([]domain.VPNUserConfig(nil), managed...)
	return append([]domain.VPNUserConfig(nil), e.users...), nil
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

func TestBuildSnapshotAddsTemplateUsersAsRegularUsers(t *testing.T) {
	engine := &templateSnapshotEngine{users: []domain.VPNUserConfig{
		{Email: "ops@example.test", UUID: "ops-id"},
		{Email: "db@example.test", UUID: "must-not-override-db"},
	}}
	registry := snapshotRegistry{subscriptions: snapshotSubscriptionRepository{items: []domain.Subscription{
		{ID: "sub-1", Email: "db@example.test", UUID: "db-id", Status: "active"},
		{ID: "sub-2", Email: "inactive@example.test", UUID: "inactive-id", Status: "inactive"},
	}}}
	service := NewService(registry, engine, newTestStore(t), nil)

	users, err := service.BuildSnapshot(context.Background())
	require.NoError(t, err)
	require.Len(t, users, 2)
	require.Equal(t, []string{"db@example.test", "ops@example.test"}, []string{users[0].Email, users[1].Email})
	require.Equal(t, "db-id", users[0].UUID, "database users win on email collision")
	require.Equal(t, "ops-id", users[1].UUID)
	require.Len(t, engine.managed, 1)
	require.Equal(t, "db@example.test", engine.managed[0].Email)
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

func TestStoreRetainsLastReplicaTotalsWhenSlaveIsStale(t *testing.T) {
	store := newTestStore(t)
	require.NoError(t, store.ReplaceReplicaStats(context.Background(), "slave-a", []statsPoint{
		{Email: "traffic@example.test", Total: 150},
	}))
	require.NoError(t, store.db.Model(&replicaRow{}).
		Where("node_id = ?", "slave-a").
		Update("last_seen_at", time.Now().UTC().Add(-2*time.Minute)).Error)

	totals, report := store.CollectReplicaStats(context.Background(), []string{"slave-a"}, time.Minute)
	require.Equal(t, domain.SlaveReport{Enabled: true, TotalServers: 1, FailedServers: 1}, report)
	require.Equal(t, []domain.SlaveUserTotal{{Email: "traffic@example.test", Slave: 150}}, totals,
		"temporary connectivity loss must not erase the slave's last cumulative total")
}

type trafficStateWriterStub struct {
	received []pluginapi.TrafficSnapshot
}

func (*trafficStateWriterStub) LocalTrafficSnapshot(context.Context) ([]pluginapi.TrafficSnapshot, error) {
	return nil, nil
}

func (s *trafficStateWriterStub) SyncClusterTraffic(_ context.Context, totals []pluginapi.TrafficSnapshot) error {
	s.received = append([]pluginapi.TrafficSnapshot(nil), totals...)
	return nil
}

func TestPluginSyncMasterTrafficWritesReplicaTotals(t *testing.T) {
	store := newTestStore(t)
	require.NoError(t, store.ReplaceReplicaStats(context.Background(), "slave-a", []statsPoint{
		{Email: "traffic@example.test", Total: 150},
	}))
	writer := &trafficStateWriterStub{}
	plugin := New()
	plugin.trafficSnapshot = writer

	err := plugin.syncMasterTraffic(context.Background(), &Service{store: store}, Config{
		AllowedNodes:  []string{"slave-a"},
		StatsInterval: "30s",
	})
	require.NoError(t, err)
	require.Equal(t, []pluginapi.TrafficSnapshot{{
		Email: "traffic@example.test",
		Usage: pluginapi.TrafficUsage{DownloadBytes: 150},
	}}, writer.received)
}

type serializedTrafficStateWriter struct {
	mu     sync.Mutex
	active int
	max    int
}

func (*serializedTrafficStateWriter) LocalTrafficSnapshot(context.Context) ([]pluginapi.TrafficSnapshot, error) {
	return nil, nil
}

func (s *serializedTrafficStateWriter) SyncClusterTraffic(context.Context, []pluginapi.TrafficSnapshot) error {
	s.mu.Lock()
	s.active++
	if s.active > s.max {
		s.max = s.active
	}
	s.mu.Unlock()

	time.Sleep(25 * time.Millisecond)

	s.mu.Lock()
	s.active--
	s.mu.Unlock()
	return nil
}

func (s *serializedTrafficStateWriter) maxConcurrentWrites() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.max
}

func TestPluginSerializesConcurrentMasterTrafficWrites(t *testing.T) {
	store := newTestStore(t)
	require.NoError(t, store.ReplaceReplicaStats(context.Background(), "slave-a", []statsPoint{
		{Email: "traffic@example.test", Total: 150},
	}))
	writer := &serializedTrafficStateWriter{}
	plugin := New()
	plugin.trafficSnapshot = writer
	config := Config{AllowedNodes: []string{"slave-a"}, StatsInterval: "30s"}

	var wg sync.WaitGroup
	errs := make(chan error, 3)
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- plugin.syncMasterTraffic(context.Background(), &Service{store: store}, config)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	require.Equal(t, 1, writer.maxConcurrentWrites())
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
