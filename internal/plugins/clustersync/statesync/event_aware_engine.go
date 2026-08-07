package statesync

import (
	"context"
	"fmt"

	"xraytool/internal/domain"
)

// EventAwareEngine wraps an existing domain.Engine and automatically appends
// synchronization events (add/update/remove) to the database log whenever the engine is mutated.
// Because these methods receive the context containing the active GORM transaction,
// the sync events are atomically committed alongside the business data.
type EventAwareEngine struct {
	domain.Engine
	syncSvc *Service
}

// NewEventAwareEngine creates a new event-aware wrapper around the given engine.
func NewEventAwareEngine(engine domain.Engine, syncSvc *Service) domain.Engine {
	return &EventAwareEngine{
		Engine:  engine,
		syncSvc: syncSvc,
	}
}

func (e *EventAwareEngine) AddUser(ctx context.Context, user domain.VPNUserConfig) error {
	err := e.Engine.AddUser(ctx, user)
	if err == nil {
		// Map hot-add to SyncActionUpdate (which serves as an upsert on the slave)
		if _, appendErr := e.syncSvc.AppendEvent(ctx, domain.SyncActionUpdate, user); appendErr != nil {
			return fmt.Errorf("failed to log sync event: %w", appendErr)
		}
	}
	return err
}

func (e *EventAwareEngine) AddUsersBulk(ctx context.Context, users []domain.VPNUserConfig) error {
	err := e.Engine.AddUsersBulk(ctx, users)
	if err == nil {
		for _, u := range users {
			if _, appendErr := e.syncSvc.AppendEvent(ctx, domain.SyncActionUpdate, u); appendErr != nil {
				return fmt.Errorf("failed to log bulk sync event: %w", appendErr)
			}
		}
	}
	return err
}

func (e *EventAwareEngine) RemoveUser(ctx context.Context, email string) error {
	err := e.Engine.RemoveUser(ctx, email)
	if err == nil {
		if _, appendErr := e.syncSvc.AppendRemoveEvent(ctx, email); appendErr != nil {
			return fmt.Errorf("failed to log sync event: %w", appendErr)
		}
	}
	return err
}

func (e *EventAwareEngine) RemoveUsersBulk(ctx context.Context, emails []string) error {
	err := e.Engine.RemoveUsersBulk(ctx, emails)
	if err == nil {
		for _, email := range emails {
			if _, appendErr := e.syncSvc.AppendRemoveEvent(ctx, email); appendErr != nil {
				return fmt.Errorf("failed to log bulk sync event: %w", appendErr)
			}
		}
	}
	return err
}

// SetExpire updates local config, so we must log it for slaves.
func (e *EventAwareEngine) SetExpire(ctx context.Context, email string, expire string) error {
	err := e.Engine.SetExpire(ctx, email, expire)
	if err == nil {
		if appendErr := e.logUpdateForEmail(ctx, email); appendErr != nil {
			return fmt.Errorf("failed to log SetExpire event: %w", appendErr)
		}
	}
	return err
}

// SetLimit updates local config, so we must log it for slaves.
func (e *EventAwareEngine) SetLimit(ctx context.Context, email string, limit float64) error {
	err := e.Engine.SetLimit(ctx, email, limit)
	if err == nil {
		if appendErr := e.logUpdateForEmail(ctx, email); appendErr != nil {
			return fmt.Errorf("failed to log SetLimit event: %w", appendErr)
		}
	}
	return err
}

// logUpdateForEmail reconstructs the full user config and logs a SyncActionUpdate.
func (e *EventAwareEngine) logUpdateForEmail(ctx context.Context, email string) error {
	// Rebuild snapshot logic just for this one user
	snapshot, err := e.syncSvc.BuildSnapshot(ctx)
	if err != nil {
		return err
	}
	for _, u := range snapshot {
		if u.Email == email {
			_, err := e.syncSvc.AppendEvent(ctx, domain.SyncActionUpdate, u)
			return err
		}
	}
	return nil // If user is not in snapshot (e.g., deleted), no update needed.
}
