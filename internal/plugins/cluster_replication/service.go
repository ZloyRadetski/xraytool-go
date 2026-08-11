package clusterreplication

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	json "github.com/goccy/go-json"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"xraytool/internal/domain"
	vpn "xraytool/internal/plugins/engine_xray"
)

const (
	artifactStaticClients = "static_clients"
	artifactRealityKeys   = "reality_keys"
)

type userPayload struct {
	User domain.VPNUserConfig `json:"user"`
}

type removePayload struct {
	Email string `json:"email"`
}

type artifactPayload struct {
	Kind string `json:"kind"`
	Data string `json:"data"`
}

// Service owns the durable replication journal and desired-state projection.
// It intentionally has no HTTP dependencies: all remote delivery is performed
// through the plugin's gRPC stream.
type Service struct {
	registry domain.Registry
	engine   domain.Engine
	store    *Store
	log      *slog.Logger

	reconcileMu sync.Mutex
}

func NewService(registry domain.Registry, engine domain.Engine, store *Store, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{registry: registry, engine: engine, store: store, log: log.With("component", "cluster_replication")}
}

func (s *Service) Store() *Store { return s.store }

func (s *Service) BuildSnapshot(ctx context.Context) ([]domain.VPNUserConfig, error) {
	subscriptions, err := s.registry.Subscriptions().FindAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("load subscriptions for replication snapshot: %w", err)
	}
	blocked := s.blockedEmails(ctx)
	users := make([]domain.VPNUserConfig, 0, len(subscriptions))
	for _, subscription := range subscriptions {
		if subscription.Email == "" || subscription.UUID == "" || subscription.Status != "active" || blocked[subscription.Email] {
			continue
		}
		users = append(users, vpn.SubscriptionToVPNUserConfig(subscription))
	}
	sort.Slice(users, func(i, j int) bool { return users[i].Email < users[j].Email })
	return users, nil
}

func (s *Service) blockedEmails(ctx context.Context) map[string]bool {
	blocked := make(map[string]bool)
	if s.registry == nil {
		return blocked
	}
	users, err := s.registry.Users().FindAll(ctx)
	if err == nil {
		blockedIDs := make(map[string]struct{})
		for _, user := range users {
			if user.IsBlocked {
				blockedIDs[user.ID] = struct{}{}
			}
		}
		if len(blockedIDs) > 0 {
			subscriptions, subscriptionsErr := s.registry.Subscriptions().FindAll(ctx)
			if subscriptionsErr == nil {
				for _, subscription := range subscriptions {
					if _, ok := blockedIDs[subscription.UserID]; ok && subscription.Email != "" {
						blocked[subscription.Email] = true
					}
				}
			}
		}
	}
	bans, err := s.registry.AntifraudBans().FindActive(ctx)
	if err == nil {
		for _, ban := range bans {
			blocked[ban.Email] = true
		}
	}
	return blocked
}

func (s *Service) PublishUser(ctx context.Context, user domain.VPNUserConfig) (Event, error) {
	payload, err := json.Marshal(userPayload{User: user})
	if err != nil {
		return Event{}, fmt.Errorf("marshal user replication event: %w", err)
	}
	return s.store.Append(ctx, eventUserUpsert, payload)
}

func (s *Service) PublishRemove(ctx context.Context, email string) (Event, error) {
	payload, err := json.Marshal(removePayload{Email: email})
	if err != nil {
		return Event{}, fmt.Errorf("marshal remove replication event: %w", err)
	}
	return s.store.Append(ctx, eventUserRemove, payload)
}

// PublishCurrentUser is used after an engine-level mutation that does not
// carry a complete user object (for example SetLimit). If the user no longer
// belongs to the desired master state it writes a remove event instead.
func (s *Service) PublishCurrentUser(ctx context.Context, email string) error {
	users, err := s.BuildSnapshot(ctx)
	if err != nil {
		return err
	}
	for _, user := range users {
		if user.Email == email {
			_, err := s.PublishUser(ctx, user)
			return err
		}
	}
	_, err = s.PublishRemove(ctx, email)
	return err
}

// RequestSnapshot appends a compact control marker. The master expands it into
// per-user gRPC frames for each connected slave; callers never construct a
// large configuration payload themselves.
func (s *Service) RequestSnapshot(ctx context.Context, reason string) error {
	_, err := s.store.Append(ctx, eventResnapshot, []byte(strings.TrimSpace(reason)))
	return err
}

