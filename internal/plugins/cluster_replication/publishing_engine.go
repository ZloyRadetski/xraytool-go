package clusterreplication

import (
	"context"
	"fmt"

	"xraytool/internal/domain"
)

// PublishingEngine is installed only on the master. It preserves the normal
// Engine contract while writing compact, durable outbox records after a local
// engine operation succeeds. The periodic desired-state detector is the safety
// net for business mutations that bypass the Engine interface.
type PublishingEngine struct {
	domain.Engine
	service *Service
}

func NewPublishingEngine(engine domain.Engine, service *Service) *PublishingEngine {
	return &PublishingEngine{Engine: engine, service: service}
}

func (e *PublishingEngine) AddUser(ctx context.Context, user domain.VPNUserConfig) error {
	if err := e.Engine.AddUser(ctx, user); err != nil {
		return err
	}
	if _, err := e.service.PublishUser(ctx, user); err != nil {
		return fmt.Errorf("record replicated add for %q: %w", user.Email, err)
	}
	return nil
}

func (e *PublishingEngine) AddUsersBulk(ctx context.Context, users []domain.VPNUserConfig) error {
	if err := e.Engine.AddUsersBulk(ctx, users); err != nil {
		return err
	}
	for _, user := range users {
		if _, err := e.service.PublishUser(ctx, user); err != nil {
			return fmt.Errorf("record replicated bulk add for %q: %w", user.Email, err)
		}
	}
	return nil
}

func (e *PublishingEngine) RemoveUser(ctx context.Context, email string) error {
	if err := e.Engine.RemoveUser(ctx, email); err != nil {
		return err
	}
	if _, err := e.service.PublishRemove(ctx, email); err != nil {
		return fmt.Errorf("record replicated removal for %q: %w", email, err)
	}
	return nil
}

func (e *PublishingEngine) RemoveUsersBulk(ctx context.Context, emails []string) error {
	if err := e.Engine.RemoveUsersBulk(ctx, emails); err != nil {
		return err
	}
	for _, email := range emails {
		if _, err := e.service.PublishRemove(ctx, email); err != nil {
			return fmt.Errorf("record replicated bulk removal for %q: %w", email, err)
		}
	}
	return nil
}

func (e *PublishingEngine) SetExpire(ctx context.Context, email string, expire string) error {
	if err := e.Engine.SetExpire(ctx, email, expire); err != nil {
		return err
	}
	return e.service.PublishCurrentUser(ctx, email)
}

func (e *PublishingEngine) SetLimit(ctx context.Context, email string, limit float64) error {
	if err := e.Engine.SetLimit(ctx, email, limit); err != nil {
		return err
	}
	return e.service.PublishCurrentUser(ctx, email)
}

func (e *PublishingEngine) BanUser(ctx context.Context, email string) error {
	if err := e.Engine.BanUser(ctx, email); err != nil {
		return err
	}
	return e.service.PublishCurrentUser(ctx, email)
}

func (e *PublishingEngine) UnbanUser(ctx context.Context, email string) error {
	if err := e.Engine.UnbanUser(ctx, email); err != nil {
		return err
	}
	return e.service.PublishCurrentUser(ctx, email)
}

var _ domain.Engine = (*PublishingEngine)(nil)
