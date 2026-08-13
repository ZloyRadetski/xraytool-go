package clusterreplication

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	json "github.com/goccy/go-json"
	"github.com/stretchr/testify/require"

	"xraytool/internal/pluginapi"
)

func TestReplicationServerFraudSinkFailureDoesNotFailStream(t *testing.T) {
	store := newTestStore(t)
	payload, err := json.Marshal([]FraudEvent{{ID: "event-1", Email: "user@example.test", IP: "hash"}})
	require.NoError(t, err)

	server := &replicationServer{
		service: &Service{store: store},
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		fraudSink: func(context.Context, string, []pluginapi.FraudEvent) error {
			return errors.New("temporary anti-fraud failure")
		},
	}

	ack, err := server.ingestFraudEvents(context.Background(), "slave-a", payload)
	require.Error(t, err)
	require.Empty(t, ack.EventIDs)
	pending, err := store.UnreceivedFraudEvents(context.Background(), "slave-a", []FraudEvent{{ID: "event-1", Email: "user@example.test", IP: "hash"}})
	require.NoError(t, err)
	require.Len(t, pending, 1)
}

func TestReplicationServerFraudDeliveryIsIdempotentAndAcknowledged(t *testing.T) {
	store := newTestStore(t)
	payload, err := json.Marshal([]FraudEvent{{ID: "event-1", Email: "user@example.test", IP: "hash"}})
	require.NoError(t, err)

	var delivered [][]pluginapi.FraudEvent
	server := &replicationServer{
		service: &Service{store: store},
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		fraudSink: func(_ context.Context, _ string, events []pluginapi.FraudEvent) error {
			delivered = append(delivered, events)
			return nil
		},
	}

	ack, err := server.ingestFraudEvents(context.Background(), "slave-a", payload)
	require.NoError(t, err)
	require.Equal(t, []string{"event-1"}, ack.EventIDs)
	require.Len(t, delivered, 1)
	require.Equal(t, "hash", delivered[0][0].IP)

	ack, err = server.ingestFraudEvents(context.Background(), "slave-a", payload)
	require.NoError(t, err)
	require.Equal(t, []string{"event-1"}, ack.EventIDs)
	require.Len(t, delivered, 1, "a retried acknowledged event must not be delivered twice")
}

func TestReplicationServerRejectsInvalidFraudPayload(t *testing.T) {
	server := &replicationServer{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	_, err := server.ingestFraudEvents(context.Background(), "slave-a", []byte("not json"))
	require.Error(t, err)
}

func TestReplicationServerAcceptsLegacyFraudPayloadWithoutAck(t *testing.T) {
	payload, err := json.Marshal([]pluginapi.FraudEvent{{Email: "user@example.test", IP: "hash"}})
	require.NoError(t, err)

	called := false
	server := &replicationServer{
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		fraudSink: func(context.Context, string, []pluginapi.FraudEvent) error {
			called = true
			return nil
		},
	}
	ack, err := server.ingestFraudEvents(context.Background(), "slave-a", payload)
	require.NoError(t, err)
	require.Empty(t, ack.EventIDs)
	require.True(t, called)
}

type schedulerTrafficSnapshot struct{ calls chan struct{} }

func (s *schedulerTrafficSnapshot) LocalTrafficSnapshot(context.Context) ([]pluginapi.TrafficSnapshot, error) {
	select {
	case s.calls <- struct{}{}:
	default:
	}
	return nil, nil
}

func TestSlaveSchedulerReportsTrafficWhileSessionStaysOpen(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	plugin := New()
	plugin.config = Config{Mode: "slave"}
	provider := &schedulerTrafficSnapshot{calls: make(chan struct{}, 1)}
	plugin.trafficSnapshot = provider
	started := make(chan struct{})
	done := make(chan struct{})
	runner := func(ctx context.Context, _ *Service, _ Config, _ *slog.Logger, _ func(*slaveSession), _ func(context.Context, []string) error) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}
	config := Config{
		Mode:              "slave",
		StatsInterval:     "10ms",
		DriftInterval:     "1h",
		ReconnectInterval: "1h",
	}
	go func() {
		plugin.runSlaveLoopWithRunner(ctx, nil, config, slog.New(slog.NewTextHandler(io.Discard, nil)), runner)
		close(done)
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("slave session was not started")
	}
	select {
	case <-provider.calls:
	case <-time.After(time.Second):
		t.Fatal("traffic report was not scheduled while the slave session remained open")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("slave scheduler did not stop after cancellation")
	}
}
