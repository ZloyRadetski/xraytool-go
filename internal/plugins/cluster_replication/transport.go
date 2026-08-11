package clusterreplication

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"xraytool/internal/domain"
	"xraytool/internal/pluginapi"
	"xraytool/internal/plugins/cluster_replication/protocol"
)

type Config struct {
	Mode               string   `json:"mode"`
	NodeID             string   `json:"node_id"`
	ListenAddress      string   `json:"listen_address"`
	MasterAddress      string   `json:"master_address"`
	AllowedNodes       []string `json:"allowed_nodes"`
	CAFile             string   `json:"ca_file"`
	CertFile           string   `json:"cert_file"`
	KeyFile            string   `json:"key_file"`
	ServerName         string   `json:"server_name"`
	ReconnectInterval  string   `json:"reconnect_interval"`
	DriftInterval      string   `json:"drift_interval"`
	MasterScanInterval string   `json:"master_scan_interval"`
	StatsInterval      string   `json:"stats_interval"`
	RealityKeysPath    string   `json:"reality_keys_path"`
}

func (c Config) Validate() error {
	c.Mode = strings.ToLower(strings.TrimSpace(c.Mode))
	if c.Mode != "master" && c.Mode != "slave" {
		return fmt.Errorf("replication mode must be master or slave")
	}
	if strings.TrimSpace(c.NodeID) == "" {
		return fmt.Errorf("replication node_id is required")
	}
	if strings.TrimSpace(c.CAFile) == "" || strings.TrimSpace(c.CertFile) == "" || strings.TrimSpace(c.KeyFile) == "" {
		return fmt.Errorf("replication requires ca_file, cert_file and key_file for mutual TLS")
	}
	if c.Mode == "master" {
		if strings.TrimSpace(c.ListenAddress) == "" {
			return fmt.Errorf("master replication listen_address is required")
		}
		if len(c.AllowedNodes) == 0 {
			return fmt.Errorf("master replication allowed_nodes must not be empty")
		}
	} else if strings.TrimSpace(c.MasterAddress) == "" {
		return fmt.Errorf("slave replication master_address is required")
	}
	return nil
}

func (c Config) reconnectEvery() time.Duration {
	return configDuration(c.ReconnectInterval, 5*time.Second)
}
func (c Config) driftEvery() time.Duration { return configDuration(c.DriftInterval, time.Minute) }
func (c Config) scanEvery() time.Duration {
	return configDuration(c.MasterScanInterval, 30*time.Second)
}
func (c Config) statsEvery() time.Duration {
	return configDuration(c.StatsInterval, 30*time.Second)
}

func configDuration(raw string, fallback time.Duration) time.Duration {
	if value, err := time.ParseDuration(strings.TrimSpace(raw)); err == nil && value > 0 {
		return value
	}
	return fallback
}

