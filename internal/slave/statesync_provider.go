package slave

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"xraytool/internal/domain"
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

	masterSnap := BuildMasterSnapshot(ctx, p.domainReg, p.engine)

	servers, err := p.registry.Servers()
	if err != nil || len(servers) == 0 {
		return nil, fmt.Errorf("no slave servers configured")
	}

	results := make([]domain.SyncResult, len(servers))
	var wg sync.WaitGroup
	var mu sync.Mutex

	i := 0
	for srvName := range servers {
		wg.Add(1)
		go func(name string, index int) {
			defer wg.Done()
			err := p.syncSlave(ctx, p.registry, name, masterSnap, dryRun)
			mu.Lock()
			results[index] = domain.SyncResult{
				ServerName: name,
				Success:    err == nil,
				Error:      err,
			}
			mu.Unlock()
		}(srvName, i)
		i++
	}
	wg.Wait()

	return results, nil
}

func (p *stateSyncProvider) syncSlave(ctx context.Context, reg *Registry, srvName string, master Snapshot, dryRun bool) error {
	var slaveSnap Snapshot

	err := reg.CallOneDecode(srvName, "usersnapshot", map[string]string{}, &slaveSnap)
	if err != nil {
		return fmt.Errorf("could not get snapshot: %w", err)
	}

	masterActive := make(map[string]SnapshotUser, len(master.Active))
	for _, mu := range master.Active {
		masterActive[mu.Email] = mu
	}
	slaveActive := make(map[string]SnapshotUser, len(slaveSnap.Active))
	for _, u := range slaveSnap.Active {
		slaveActive[u.Email] = u
	}

		batch := struct {
		Add    []SnapshotUser `json:"add"`
		Remove []string       `json:"remove"`
	}{
		Add:    []SnapshotUser{},
		Remove: []string{},
	}

	for _, mu := range master.Active {
		su, existsActive := slaveActive[mu.Email]
		if !existsActive {
			batch.Add = append(batch.Add, mu)
		} else if su.UUID != mu.UUID || su.Auth != mu.Auth || su.Expire != mu.Expire || su.Subfile != mu.Subfile || !compareLimits(su.Limit, mu.Limit) {
			batch.Add = append(batch.Add, mu)
		}
	}

	for _, su := range slaveSnap.Active {
		if _, exists := masterActive[su.Email]; !exists {
			batch.Remove = append(batch.Remove, su.Email)
		}
	}

	if len(batch.Add) == 0 && len(batch.Remove) == 0 {
		return nil
	}

	if dryRun {
		return nil
	}

	payloadBytes, err := json.Marshal(batch)
	if err != nil {
		return fmt.Errorf("failed to marshal batch: %w", err)
	}

	_, err = reg.CallOne(srvName, "apply-batch", map[string]string{
		"payload": string(payloadBytes),
	})
	if err != nil {
		return fmt.Errorf("failed to apply batch: %w", err)
	}
	return nil
}

func compareLimits(a, b *float64) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	if a != nil && *a != *b {
		return false
	}
	return true
}
