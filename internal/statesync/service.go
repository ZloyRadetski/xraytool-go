package statesync

import (
	"context"
	"fmt"
	"sync"

	"xraytool/internal/domain"
	"xraytool/internal/vpn"
)

type Service struct {
	registry      domain.Registry
	engine        domain.Engine
	slaveProvider domain.StateSyncSlaveProvider
	syncMu        sync.Mutex
}

func NewService(registry domain.Registry, engine domain.Engine, slaveProvider domain.StateSyncSlaveProvider) *Service {
	return &Service{registry: registry, engine: engine, slaveProvider: slaveProvider}
}

func (s *Service) SyncAllSlaves(ctx context.Context, dryRun bool) ([]domain.SyncResult, error) {
	if s.slaveProvider == nil {
		return nil, fmt.Errorf("syncstates can only run on master node")
	}
	if !s.syncMu.TryLock() {
		return nil, nil // Skip if another synchronization is already in progress
	}
	defer s.syncMu.Unlock()

	return s.slaveProvider.SyncAllSlaves(ctx, dryRun)
}

func (s *Service) SelfHealMasterUUIDs(ctx context.Context) (bool, error) {
	if !s.syncMu.TryLock() {
		return false, nil // Skip if another synchronization is already in progress
	}
	defer s.syncMu.Unlock()

	subs, err := s.registry.Subscriptions().FindAll(ctx)
	if err != nil {
		return false, fmt.Errorf("failed to load subscriptions: %w", err)
	}

	blockedMap := s.getBlockedEmails(ctx)

	dbUsers := make([]domain.VPNUserConfig, 0, len(subs))
	for _, sub := range subs {
		if sub.Email == "" || sub.XrayUUID == "" {
			continue
		}
		if sub.Status != "active" || blockedMap[sub.Email] {
			continue
		}
		dbUsers = append(dbUsers, vpn.SubscriptionToVPNUserConfig(sub))
	}

	result, err := s.engine.SyncUsers(ctx, dbUsers, true)
	if err != nil {
		return false, fmt.Errorf("failed to sync master users: %w", err)
	}

	changed := (result.Added > 0 || result.Removed > 0)
	return changed, nil
}

func (s *Service) getBlockedEmails(ctx context.Context) map[string]bool {
	blockedMap := make(map[string]bool)
	if s.registry == nil {
		return blockedMap
	}

	// Blocked users (admin block)
	users, err := s.registry.Users().FindAll(ctx)
	if err == nil {
		blockedUserIDs := make(map[string]bool)
		for _, u := range users {
			if u.IsBlocked {
				blockedUserIDs[u.ID] = true
			}
		}

		if len(blockedUserIDs) > 0 {
			subs, err := s.registry.Subscriptions().FindAll(ctx)
			if err == nil {
				for _, sub := range subs {
					if blockedUserIDs[sub.UserID] && sub.Email != "" {
						blockedMap[sub.Email] = true
					}
				}
			}
		}
	}

	// Active antifraud bans
	bans, err := s.registry.AntifraudBans().FindActive(ctx)
	if err == nil {
		for _, b := range bans {
			blockedMap[b.Email] = true
		}
	}

	return blockedMap
}



