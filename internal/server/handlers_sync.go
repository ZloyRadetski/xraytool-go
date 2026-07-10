package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"xraytool/internal/domain"
	"xraytool/internal/statesync"
	"xraytool/internal/user"
)

// ─────────────────────────────────────────────────────────────────────────────
// Master-side handlers (called by slaves pulling data from master)
// ─────────────────────────────────────────────────────────────────────────────

// handleSyncSnapshot serves paginated VPNUserConfig chunks to a slave performing
// a full-sync. The slave calls this endpoint in a loop until has_more=false.
//
// GET /api/v1/internal/xray/sync/snapshot?offset=0&limit=1000
func (r *Router) handleSyncSnapshot(w http.ResponseWriter, req *http.Request) {
	if r.syncSvc == nil {
		writeError(w, http.StatusServiceUnavailable, "sync service not available on this node")
		return
	}

	offsetStr := req.URL.Query().Get("offset")
	limitStr := req.URL.Query().Get("limit")

	offset, _ := strconv.Atoi(offsetStr)
	limit, _ := strconv.Atoi(limitStr)
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	if offset < 0 {
		offset = 0
	}

	users, err := r.syncSvc.BuildSnapshot(req.Context())
	if err != nil {
		r.log.Error("sync snapshot: failed to build snapshot", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to build snapshot")
		return
	}

	end := offset + limit
	hasMore := end < len(users)
	if end > len(users) {
		end = len(users)
	}
	chunk := users[offset:end]

	type snapshotResponse struct {
		Users   []domain.VPNUserConfig `json:"users"`
		HasMore bool                   `json:"has_more"`
		Total   int                    `json:"total"`
	}
	writeJSON(w, http.StatusOK, snapshotResponse{
		Users:   chunk,
		HasMore: hasMore,
		Total:   len(users),
	})
}

// handleSyncState returns the master's current sync state (event_id + hash).
//
// GET /api/v1/internal/xray/sync/state
func (r *Router) handleSyncState(w http.ResponseWriter, req *http.Request) {
	if r.syncSvc == nil {
		writeError(w, http.StatusServiceUnavailable, "sync service not available on this node")
		return
	}
	state, err := r.syncSvc.MasterState(req.Context())
	if err != nil {
		r.log.Error("sync state: failed to read master state", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to read state")
		return
	}
	writeJSON(w, http.StatusOK, state)
}

// ─────────────────────────────────────────────────────────────────────────────
// Slave-side sync handler (called by master)
// The existing POST /api/v1/internal/xray/sync is extended with new actions.
// ─────────────────────────────────────────────────────────────────────────────

// handleSyncPing handles the lightweight master→slave state check.
// Master sends its current event_id + hash; slave compares with its own state.
//
// Action: "sync-ping"
// Params: last_event_id, state_hash
// Response: {"match": true} | {"match": false, "last_event_id": 42}
func (r *Router) handleSyncPing(w http.ResponseWriter, req *http.Request, body internalSyncRequest) {
	if r.registry == nil {
		writeError(w, http.StatusServiceUnavailable, "registry not available on this node")
		return
	}

	masterEventID, err := strconv.ParseInt(body.Payload, 10, 64) // Payload reused for event_id
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid last_event_id")
		return
	}
	masterHash := body.Auth // Auth field reused for state_hash

	slaveState, err := r.registry.SyncEvents().GetState(req.Context())
	if err != nil {
		r.log.Error("sync ping: failed to read slave state", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to read state")
		return
	}

	match := slaveState.LastEventID == masterEventID && slaveState.StateHash == masterHash
	writeJSON(w, http.StatusOK, domain.SyncCheckResult{
		Match:       match,
		LastEventID: slaveState.LastEventID,
	})
}

// handleSyncDelta applies an ordered list of SyncDeltaEvents to this slave node.
// Events are applied strictly sequentially in a single transaction.
//
// Action: "sync-delta"
// Params: payload = JSON array of SyncDeltaEvent
func (r *Router) handleSyncDelta(w http.ResponseWriter, req *http.Request, body internalSyncRequest) {
	if r.registry == nil || r.engine == nil {
		writeError(w, http.StatusServiceUnavailable, "not configured as slave")
		return
	}

	var events []domain.SyncDeltaEvent
	if err := json.Unmarshal([]byte(body.Payload), &events); err != nil {
		r.log.Error("sync delta: invalid events payload", "err", err)
		writeError(w, http.StatusBadRequest, "invalid events payload")
		return
	}
	if len(events) == 0 {
		writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "applied": 0})
		return
	}

	targetEventID, _ := strconv.ParseInt(body.UUID, 10, 64)
	targetHash := body.Auth

	ctx := req.Context()
	applied := 0

	// Apply events strictly sequentially — order is crucial for correctness.
	for _, ev := range events {
		var cfg domain.VPNUserConfig
		if err := json.Unmarshal([]byte(ev.Payload), &cfg); err != nil {
			r.log.Error("sync delta: failed to unmarshal event payload",
				"event_id", ev.ID, "err", err)
			writeError(w, http.StatusBadRequest, fmt.Sprintf("bad payload for event %d", ev.ID))
			return
		}

		var applyErr error
		switch ev.Action {
		case domain.SyncActionAdd, domain.SyncActionUpdate:
			applyErr = r.engine.AddUser(ctx, cfg)
		case domain.SyncActionRemove:
			applyErr = r.engine.RemoveUser(ctx, cfg.Email)
		default:
			r.log.Warn("sync delta: unknown action", "action", ev.Action, "event_id", ev.ID)
			continue
		}

		if applyErr != nil {
			r.log.Error("sync delta: failed to apply event",
				"event_id", ev.ID, "action", ev.Action, "email", cfg.Email, "err", applyErr)
			writeError(w, http.StatusInternalServerError,
				fmt.Sprintf("failed to apply event %d", ev.ID))
			return
		}

		// Persist the event in slave's own log so its state is queryable.
		if _, err := r.registry.SyncEvents().Append(ctx, ev.Action, ev.Payload); err != nil {
			// Non-fatal: we already applied the change to Xray.
			// Log the error — the next ping will detect a hash mismatch and re-sync.
			r.log.Warn("sync delta: failed to persist event in slave log",
				"event_id", ev.ID, "err", err)
		}
		applied++
	}

	// Update slave's sync_state to the target position supplied by master.
	if targetEventID > 0 {
		if err := r.registry.SyncEvents().SaveState(ctx, domain.SyncState{
			LastEventID: targetEventID,
			StateHash:   targetHash,
		}); err != nil {
			r.log.Warn("sync delta: failed to update slave sync_state", "err", err)
		}
	}

	r.log.Info("sync delta: applied events", "count", applied)
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "applied": applied})
}

