package slave

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"xraytool/internal/domain"
	"xraytool/internal/vpn"
)

type stateSyncProvider struct {
	registry        *Registry
	engine          domain.Engine
	domainReg       domain.Registry
	realityRotation bool
	realityKeysPath string
}

func NewStateSyncProvider(registry *Registry, engine domain.Engine, domainReg domain.Registry, realityRotation bool, realityKeysPath string) domain.StateSyncSlaveProvider {
	return &stateSyncProvider{
		registry:        registry,
		engine:          engine,
		domainReg:       domainReg,
		realityRotation: realityRotation,
		realityKeysPath: realityKeysPath,
	}
}

func (p *stateSyncProvider) SyncAllSlaves(ctx context.Context, dryRun bool) ([]domain.SyncResult, error) {
	// Propagate Reality keys first if rotation is enabled
	if p.realityRotation && p.realityKeysPath != "" && !dryRun {
		if keysBytes, err := os.ReadFile(p.realityKeysPath); err == nil {
			p.registry.PropagateAll("sync-keys", map[string]string{
				"payload": string(keysBytes),
			})
		}
	}

	subs, err := p.domainReg.Subscriptions().FindAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to load subscriptions: %w", err)
	}

	blockedMap := p.getBlockedEmails(ctx)

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

	servers, err := p.registry.Servers()
	if err != nil || len(servers) == 0 {
		return nil, fmt.Errorf("no slave servers configured")
	}

	payloadBytes, err := json.Marshal(dbUsers)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal users: %w", err)
	}

	results := make([]domain.SyncResult, len(servers))
	var wg sync.WaitGroup
	var mu sync.Mutex

	i := 0
	for srvName := range servers {
		wg.Add(1)
		go func(name string, index int) {
			defer wg.Done()
			var syncErr error
			if !dryRun {
				_, syncErr = p.registry.CallOne(name, "sync-users", map[string]string{
					"payload": string(payloadBytes),
				})
			}
			mu.Lock()
			results[index] = domain.SyncResult{
				ServerName: name,
				Success:    syncErr == nil,
				Error:      syncErr,
			}
			mu.Unlock()
		}(srvName, i)
		i++
	}
	wg.Wait()

	return results, nil
}

func (p *stateSyncProvider) getBlockedEmails(ctx context.Context) map[string]bool {
	blockedMap := make(map[string]bool)
	if p.domainReg == nil {
		return blockedMap
	}

	// Blocked users (admin block)
	users, err := p.domainReg.Users().FindAll(ctx)
	if err == nil {
		blockedUserIDs := make(map[string]bool)
		for _, u := range users {
			if u.IsBlocked {
				blockedUserIDs[u.ID] = true
			}
		}

		if len(blockedUserIDs) > 0 {
			subs, err := p.domainReg.Subscriptions().FindAll(ctx)
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
	bans, err := p.domainReg.AntifraudBans().FindActive(ctx)
	if err == nil {
		for _, b := range bans {
			blockedMap[b.Email] = true
		}
	}

	return blockedMap
}
