package vpn

import (
	"context"

	"xraytool/internal/domain"
)

// NoopEngine is an Engine implementation that does nothing.
// Useful for batch jobs that don't need to touch the VPN daemon,
// or for testing.
type NoopEngine struct{}

func (n *NoopEngine) AddUser(ctx context.Context, user domain.VPNUserConfig) error               { return nil }
func (n *NoopEngine) RemoveUser(ctx context.Context, email string) error               { return nil }
func (n *NoopEngine) RemoveUsersBulk(ctx context.Context, emails []string) error       { return nil }
func (n *NoopEngine) SetExpire(ctx context.Context, email string, expire string) error { return nil }
func (n *NoopEngine) SetLimit(ctx context.Context, email string, limit float64) error  { return nil }

func (n *NoopEngine) QueryStats(ctx context.Context) ([]domain.TrafficStat, error) {
	return nil, nil
}

func (n *NoopEngine) BanUser(ctx context.Context, email string) error   { return nil }
func (n *NoopEngine) UnbanUser(ctx context.Context, email string) error { return nil }

func (n *NoopEngine) RestartLogger(ctx context.Context) error { return nil }

func (n *NoopEngine) ListUsers(ctx context.Context) ([]domain.VPNUserConfig, error) {
	return nil, nil
}
