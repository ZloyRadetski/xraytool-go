package statesync

import (
	"context"
	"fmt"

		"xraytool/internal/domain"
	)

type Service struct {
	registry      domain.Registry
	engine        domain.Engine
	slaveProvider domain.StateSyncSlaveProvider
}

func NewService(registry domain.Registry, engine domain.Engine, slaveProvider domain.StateSyncSlaveProvider) *Service {
	return &Service{registry: registry, engine: engine, slaveProvider: slaveProvider}
}

func (s *Service) SyncAllSlaves(ctx context.Context, dryRun bool) ([]domain.SyncResult, error) {
	if s.slaveProvider == nil {
		return nil, fmt.Errorf("syncstates can only run on master node")
	}
	return s.slaveProvider.SyncAllSlaves(ctx, dryRun)
}

func (s *Service) SelfHealMasterUUIDs(ctx context.Context) (bool, error) {
	subs, err := s.registry.Subscriptions().FindAll(ctx)
	if err != nil {
		return false, fmt.Errorf("failed to load subscriptions: %w", err)
	}

	users, err := s.engine.ListUsers(ctx)
	if err != nil {
		return false, fmt.Errorf("failed to list users: %w", err)
	}

	userMap := make(map[string]domain.VPNUserConfig)
	for _, u := range users {
		userMap[u.Email] = u
	}

	updatedCount := 0
	for _, sub := range subs {
		if sub.Email == "" || sub.XrayUUID == "" {
			continue
		}

		u, exists := userMap[sub.Email]
		if !exists {
			continue
		}

		if u.UUID != sub.XrayUUID {
			// Engine doesn't have an UpdateUUID, so we remove and re-add.
			s.engine.RemoveUser(ctx, sub.Email)
			u.UUID = sub.XrayUUID
			s.engine.AddUser(ctx, u)
			updatedCount++
		}
	}

	if updatedCount > 0 {
		return true, nil
	}
	return false, nil
}