func serverTLSConfig(config Config) (*tls.Config, error) {
	certificate, err := tls.LoadX509KeyPair(config.CertFile, config.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("load replication server certificate: %w", err)
	}
	ca, err := os.ReadFile(config.CAFile)
	if err != nil {
		return nil, fmt.Errorf("read replication CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("replication CA does not contain a certificate")
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{certificate},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
	}, nil
}

func clientTLSConfig(config Config) (*tls.Config, error) {
	certificate, err := tls.LoadX509KeyPair(config.CertFile, config.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("load replication client certificate: %w", err)
	}
	ca, err := os.ReadFile(config.CAFile)
	if err != nil {
		return nil, fmt.Errorf("read replication CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("replication CA does not contain a certificate")
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{certificate},
		RootCAs:      pool,
		ServerName:   strings.TrimSpace(config.ServerName),
	}, nil
}

type helloPayload struct {
	NodeID       string `json:"node_id"`
	LastRevision int64  `json:"last_revision"`
	Initialized  bool   `json:"initialized"`
}

type wireEvent struct {
	Kind     string `json:"kind"`
	Checksum string `json:"checksum"`
	Payload  string `json:"payload"`
}

type snapshotPayload struct {
	ID string `json:"id"`
}

type statsPoint struct {
	Email string `json:"email"`
	Total int64  `json:"total"`
}

type fraudAckPayload struct {
	EventIDs []string `json:"event_ids"`
}

type replicationServer struct {
	protocol.UnimplementedReplicationServer
	service   *Service
	config    Config
	log       *slog.Logger
	fraudSink func(context.Context, string, []pluginapi.FraudEvent) error
}

func (s *replicationServer) Connect(stream protocol.Replication_ConnectServer) error {
	first, err := receiveFrame(stream)
	if err != nil {
		return err
	}
	if first.Kind != protocol.KindHello {
		return fmt.Errorf("first replication frame must be hello")
	}
	var hello helloPayload
	if err := json.Unmarshal(first.Payload, &hello); err != nil {
		return fmt.Errorf("decode replication hello: %w", err)
	}
	if !s.nodeAllowed(hello.NodeID) {
		return fmt.Errorf("replication node %q is not allowed", hello.NodeID)
	}
	if peer, ok := peerCommonName(stream.Context()); ok && peer != hello.NodeID {
		return fmt.Errorf("replication hello node_id %q does not match client certificate %q", hello.NodeID, peer)
	}
	if err := s.service.store.MarkReplicaConnected(stream.Context(), hello.NodeID); err != nil {
		return fmt.Errorf("record replication node connection: %w", err)
	}

	// Receiving runs independently so a slow anti-fraud processor never blocks
	// desired-state delivery. Only this outer goroutine writes to stream: gRPC
	// permits one sender and one receiver concurrently, but not two senders.
	receiveDone := make(chan error, 1)
	fraudAcks := make(chan fraudAckPayload, 128)
	go func() {
		for {
			frame, receiveErr := receiveFrame(stream)
			if receiveErr != nil {
				receiveDone <- receiveErr
				return
			}
			if frame.Kind == protocol.KindFraudEvents {
				ack, fraudErr := s.ingestFraudEvents(stream.Context(), hello.NodeID, frame.Payload)
				if fraudErr != nil {
					// The slave keeps its event in the durable outbox until it gets an
					// explicit ACK. Do not tear down normal replication for a temporary
					// anti-fraud dependency or database failure.
					s.log.Warn("replication anti-fraud delivery deferred", "node", hello.NodeID, "err", fraudErr)
					continue
				}
				if len(ack.EventIDs) > 0 {
					select {
					case fraudAcks <- ack:
					default:
						s.log.Warn("replication anti-fraud ACK queue is full", "node", hello.NodeID, "count", len(ack.EventIDs))
					}
				}
				continue
			}
			if frame.Kind == protocol.KindStats {
				var points []statsPoint
				if decodeErr := json.Unmarshal(frame.Payload, &points); decodeErr != nil {
					receiveDone <- fmt.Errorf("decode replication statistics: %w", decodeErr)
					return
				}
				if storeErr := s.service.store.ReplaceReplicaStats(stream.Context(), hello.NodeID, points); storeErr != nil {
					receiveDone <- fmt.Errorf("store replication statistics: %w", storeErr)
					return
				}
				continue
			}
			if frame.Kind == protocol.KindAck {
				if ackErr := s.service.store.AcknowledgeReplica(stream.Context(), hello.NodeID, frame.Revision); ackErr != nil {
					receiveDone <- fmt.Errorf("record replication acknowledgement: %w", ackErr)
					return
				}
				continue
			}
			if frame.Kind != protocol.KindStatus {
				receiveDone <- fmt.Errorf("unexpected slave replication frame %d", frame.Kind)
				return
			}
		}
	}()

	lastSent := hello.LastRevision
	needsSnapshot := !hello.Initialized
	if !needsSnapshot {
		oldest, oldestErr := s.service.store.OldestRevision(stream.Context())
		if oldestErr != nil {
			return oldestErr
		}
		if oldest > 0 && hello.LastRevision < oldest-1 {
			needsSnapshot = true
		}
	}
	if needsSnapshot {
		lastSent, err = s.sendSnapshot(stream.Context(), stream)
		if err != nil {
			return err
		}
	}

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if err := s.sendEventsAfter(stream.Context(), stream, &lastSent); err != nil {
			return err
		}
		select {
		case <-stream.Context().Done():
			return stream.Context().Err()
		case receiveErr := <-receiveDone:
			if receiveErr == io.EOF {
				return nil
			}
			return receiveErr
		case ack := <-fraudAcks:
			payload, marshalErr := json.Marshal(ack)
			if marshalErr != nil {
				return fmt.Errorf("encode replication anti-fraud acknowledgement: %w", marshalErr)
			}
			if err := sendFrame(stream, protocol.Frame{Kind: protocol.KindFraudAck, Payload: payload}); err != nil {
				return err
			}
		case <-ticker.C:
		}
	}
}

// ingestFraudEvents accepts a slave batch and returns an ACK only after every
// identified event is durably handed to the anti-fraud provider. Empty IDs are
// treated as the legacy best-effort format so an already deployed old slave
// can still report observations during the rollout.
func (s *replicationServer) ingestFraudEvents(ctx context.Context, sourceID string, payload []byte) (fraudAckPayload, error) {
	var events []FraudEvent
	if err := json.Unmarshal(payload, &events); err != nil {
		return fraudAckPayload{}, fmt.Errorf("decode replication anti-fraud events: %w", err)
	}
	if len(events) == 0 {
		return fraudAckPayload{}, fmt.Errorf("replication anti-fraud events are empty")
	}
	legacy := events[0].ID == ""
	for _, event := range events {
		if event.Email == "" || event.IP == "" || (legacy && event.ID != "") || (!legacy && event.ID == "") {
			return fraudAckPayload{}, fmt.Errorf("invalid replication anti-fraud event")
		}
	}

	if legacy {
		legacyEvents := make([]pluginapi.FraudEvent, len(events))
		for index, event := range events {
			legacyEvents[index] = pluginapi.FraudEvent{Email: event.Email, IP: event.IP, OccurredAt: event.OccurredAt, HashKeyID: event.HashKeyID}
		}
		if s.fraudSink == nil {
			s.log.Warn("replication legacy anti-fraud event ignored: sink is unavailable", "node", sourceID)
			return fraudAckPayload{}, nil
		}
		if err := s.fraudSink(ctx, sourceID, legacyEvents); err != nil {
			s.log.Warn("replication legacy anti-fraud event rejected", "node", sourceID, "err", err)
		}
		return fraudAckPayload{}, nil
	}

	if s.fraudSink == nil {
		return fraudAckPayload{}, fmt.Errorf("anti-fraud sink is unavailable")
	}
	if s.service == nil || s.service.store == nil {
		return fraudAckPayload{}, fmt.Errorf("replication anti-fraud store is unavailable")
	}
	pending, err := s.service.store.UnreceivedFraudEvents(ctx, sourceID, events)
	if err != nil {
		return fraudAckPayload{}, err
	}
	if len(pending) > 0 {
		toIngest := make([]pluginapi.FraudEvent, len(pending))
		for index, event := range pending {
			toIngest[index] = pluginapi.FraudEvent{Email: event.Email, IP: event.IP, OccurredAt: event.OccurredAt, HashKeyID: event.HashKeyID}
		}
		if err := s.fraudSink(ctx, sourceID, toIngest); err != nil {
			return fraudAckPayload{}, fmt.Errorf("deliver replication anti-fraud events: %w", err)
		}
		if err := s.service.store.MarkFraudReceived(ctx, sourceID, pending); err != nil {
			return fraudAckPayload{}, err
		}
	}
	ids := make([]string, 0, len(events))
	for _, event := range events {
		ids = append(ids, event.ID)
	}
	return fraudAckPayload{EventIDs: ids}, nil
}

func (s *replicationServer) sendEventsAfter(ctx context.Context, stream protocol.Replication_ConnectServer, lastSent *int64) error {
	events, err := s.service.store.EventsAfter(ctx, *lastSent, 500)
	if err != nil {
		return err
	}
	for _, event := range events {
		if event.Kind == eventResnapshot {
			revision, snapshotErr := s.sendSnapshot(ctx, stream)
			if snapshotErr != nil {
				return snapshotErr
			}
			*lastSent = revision
			// The snapshot already includes the latest state and artifacts. Do not
			// continue iterating a batch read before it: it can contain stale
			// resnapshot markers and would stream the same full snapshot repeatedly.
			return nil
		}
		payload, marshalErr := json.Marshal(wireEvent{
			Kind: event.Kind, Checksum: event.Checksum, Payload: base64.StdEncoding.EncodeToString(event.Payload),
		})
		if marshalErr != nil {
			return marshalErr
		}
		if err := sendFrame(stream, protocol.Frame{Kind: protocol.KindEvent, Revision: event.Revision, Payload: payload}); err != nil {
			return err
		}
		*lastSent = event.Revision
	}
	return nil
}

func (s *replicationServer) sendSnapshot(ctx context.Context, stream protocol.Replication_ConnectServer) (int64, error) {
	// A snapshot is a complete desired-state transfer. Refresh the remaining
	// configuration artifacts here as well as in the periodic master loop.
	if count, err := s.service.PublishArtifacts(ctx, s.config.RealityKeysPath); err != nil {
		return 0, fmt.Errorf("refresh replication artifacts for snapshot: %w", err)
	} else if count > 0 {
		s.log.Info("replication snapshot artifacts refreshed", "count", count)
	}

	// Capture the revision before reading state. A concurrent change can appear
	// in both this snapshot and the later delta, which is harmless because user
	// upserts are idempotent; it cannot be omitted from both.
	revision, err := s.service.store.LatestRevision(ctx)
	if err != nil {
		return 0, err
	}
	users, err := s.service.BuildSnapshot(ctx)
	if err != nil {
		return 0, err
	}
	identifier := fmt.Sprintf("%s-%d", s.config.NodeID, time.Now().UTC().UnixNano())
	begin, _ := json.Marshal(snapshotPayload{ID: identifier})
	if err := sendFrame(stream, protocol.Frame{Kind: protocol.KindSnapshotBegin, Revision: revision, Payload: begin}); err != nil {
		return 0, err
	}
	for _, user := range users {
		payload, marshalErr := json.Marshal(userPayload{User: user})
		if marshalErr != nil {
			return 0, marshalErr
		}
		if err := sendFrame(stream, protocol.Frame{Kind: protocol.KindSnapshotUser, Revision: revision, Payload: payload}); err != nil {
			return 0, err
		}
	}
	artifacts, err := s.service.store.Artifacts(ctx)
	if err != nil {
		return 0, err
	}
	for _, artifact := range artifacts {
		if artifact.Kind == legacyStaticClientsArtifact {
			// Defensive compatibility filter for a stale row created by an older
			// release. Hardcoded users are snapshot users now, never artifacts.
			continue
		}
		payload, marshalErr := json.Marshal(artifactPayload{Kind: artifact.Kind, Data: base64.StdEncoding.EncodeToString(artifact.Payload)})
		if marshalErr != nil {
			return 0, marshalErr
		}
		if err := sendFrame(stream, protocol.Frame{Kind: protocol.KindSnapshotArtifact, Revision: revision, Payload: payload}); err != nil {
			return 0, err
		}
	}
	if err := sendFrame(stream, protocol.Frame{Kind: protocol.KindSnapshotEnd, Revision: revision, Payload: begin}); err != nil {
		return 0, err
	}
	return revision, nil
}

func (s *replicationServer) nodeAllowed(nodeID string) bool {
	for _, allowed := range s.config.AllowedNodes {
		if strings.TrimSpace(allowed) == nodeID {
			return true
		}
	}
	return false
}

func peerCommonName(ctx context.Context) (string, bool) {
	connectionPeer, ok := peer.FromContext(ctx)
	if !ok || connectionPeer.AuthInfo == nil {
		return "", false
	}
	tlsInfo, ok := connectionPeer.AuthInfo.(credentials.TLSInfo)
	if !ok || len(tlsInfo.State.PeerCertificates) == 0 {
		return "", false
	}
	return tlsInfo.State.PeerCertificates[0].Subject.CommonName, true
}

func startMasterTransport(ctx context.Context, service *Service, config Config, log *slog.Logger, fraudSink func(context.Context, string, []pluginapi.FraudEvent) error) (func() error, error) {
	tlsConfig, err := serverTLSConfig(config)
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", config.ListenAddress)
	if err != nil {
		return nil, fmt.Errorf("listen replication gRPC: %w", err)
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsConfig)))
	protocol.RegisterReplicationServer(server, &replicationServer{service: service, config: config, log: log, fraudSink: fraudSink})
	go func() {
		if serveErr := server.Serve(listener); serveErr != nil && serveErr != grpc.ErrServerStopped {
			log.Error("replication gRPC server stopped", "err", serveErr)
		}
	}()
	stop := func() error {
		server.GracefulStop()
		return listener.Close()
	}
	return stop, nil
}

