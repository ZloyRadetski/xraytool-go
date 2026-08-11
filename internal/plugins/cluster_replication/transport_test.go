package clusterreplication

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

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
