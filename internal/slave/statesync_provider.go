package slave

import (
	"context"
	"fmt"
	"sync"

	"xraytool/internal/domain"
)

type stateSyncProvider struct {
	registry *Registry
	engine   domain.Engine
	domainReg domain.Registry
}

func NewStateSyncProvider(registry *Registry, engine domain.Engine, domainReg domain.Registry) domain.StateSyncSlaveProvider {
	return &stateSyncProvider{
		registry:  registry,
		engine:    engine,
		domainReg: domainReg,
	}
}

func (p *stateSyncProvider) SyncAllSlaves(ctx context.Context, dryRun bool) ([]domain.SyncResult, error) {
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
		} else if su.UUID != mu.UUID {
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

	err = reg.CallOneDecode(srvName, "applybatch", map[string]string{}, &batch)
	if err != nil {
		return fmt.Errorf("failed to apply batch: %w", err)
	}
	return nil
}