type slaveSession struct {
	stream protocol.Replication_ConnectClient
	mu     sync.Mutex
}

func (s *slaveSession) send(frame protocol.Frame) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return sendFrame(s.stream, frame)
}

func runSlaveSession(
	ctx context.Context,
	service *Service,
	config Config,
	log *slog.Logger,
	onSession func(*slaveSession),
	onFraudAck func(context.Context, []string) error,
) error {
	tlsConfig, err := clientTLSConfig(config)
	if err != nil {
		return err
	}
	connection, err := grpc.DialContext(ctx, config.MasterAddress, grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)), grpc.WithBlock())
	if err != nil {
		return fmt.Errorf("dial replication master: %w", err)
	}
	defer connection.Close()
	stream, err := protocol.NewReplicationClient(connection).Connect(ctx)
	if err != nil {
		return fmt.Errorf("open replication stream: %w", err)
	}
	lastRevision := currentRevision(ctx, service.store)
	hello, _ := json.Marshal(helloPayload{NodeID: config.NodeID, LastRevision: lastRevision, Initialized: snapshotInitialized(ctx, service.store)})
	if err := sendFrame(stream, protocol.Frame{Kind: protocol.KindHello, Payload: hello}); err != nil {
		return err
	}
	session := &slaveSession{stream: stream}
	if onSession != nil {
		onSession(session)
		defer onSession(nil)
	}
	return receiveSlaveFrames(ctx, service, config, session, log, onFraudAck)
}