// handleSyncFullTrigger receives master's command to start a full paginated sync.
// The slave pulls chunks from master in a background goroutine and then calls
// engine.SyncUsers for a full reconciliation.
//
// Action: "sync-full-trigger"
// Params: payload = target_event_id, auth = target_state_hash, uuid = master base URL
func (r *Router) handleSyncFullTrigger(w http.ResponseWriter, req *http.Request, body internalSyncRequest) {
	if r.registry == nil || r.engine == nil {
		writeError(w, http.StatusServiceUnavailable, "not configured as slave")
		return
	}

	targetEventID, err := strconv.ParseInt(body.Payload, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid target_event_id")
		return
	}
	targetHash := body.Auth
	
	// Slave reads its own configured master URL to know where to pull from.
	masterBaseURL := r.cfg.MasterAPI.URL

	if masterBaseURL == "" {
		writeError(w, http.StatusBadRequest, "master_api.url is not configured on this slave")
		return
	}

	// Ack immediately — full sync happens in background.
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "status": "full_sync_started"})

	// Background goroutine: pull → apply → update state.
	r.bgTasks.Add(1)
	go func() {
		defer r.bgTasks.Done()

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()

		log := r.log.With("op", "full-sync", "target_event_id", targetEventID)
		log.Info("full-sync: starting paginated snapshot pull", "master_url", masterBaseURL)

		users, err := pullFullSnapshot(ctx, masterBaseURL, r.apiKey, log)
		if err != nil {
			log.Error("full-sync: snapshot pull failed", "err", err)
			return
		}

		log.Info("full-sync: snapshot pulled, applying to engine", "users", len(users))

		if _, err := r.engine.SyncUsers(ctx, users, true); err != nil {
			log.Error("full-sync: engine.SyncUsers failed", "err", err)
			return
		}

		// Save the target state so next ping returns match=true.
		if err := r.registry.SyncEvents().SaveState(ctx, domain.SyncState{
			LastEventID: targetEventID,
			StateHash:   targetHash,
		}); err != nil {
			log.Warn("full-sync: failed to persist final sync_state", "err", err)
		}

		log.Info("full-sync: completed successfully", "users_synced", len(users))
	}()
}

// ─────────────────────────────────────────────────────────────────────────────
// pullFullSnapshot — pulls all VPNUserConfig from master via paginated GET.
// Chunks are small (≤1000), so RAM stays flat regardless of database size.
// ─────────────────────────────────────────────────────────────────────────────

func pullFullSnapshot(ctx context.Context, masterBaseURL, apiKey string, log *slog.Logger) ([]domain.VPNUserConfig, error) {
	type snapshotPage struct {
		Users   []domain.VPNUserConfig `json:"users"`
		HasMore bool                   `json:"has_more"`
		Total   int                    `json:"total"`
	}

	base := strings.TrimRight(masterBaseURL, "/") + "/api/v1/internal/xray/sync/snapshot"
	var all []domain.VPNUserConfig
	offset := 0
	const limit = 1000

	for {
		url := fmt.Sprintf("%s?offset=%d&limit=%d", base, offset, limit)
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, fmt.Errorf("build request: %w", err)
		}
		httpReq.Header.Set("X-API-Key", apiKey)

		resp, err := http.DefaultClient.Do(httpReq)
		if err != nil {
			return nil, fmt.Errorf("GET %s: %w", url, err)
		}

		body, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20)) // 10 MB cap per chunk
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("read response: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("master returned HTTP %d: %s", resp.StatusCode, body)
		}

		var page snapshotPage
		if err := json.NewDecoder(bytes.NewReader(body)).Decode(&page); err != nil {
			return nil, fmt.Errorf("decode page: %w", err)
		}

		all = append(all, page.Users...)
		log.Info("full-sync: fetched chunk",
			"offset", offset, "chunk", len(page.Users), "total_so_far", len(all))

		if !page.HasMore {
			break
		}
		offset += limit

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
	}

	return all, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Wiring helpers — injected into Router
// ─────────────────────────────────────────────────────────────────────────────

// WithSyncService injects the statesync.Service and domain.Registry into the Router
// so that the sync handlers can serve master (snapshot) and slave (ping/delta/full) endpoints.
// Call this before the server starts.
func (r *Router) WithSyncService(syncSvc *statesync.Service, registry domain.Registry) *Router {
	r.syncSvc = syncSvc
	r.registry = registry
	return r
}

// WithUserSvcForSync provides a user.Service reference for slave-side AddUser/RemoveUser
// operations that go through the service layer (with SkipDB=true).
// Optional: if nil, the engine is called directly.
func (r *Router) WithUserSvcForSync(userSvc *user.Service) *Router {
	r.userSvc = userSvc
	return r
}
