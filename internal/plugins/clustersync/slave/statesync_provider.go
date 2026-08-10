package slave

import (
	"context"
	"fmt"
	json "github.com/goccy/go-json"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"

	"xraytool/internal/domain"
	"xraytool/internal/plugins/clustersync/statesync"
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

func (p *stateSyncProvider) SyncAllSlaves(ctx context.Context, dryRun bool, forceFull bool) ([]domain.SyncResult, error) {
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

	// Static/template clients are not represented by the subscription event
	// log. Build their snapshot once and send it to every slave before the
	// regular ping/delta/full protocol. The slave-side operation is idempotent,
	// so this adds no Xray rebuild when the template has not changed.
	staticClients, staticSupported, err := p.syncSvc.BuildStaticClientSnapshot(ctx)
	if err != nil {
		return nil, fmt.Errorf("build static client snapshot: %w", err)
	}
	var staticPayload string
	if staticSupported && !dryRun {
		encoded, err := json.Marshal(staticClients)
		if err != nil {
			return nil, fmt.Errorf("marshal static client snapshot: %w", err)
		}
		staticPayload = string(encoded)
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
				syncErr = p.syncOneSlave(ctx, srvName, entry, masterState, forceFull, staticSupported, staticPayload)
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
	forceFull bool,
	staticSupported bool,
	staticPayload string,
) error {
	log := p.log.With("slave", name)

	if staticSupported {
		if err := p.syncStaticClients(entry, staticPayload); err != nil {
			return fmt.Errorf("sync static clients: %w", err)
		}
	}

	if forceFull {
		log.Info("statesync: force full sync requested")
		return p.triggerFullSync(ctx, name, entry, masterState)
	}

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

// syncStaticClients sends the master template's config-only clients. It runs
// before every state check because static entries have no DB event and must be
// reconciled even when both nodes have the same event cursor.
func (p *stateSyncProvider) syncStaticClients(entry Entry, payload string) error {
	_, err := p.registry.client.Call(entry, "sync-static-clients", map[string]string{
		"payload": payload,
	})
	// During a rolling upgrade an older slave does not know this action yet.
	// Keep the existing user delta/full-sync path working; once that slave is
	// updated, its static clients are picked up on the very next interval.
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "unknown action") {
		p.log.Warn("statesync: slave does not support static client sync yet")
		return nil
	}
	return err
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