func receiveSlaveFrames(
	ctx context.Context,
	service *Service,
	config Config,
	session *slaveSession,
	log *slog.Logger,
	onFraudAck func(context.Context, []string) error,
) error {
	var activeSnapshot string
	for {
		message, err := session.stream.Recv()
		if err != nil {
			return err
		}
		frame, err := protocol.Unmarshal(message.Value)
		if err != nil {
			return err
		}
		switch frame.Kind {
		case protocol.KindFraudAck:
			var payload fraudAckPayload
			if err := json.Unmarshal(frame.Payload, &payload); err != nil {
				return fmt.Errorf("decode replication anti-fraud acknowledgement: %w", err)
			}
			if len(nonEmptyStrings(payload.EventIDs)) == 0 {
				return fmt.Errorf("replication anti-fraud acknowledgement is empty")
			}
			if onFraudAck != nil {
				if err := onFraudAck(ctx, payload.EventIDs); err != nil {
					return fmt.Errorf("apply replication anti-fraud acknowledgement: %w", err)
				}
			}
		case protocol.KindEvent:
			var wire wireEvent
			if err := json.Unmarshal(frame.Payload, &wire); err != nil {
				return err
			}
			payload, err := base64.StdEncoding.DecodeString(wire.Payload)
			if err != nil {
				return err
			}
			event := Event{Revision: frame.Revision, Kind: wire.Kind, Checksum: wire.Checksum, Payload: payload}
			if err := service.ApplyEvent(ctx, config.MasterAddress, event, config.RealityKeysPath); err != nil {
				return err
			}
			if err := session.send(protocol.Frame{Kind: protocol.KindAck, Revision: frame.Revision}); err != nil {
				return err
			}
		case protocol.KindSnapshotBegin:
			var payload snapshotPayload
			if err := json.Unmarshal(frame.Payload, &payload); err != nil {
				return err
			}
			if payload.ID == "" {
				return fmt.Errorf("replication snapshot has empty id")
			}
			activeSnapshot = payload.ID
		case protocol.KindSnapshotUser:
			if activeSnapshot == "" {
				return fmt.Errorf("replication snapshot user received without begin")
			}
			var payload userPayload
			if err := json.Unmarshal(frame.Payload, &payload); err != nil {
				return err
			}
			if payload.User.Email == "" {
				return fmt.Errorf("replication snapshot contains user without email")
			}
			if err := service.store.StageUser(ctx, activeSnapshot, payload.User.Email, frame.Payload); err != nil {
				return err
			}
		case protocol.KindSnapshotArtifact:
			if activeSnapshot == "" {
				return fmt.Errorf("replication snapshot artifact received without begin")
			}
			if err := service.applyArtifact(ctx, frame.Payload, frame.Revision, config.RealityKeysPath); err != nil {
				return err
			}
		case protocol.KindSnapshotEnd:
			var payload snapshotPayload
			if err := json.Unmarshal(frame.Payload, &payload); err != nil {
				return err
			}
			if activeSnapshot == "" || payload.ID != activeSnapshot {
				return fmt.Errorf("replication snapshot end does not match active snapshot")
			}
			rawUsers, err := service.store.ActivateSnapshot(ctx, activeSnapshot, frame.Revision)
			if err != nil {
				return err
			}
			users, err := decodeSnapshotUsers(rawUsers)
			if err != nil {
				return err
			}
			if err := service.reconcileUsers(ctx, users); err != nil {
				return fmt.Errorf("apply replication snapshot: %w", err)
			}
			activeSnapshot = ""
			if err := session.send(protocol.Frame{Kind: protocol.KindAck, Revision: frame.Revision}); err != nil {
				return err
			}
		case protocol.KindError:
			return fmt.Errorf("master replication error: %s", string(frame.Payload))
		default:
			return fmt.Errorf("unexpected master replication frame %d", frame.Kind)
		}
	}
}

