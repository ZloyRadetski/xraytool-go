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

	blockedMap := s.getBlockedEmails(ctx)

	updatedCount := 0
	for _, sub := range subs {
		if sub.Email == "" || sub.XrayUUID == "" {
			continue
		}

		u, exists := userMap[sub.Email]
		isActive := sub.Status == "active" && !blockedMap[sub.Email]

		if isActive {
			expectedUser := vpn.SubscriptionToVPNUserConfig(sub)
			if !exists {
				// Add active user missing from Engine
				if err := s.engine.AddUser(ctx, expectedUser); err == nil {
					updatedCount++
				}
			} else if u.UUID != expectedUser.UUID || u.Auth != expectedUser.Auth || u.Expire != expectedUser.Expire || u.Subfile != expectedUser.Subfile || u.MaxDevices != expectedUser.MaxDevices {
				// Update mismatched user in Engine
				s.engine.RemoveUser(ctx, sub.Email)
				if err := s.engine.AddUser(ctx, expectedUser); err == nil {
					updatedCount++
				}
			}
		} else {
			if exists {
				// Remove inactive/blocked user from Engine
				if err := s.engine.RemoveUser(ctx, sub.Email); err == nil {
					updatedCount++
				}
			}
		}
	}

	return updatedCount > 0, nil
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



