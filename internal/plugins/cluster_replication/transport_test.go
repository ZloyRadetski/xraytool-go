package clusterreplication

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
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

func TestRoutingReplication_MasterPublishesArtifacts(t *testing.T) {
	store := newTestStore(t)
	service := &Service{store: store}
	ctx := context.Background()

	tmpRoutingDir := t.TempDir()
	outboundsDir := filepath.Join(tmpRoutingDir, "outbounds")
	require.NoError(t, os.MkdirAll(outboundsDir, 0o755))

	// Write server config and outbound template
	require.NoError(t, os.WriteFile(filepath.Join(tmpRoutingDir, "msk-slave.json"), []byte(`{"server":"msk-slave","rules":[]}`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(outboundsDir, "nld-master.json"), []byte(`{"tag":"relay_nld-master"}`), 0o644))

	count, err := service.PublishArtifacts(ctx, "", tmpRoutingDir)
	require.NoError(t, err)
	require.Equal(t, 2, count)

	// Verify artifacts are stored
	arts, err := store.Artifacts(ctx)
	require.NoError(t, err)
	require.Len(t, arts, 2)

	foundMsk := false
	foundNld := false
	for _, a := range arts {
		if a.Kind == "routing_file:msk-slave.json" {
			foundMsk = true
			require.Contains(t, string(a.Payload), "msk-slave")
		}
		if a.Kind == "routing_file:outbounds/nld-master.json" {
			foundNld = true
			require.Contains(t, string(a.Payload), "relay_nld-master")
		}
	}
	require.True(t, foundMsk)
	require.True(t, foundNld)
}

func TestRoutingReplication_SlaveAppliesXrayConfig(t *testing.T) {
	store := newTestStore(t)
	service := &Service{store: store}
	ctx := context.Background()

	restarted := false
	service.SetRestartFunc(func(ctx context.Context) error {
		restarted = true
		return nil
	})

	slaveRoutingDir := t.TempDir()
	outboundsDir := filepath.Join(slaveRoutingDir, "outbounds")
	require.NoError(t, os.MkdirAll(outboundsDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(outboundsDir, "nld-master.json"), []byte(`{"protocol":"freedom","tag":"relay_nld-master"}`), 0o644))

	xrayCfgPath := filepath.Join(t.TempDir(), "config.json")
	require.NoError(t, os.WriteFile(xrayCfgPath, []byte(`{"routing":{"rules":[]},"outbounds":[]}`), 0o644))

	ruleContent := `{"server":"msk-slave","rules":[{"id":"r1","name":"Telegram","source_server":"msk-slave","target_server":"nld-master","domain":["geosite:telegram"],"priority":1,"enabled":true}]}`
	payload, _ := json.Marshal(artifactPayload{
		Kind: "routing_file:msk-slave.json",
		Data: base64.StdEncoding.EncodeToString([]byte(ruleContent)),
	})
	event := Event{
		Revision: 1,
		Kind:     eventArtifact,
		Checksum: checksum(eventArtifact, payload),
		Payload:  payload,
	}

	err := service.ApplyEvent(ctx, "master", event, "", slaveRoutingDir, "msk-slave", xrayCfgPath)
	require.NoError(t, err)
	require.True(t, restarted)

	// Verify file was written to disk
	writtenFile, err := os.ReadFile(filepath.Join(slaveRoutingDir, "msk-slave.json"))
	require.NoError(t, err)
	require.Contains(t, string(writtenFile), "Telegram")

	// Verify Xray config was patched
	xrayData, err := os.ReadFile(xrayCfgPath)
	require.NoError(t, err)
	require.Contains(t, string(xrayData), "geosite:telegram")
	require.Contains(t, string(xrayData), "relay_nld-master")
}

func TestRoutingReplication_SlaveRollbackOnRestartFailure(t *testing.T) {
	store := newTestStore(t)
	service := &Service{store: store}
	ctx := context.Background()

	service.SetRestartFunc(func(ctx context.Context) error {
		return errors.New("simulated systemd failure")
	})

	slaveRoutingDir := t.TempDir()
	outboundsDir := filepath.Join(slaveRoutingDir, "outbounds")
	require.NoError(t, os.MkdirAll(outboundsDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(outboundsDir, "nld-master.json"), []byte(`{"protocol":"freedom","tag":"relay_nld-master"}`), 0o644))

	xrayCfgPath := filepath.Join(t.TempDir(), "config.json")
	origConfig := `{"routing":{"rules":[{"type":"field","outboundTag":"direct"}]},"outbounds":[]}`
	require.NoError(t, os.WriteFile(xrayCfgPath, []byte(origConfig), 0o644))

	ruleContent := `{"server":"msk-slave","rules":[{"id":"r1","name":"Telegram","source_server":"msk-slave","target_server":"nld-master","domain":["geosite:telegram"],"priority":1,"enabled":true}]}`
	payload, _ := json.Marshal(artifactPayload{
		Kind: "routing_file:msk-slave.json",
		Data: base64.StdEncoding.EncodeToString([]byte(ruleContent)),
	})
	event := Event{
		Revision: 1,
		Kind:     eventArtifact,
		Checksum: checksum(eventArtifact, payload),
		Payload:  payload,
	}

	err := service.ApplyEvent(ctx, "master", event, "", slaveRoutingDir, "msk-slave", xrayCfgPath)
	require.Error(t, err)

	// Verify Xray config was rolled back to original
	xrayData, err := os.ReadFile(xrayCfgPath)
	require.NoError(t, err)
	require.Equal(t, origConfig, string(xrayData))
}

func TestRoutingReplication_PathTraversalRejected(t *testing.T) {
	store := newTestStore(t)
	service := &Service{store: store}
	ctx := context.Background()

	slaveRoutingDir := t.TempDir()
	payload, _ := json.Marshal(artifactPayload{
		Kind: "routing_file:../../evil.txt",
		Data: base64.StdEncoding.EncodeToString([]byte("malicious content")),
	})
	event := Event{
		Revision: 1,
		Kind:     eventArtifact,
		Checksum: checksum(eventArtifact, payload),
		Payload:  payload,
	}

	err := service.ApplyEvent(ctx, "master", event, "", slaveRoutingDir, "msk-slave", "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "path traversal")
}
