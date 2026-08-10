package domain

import (
	"context"

	json "github.com/goccy/go-json"
)

// VPNUserConfig is the domain-agnostic representation of a VPN user for the engine.
type VPNUserConfig struct {
	Email      string
	UUID       string
	Auth       string
	Subfile    string
	Expire     string
	MaxDevices int
	Flow       string
	Cipher     string
	// PlanEngineIDs is the engine set selected by the subscription plan. It
	// is consumed only when MultiEngine runs in by-plan mode. Keeping the
	// routing hint beside the VPN user makes state-sync events self-contained.
	PlanEngineIDs []string
	// SubscriptionEngineIDs is an administrator override from
	// Subscription.Metadata["engine_ids"]. It takes precedence in
	// by-subscription-override mode.
	SubscriptionEngineIDs []string
}

// TrafficStat represents the abstract traffic statistics for a single user.
type TrafficStat struct {
	Email string
	Up    int64
	Down  int64
}

// EngineSyncResult reports what SyncUsers did.
type EngineSyncResult struct {
	Added   int // users hot-added via gRPC
	Removed int // users hot-removed via gRPC
}

// UserMutator handles the lifecycle of users in the VPN engine.
type UserMutator interface {
	AddUser(ctx context.Context, user VPNUserConfig) error
	AddUsersBulk(ctx context.Context, users []VPNUserConfig) error
	RemoveUser(ctx context.Context, email string) error
	RemoveUsersBulk(ctx context.Context, emails []string) error
	SetExpire(ctx context.Context, email string, expire string) error
	SetLimit(ctx context.Context, email string, limit float64) error

	// RebuildInbound hot-rebuilds a single inbound by its tag.
	// This is needed for protocols like hysteria2 that do not support
	// per-user hot-add/hot-remove — the entire inbound is removed and
	// re-added with the updated client list.
	RebuildInbound(ctx context.Context, tag string) error
}

// TrafficReader handles reading metrics.
type TrafficReader interface {
	QueryStats(ctx context.Context) ([]TrafficStat, error)
}

// SoftBanner handles banning and unbanning.
type SoftBanner interface {
	BanUser(ctx context.Context, email string) error
	UnbanUser(ctx context.Context, email string) error
}

// LoggerController manages the engine logs.
type LoggerController interface {
	RestartLogger(ctx context.Context) error
}

// StateSyncer lists users.
type StateSyncer interface {
	ListUsers(ctx context.Context) ([]VPNUserConfig, error)
}

// ConfigRegenerator regenerates the xray config from a template and DB users,
// then syncs the running xray process via hot-add/hot-remove.
type ConfigRegenerator interface {
	// SyncUsers regenerates config.json from template + dbUsers, then diffs
	// against the running xray process:
	//   - Users in dbUsers but not in xray → hot-added via gRPC.
	//   - Users in xray but not in dbUsers → hot-removed (if removeOrphans).
	// The config file is completely regenerated before the diff.
	SyncUsers(ctx context.Context, dbUsers []VPNUserConfig, removeOrphans bool) (*EngineSyncResult, error)
}

// StaticInboundClients is an opaque, field-preserving static client list for
// one inbound. It is deliberately kept separate from VPNUserConfig: clients
// written directly into an engine template may contain protocol-specific
// fields that xraytool does not own and must not discard during replication.
type StaticInboundClients struct {
	InboundTag string          `json:"inbound_tag"`
	Protocol   string          `json:"protocol"`
	Clients    json.RawMessage `json:"clients"`
}

// StaticClientSynchronizer is an optional engine capability used by cluster
// synchronisation. Engines that support static/template clients can export
// and apply their exact JSON while ordinary database-managed users continue
// through ConfigRegenerator.
//
// managedUsers identifies the current database-owned users, so a direct
// config source can exclude them from the static snapshot.
type StaticClientSynchronizer interface {
	StaticClientSnapshot(ctx context.Context, managedUsers []VPNUserConfig) ([]StaticInboundClients, error)
	ApplyStaticClientSnapshot(ctx context.Context, inbounds []StaticInboundClients) error
}

// DriftReconciler restores the managed part of an engine configuration from a
// known desired state. It is intentionally separate from Engine: replication
// uses it on slave nodes to repair manual edits without making every caller pay
// for a forced configuration rewrite.
type DriftReconciler interface {
	ReconcileUsers(ctx context.Context, users []VPNUserConfig) (*EngineSyncResult, error)
}

// Engine combines all the granular interfaces.
type Engine interface {
	UserMutator
	TrafficReader
	SoftBanner
	LoggerController
	StateSyncer
	ConfigRegenerator
}

// BatchPayload represents a domain instruction to add/remove users.
type BatchPayload struct {
	Add    []VPNUserConfig
	Remove []string
}
