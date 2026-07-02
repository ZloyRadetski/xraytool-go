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

// UserMutator handles the lifecycle of users in the VPN engine.
type UserMutator interface {
	AddUser(ctx context.Context, user VPNUserConfig) error
	RemoveUser(ctx context.Context, email string) error
	RemoveUsersBulk(ctx context.Context, emails []string) error
	SetExpire(ctx context.Context, email string, expire string) error
	SetLimit(ctx context.Context, email string, limit float64) error
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

// Engine combines all the granular interfaces.
type Engine interface {
	UserMutator
	TrafficReader
	SoftBanner
	LoggerController
	StateSyncer
}


// BatchPayload represents a domain instruction to add/remove users.
type BatchPayload struct {
	Add    []VPNUserConfig
	Remove []string
}