// DetectDesiredState appends one compact resnapshot marker only when a change
// occurred outside the normal engine mutation path. The marker contains no
// giant state payload; a connected slave receives the snapshot as a streamed
// sequence of user frames.
func (s *Service) DetectDesiredState(ctx context.Context) (bool, error) {
	users, err := s.BuildSnapshot(ctx)
	if err != nil {
		return false, err
	}
	digest, err := desiredDigest(users)
	if err != nil {
		return false, err
	}
	previous, exists, err := s.store.GetMeta(ctx, "master_desired_digest")
	if err != nil {
		return false, err
	}
	if exists && previous == digest {
		return false, nil
	}
	if _, err := s.store.Append(ctx, eventResnapshot, []byte(digest)); err != nil {
		return false, err
	}
	if err := s.store.PutMeta(ctx, "master_desired_digest", digest); err != nil {
		return false, err
	}
	return true, nil
}

func desiredDigest(users []domain.VPNUserConfig) (string, error) {
	payload, err := json.Marshal(users)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

func (s *Service) PublishArtifacts(ctx context.Context, realityKeysPath string) (int, error) {
	users, err := s.BuildSnapshot(ctx)
	if err != nil {
		return 0, err
	}
	artifacts := make(map[string][]byte)
	if synchronizer, ok := staticClientSynchronizer(s.engine); ok {
		clients, snapshotErr := synchronizer.StaticClientSnapshot(ctx, users)
		if snapshotErr != nil {
			return 0, fmt.Errorf("build static client artifact: %w", snapshotErr)
		}
		payload, marshalErr := json.Marshal(clients)
		if marshalErr != nil {
			return 0, marshalErr
		}
		artifacts[artifactStaticClients] = payload
	}
	if path := strings.TrimSpace(realityKeysPath); path != "" {
		payload, readErr := os.ReadFile(path)
		if readErr != nil {
			if !os.IsNotExist(readErr) {
				return 0, fmt.Errorf("read reality key artifact: %w", readErr)
			}
		} else {
			artifacts[artifactRealityKeys] = payload
		}
	}

	published := 0
	for kind, data := range artifacts {
		key := "master_artifact_digest_" + kind
		digest := checksum(kind, data)
		previous, exists, metaErr := s.store.GetMeta(ctx, key)
		if metaErr != nil {
			return published, metaErr
		}
		if exists && previous == digest {
			continue
		}
		payload, marshalErr := json.Marshal(artifactPayload{Kind: kind, Data: base64.StdEncoding.EncodeToString(data)})
		if marshalErr != nil {
			return published, marshalErr
		}
		event, appendErr := s.store.Append(ctx, eventArtifact, payload)
		if appendErr != nil {
			return published, appendErr
		}
		if err := s.store.PutArtifact(ctx, kind, data, event.Revision); err != nil {
			return published, err
		}
		if err := s.store.PutMeta(ctx, key, digest); err != nil {
			return published, err
		}
		published++
	}
	return published, nil
}

func staticClientSynchronizer(engine domain.Engine) (domain.StaticClientSynchronizer, bool) {
	if probe, ok := engine.(interface{ SupportsStaticClientSync() bool }); ok && !probe.SupportsStaticClientSync() {
		return nil, false
	}
	synchronizer, ok := engine.(domain.StaticClientSynchronizer)
	return synchronizer, ok
}

func (s *Service) ApplyEvent(ctx context.Context, masterID string, event Event, realityKeysPath string) error {
	if checksum(event.Kind, event.Payload) != event.Checksum {
		return fmt.Errorf("replication event %d checksum mismatch", event.Revision)
	}
	alreadyApplied, err := s.store.AlreadyApplied(ctx, masterID, event.Revision)
	if err != nil {
		return fmt.Errorf("check replication inbox: %w", err)
	}
	if alreadyApplied {
		return nil
	}

	switch event.Kind {
	case eventUserUpsert:
		var payload userPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return fmt.Errorf("decode user event %d: %w", event.Revision, err)
		}
		if payload.User.Email == "" {
			return fmt.Errorf("replication event %d has empty user email", event.Revision)
		}
		if err := s.engine.AddUser(ctx, payload.User); err != nil {
			return fmt.Errorf("apply user event %d: %w", event.Revision, err)
		}
		if err := s.store.UpsertDesiredUser(ctx, payload.User.Email, event.Payload, event.Revision); err != nil {
			return err
		}
	case eventUserRemove:
		var payload removePayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return fmt.Errorf("decode remove event %d: %w", event.Revision, err)
		}
		if payload.Email == "" {
			return fmt.Errorf("replication event %d has empty remove email", event.Revision)
		}
		if err := s.engine.RemoveUser(ctx, payload.Email); err != nil {
			return fmt.Errorf("apply remove event %d: %w", event.Revision, err)
		}
		if err := s.store.RemoveDesiredUser(ctx, payload.Email); err != nil {
			return err
		}
	case eventArtifact:
		if err := s.applyArtifact(ctx, event.Payload, event.Revision, realityKeysPath); err != nil {
			return err
		}
	case eventResnapshot:
		// The master expands this marker into Snapshot* frames. Receiving one
		// directly is a protocol violation rather than an instruction to guess.
		return fmt.Errorf("received unexpanded replication resnapshot marker")
	default:
		return fmt.Errorf("unknown replication event kind %q", event.Kind)
	}
	return s.store.MarkApplied(ctx, masterID, event)
}

