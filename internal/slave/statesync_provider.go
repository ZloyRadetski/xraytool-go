package slave

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"sync"

	"xraytool/internal/domain"
	"xraytool/internal/statesync"
)

// stateSyncProvider implements domain.StateSyncSlaveProvider.
// It orchestrates the three-phase sync protocol:
//
//  1. Ping — check if slave state_hash matches master.
//  2. Delta — send ordered list of events since slave's last_event_id.
//  3. Full-sync — trigger paginated snapshot pull when delta is unavailable.
type stateSyncProvider struct {
	registry        *Registry
	syncSvc         *statesync.Service
	realityRotation bool
	realityKeysPath string
	log             *slog.Logger
}

func NewStateSyncProvider(
	registry *Registry,
	syncSvc *statesync.Service,
	realityRotation bool,
	realityKeysPath string,
	log *slog.Logger,
) domain.StateSyncSlaveProvider {
	if log == nil {
		log = slog.Default()
	}
	return &stateSyncProvider{
		registry:        registry,
		syncSvc:         syncSvc,
		realityRotation: realityRotation,
		realityKeysPath: realityKeysPath,
		log:             log.With("component", "statesync-provider"),
	}
}

func (p *stateSyncProvider) SyncAllSlaves(ctx context.Context, dryRun bool) ([]domain.SyncResult, error) {
	// 0. Propagate Reality keys first if rotation is enabled.
	if p.realityRotation && p.realityKeysPath != "" && !dryRun {
		if keysBytes, err := os.ReadFile(p.realityKeysPath); err == nil {
			p.registry.PropagateAll("sync-keys", map[string]string{
				"payload": string(keysBytes),
			})
		}
	}

	// 1. Get master's current state.
	masterState, err := p.syncSvc.MasterState(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get master state: %w", err)
	}

	servers, err := p.registry.Servers()
	if err != nil || len(servers) == 0 {
		return nil, fmt.Errorf("no slave servers configured")
	}

	results := make([]domain.SyncResult, 0, len(servers))
	var mu sync.Mutex
	var wg sync.WaitGroup

	for srvName, entry := range servers {
		srvName, entry := srvName, entry
		wg.Add(1)
		go func() {
			defer wg.Done()

			var syncErr error
			if !dryRun {
				syncErr = p.syncOneSlave(ctx, srvName, entry, masterState)
			}

			mu.Lock()
			results = append(results, domain.SyncResult{
				ServerName: srvName,
				Success:    syncErr == nil,
				Error:      syncErr,
			})
			mu.Unlock()
		}()
	}
	wg.Wait()

	return results, nil
}

// syncOneSlave runs the full 3-phase protocol for a single slave.
func (p *stateSyncProvider) syncOneSlave(
	ctx context.Context,
	name string,
	entry Entry,
	masterState domain.SyncState,
) error {
	log := p.log.With("slave", name)

	// ── Phase 1: Ping ────────────────────────────────────────────────────────
	var checkResult domain.SyncCheckResult
	err := p.registry.client.CallDecode(entry, "sync-ping", map[string]string{
		"payload": strconv.FormatInt(masterState.LastEventID, 10),
		"auth":    masterState.StateHash,
	}, &checkResult)
	if err != nil {
		return fmt.Errorf("ping failed: %w", err)
	}

	if checkResult.Match {
		// 99% case: slave is in sync.
		return nil
	}

	// Slave is out of sync. Decide: delta or full-sync?
	slaveID := checkResult.LastEventID

	// Edge case: same event_id but different hash → direct data corruption on slave.
	// Delta cannot fix this — enforce full-sync.
	if slaveID == masterState.LastEventID {
		log.Warn("statesync: slave has same event_id but different hash — forcing full-sync",
			"event_id", slaveID, "slave_hash", "?", "master_hash", masterState.StateHash)
		return p.triggerFullSync(ctx, name, entry, masterState)
	}

	// ── Phase 2: Try delta ────────────────────────────────────────────────────
	delta, err := p.syncSvc.BuildDelta(ctx, slaveID)
	if err != nil {
		return fmt.Errorf("build delta: %w", err)
	}

	if delta != nil {
		log.Info("statesync: sending delta to slave",
			"from_event_id", slaveID, "events", len(delta))
		return p.sendDelta(entry, delta, masterState)
	}

	// ── Phase 3: Full-sync fallback ───────────────────────────────────────────
	log.Warn("statesync: delta unavailable — falling back to full-sync",
		"slave_event_id", slaveID, "master_event_id", masterState.LastEventID)
	return p.triggerFullSync(ctx, name, entry, masterState)
}

// sendDelta transmits an ordered list of events to the slave.
func (p *stateSyncProvider) sendDelta(entry Entry, delta []domain.SyncDeltaEvent, masterState domain.SyncState) error {
	eventsJSON, err := json.Marshal(delta)
	if err != nil {
		return fmt.Errorf("marshal delta: %w", err)
	}
	_, err = p.registry.client.Call(entry, "sync-delta", map[string]string{
		"payload": string(eventsJSON),
		"uuid":    strconv.FormatInt(masterState.LastEventID, 10),
		"auth":    masterState.StateHash,
	})
	return err
}

// triggerFullSync tells the slave to start pulling a paginated snapshot.
func (p *stateSyncProvider) triggerFullSync(
	ctx context.Context,
	name string,
	entry Entry,
	masterState domain.SyncState,
) error {
	_, err := p.registry.client.Call(entry, "sync-full-trigger", map[string]string{
		"payload": strconv.FormatInt(masterState.LastEventID, 10),
		"auth":    masterState.StateHash,
	})
	return err
}