func decodeSnapshotUsers(payloads [][]byte) ([]domain.VPNUserConfig, error) {
	users := make([]domain.VPNUserConfig, 0, len(payloads))
	for _, raw := range payloads {
		var payload userPayload
		if err := json.Unmarshal(raw, &payload); err != nil {
			return nil, err
		}
		users = append(users, payload.User)
	}
	return users, nil
}

func currentRevision(ctx context.Context, store *Store) int64 {
	raw, exists, err := store.GetMeta(ctx, "applied_revision")
	if err != nil || !exists {
		return 0
	}
	revision, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0
	}
	return revision
}

func snapshotInitialized(ctx context.Context, store *Store) bool {
	value, exists, err := store.GetMeta(ctx, "snapshot_initialized")
	return err == nil && exists && value == "true"
}

func sendFrame(stream interface {
	Send(*wrapperspb.BytesValue) error
}, frame protocol.Frame) error {
	return stream.Send(wrapperspb.Bytes(protocol.Marshal(frame)))
}

func receiveFrame(stream interface {
	Recv() (*wrapperspb.BytesValue, error)
}) (protocol.Frame, error) {
	message, err := stream.Recv()
	if err != nil {
		return protocol.Frame{}, err
	}
	return protocol.Unmarshal(message.Value)
}
