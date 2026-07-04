package domain

import (
	"context"
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
