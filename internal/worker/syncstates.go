package worker

import (
	"context"
	"log/slog"
	"time"

	"xraytool/internal/statesync"
)

// SyncStatesWorker periodically synchronizes state from master to all slaves.
type SyncStatesWorker struct {
	syncSvc  *statesync.Service
	interval time.Duration
	log      *slog.Logger
}

// NewSyncStatesWorker creates a new SyncStatesWorker.
func NewSyncStatesWorker(syncSvc *statesync.Service, interval time.Duration, log *slog.Logger) *SyncStatesWorker {
	return &SyncStatesWorker{
		syncSvc:  syncSvc,
		interval: interval,
		log:      log.With("component", "syncstates_worker"),
	}
}

// Run starts the background ticker. It blocks until ctx is canceled.
func (w *SyncStatesWorker) Run(ctx context.Context) {
	w.log.Info("Starting syncstates worker", "interval", w.interval)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	// Run once on startup
	w.sync(ctx)

	for {
		select {
		case <-ctx.Done():
			w.log.Info("Stopping syncstates worker")
			return
		case <-ticker.C:
			w.sync(ctx)
		}
	}
}

func (w *SyncStatesWorker) sync(ctx context.Context) {
	w.log.Info("Periodic state sync started")
	
	// Self-heal Master UUIDs before syncing
	changed, err := w.syncSvc.SelfHealMasterUUIDs(ctx)
	if err != nil {
		w.log.Error("Self-healing master UUIDs failed", "err", err)
	} else if changed {
		w.log.Info("Self-healing master UUIDs completed successfully")
	}

	results, err := w.syncSvc.SyncAllSlaves(ctx, false)
	if err != nil {
		w.log.Error("Failed to sync states to slaves", "err", err)
		return
	}

	for _, res := range results {
		if res.Success {
			w.log.Info("Slave synchronized successfully", "server", res.ServerName)
		} else {
			w.log.Error("Slave synchronization failed", "server", res.ServerName, "err", res.Error)
		}
	}
	w.log.Info("Periodic state sync completed")
}
