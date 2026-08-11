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
	payload, err := json.Marshal([]pluginapi.FraudEvent{{Email: "user@example.test", IP: "hash"}})
	require.NoError(t, err)

	server := &replicationServer{
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		fraudSink: func(context.Context, string, []pluginapi.FraudEvent) error {
			return errors.New("temporary anti-fraud failure")
		},
	}

	require.NoError(t, server.ingestFraudEvents(context.Background(), "slave-a", payload))
}

func TestReplicationServerUnavailableFraudSinkDoesNotFailStream(t *testing.T) {
	payload, err := json.Marshal([]pluginapi.FraudEvent{{Email: "user@example.test", IP: "hash"}})
	require.NoError(t, err)

	server := &replicationServer{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	require.NoError(t, server.ingestFraudEvents(context.Background(), "slave-a", payload))
}

func TestReplicationServerRejectsInvalidFraudPayload(t *testing.T) {
	server := &replicationServer{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	require.Error(t, server.ingestFraudEvents(context.Background(), "slave-a", []byte("not json")))
}