func (s *Service) applyArtifact(ctx context.Context, raw []byte, revision int64, realityKeysPath string) error {
	var payload artifactPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return fmt.Errorf("decode replication artifact: %w", err)
	}
	data, err := base64.StdEncoding.DecodeString(payload.Data)
	if err != nil {
		return fmt.Errorf("decode replication artifact body: %w", err)
	}
	switch payload.Kind {
	case artifactStaticClients:
		synchronizer, ok := staticClientSynchronizer(s.engine)
		if !ok {
			return fmt.Errorf("selected engine cannot apply static client artifact")
		}
		var clients []domain.StaticInboundClients
		if err := json.Unmarshal(data, &clients); err != nil {
			return fmt.Errorf("decode static client artifact: %w", err)
		}
		if err := synchronizer.ApplyStaticClientSnapshot(ctx, clients); err != nil {
			return fmt.Errorf("apply static client artifact: %w", err)
		}
	case artifactRealityKeys:
		path := strings.TrimSpace(realityKeysPath)
		if path == "" {
			return fmt.Errorf("received reality key artifact but reality_keys_path is unset")
		}
		if err := writeSensitiveArtifact(path, data); err != nil {
			return fmt.Errorf("write reality key artifact: %w", err)
		}
		if synchronizer, ok := s.engine.(interface {
			SyncRealityKeys(context.Context, []byte) error
		}); ok {
			if err := synchronizer.SyncRealityKeys(ctx, data); err != nil {
				return fmt.Errorf("apply reality key artifact: %w", err)
			}
		}
	default:
		return fmt.Errorf("unknown replication artifact %q", payload.Kind)
	}
	return s.store.PutArtifact(ctx, payload.Kind, data, revision)
}

func writeSensitiveArtifact(path string, data []byte) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".replication-artifact-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer func() { _ = os.Remove(temporaryName) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryName, path)
}

func (s *Service) DesiredUsers(ctx context.Context) ([]domain.VPNUserConfig, error) {
	payloads, err := s.store.DesiredUsers(ctx)
	if err != nil {
		return nil, err
	}
	users := make([]domain.VPNUserConfig, 0, len(payloads))
	for _, payload := range payloads {
		var user userPayload
		if err := json.Unmarshal(payload, &user); err != nil {
			return nil, fmt.Errorf("decode desired replicated user: %w", err)
		}
		users = append(users, user.User)
	}
	return users, nil
}

// ReconcileSlave repairs local managed configuration from the persisted master
// projection. A manual deletion from one inbound is therefore restored on the
// next interval and is never sent back to the master.
func (s *Service) ReconcileSlave(ctx context.Context) error {
	if !s.reconcileMu.TryLock() {
		return nil
	}
	defer s.reconcileMu.Unlock()
	users, err := s.DesiredUsers(ctx)
	if err != nil {
		return err
	}
	if err := s.reconcileUsers(ctx, users); err != nil {
		return fmt.Errorf("reconcile slave desired state: %w", err)
	}
	return nil
}

// ReconcileDesiredState restores this node's generated configuration from the
// durable template and the current database snapshot. It deliberately bypasses
// the regular desired-state hash so a template-only correction takes effect
// even when the database users themselves did not change.
func (s *Service) ReconcileDesiredState(ctx context.Context) error {
	users, err := s.BuildSnapshot(ctx)
	if err != nil {
		return err
	}
	return s.reconcileUsers(ctx, users)
}

// reconcileUsers always repairs configuration from the supplied desired state.
// A streamed snapshot must use this path rather than a hash-skippable regular
// SyncUsers call: a static artifact can have changed the local template while
// the database user list itself is unchanged.
func (s *Service) reconcileUsers(ctx context.Context, users []domain.VPNUserConfig) error {
	if reconciler, ok := s.engine.(domain.DriftReconciler); ok {
		_, err := reconciler.ReconcileUsers(ctx, users)
		if err != nil {
			return err
		}
	} else {
		// Engines without an explicit drift-repair capability still receive the
		// snapshot through their normal synchronisation interface.
		if _, err := s.engine.SyncUsers(ctx, users, true); err != nil {
			return err
		}
	}
	return nil
}
