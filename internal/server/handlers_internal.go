package server

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"

	"xraytool/internal/domain"
	"xraytool/internal/stats"
	"xraytool/internal/user"
)

type internalSyncRequest struct {
	Action  string `json:"action"`
	Email   string `json:"email"`
	UUID    string `json:"uuid,omitempty"`
	Payload string `json:"payload,omitempty"`
	Expire  string `json:"expire,omitempty"`
	Subfile string `json:"subfile,omitempty"`
	Auth    string `json:"auth,omitempty"`
	Limit   string `json:"limit,omitempty"`
}

func (r *Router) handleInternalXraySync(w http.ResponseWriter, req *http.Request) {
	var body internalSyncRequest
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		r.log.Warn("internal sync: invalid json", "err", err)
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}

	if body.Email == "" && body.Action != "sync-users" && body.Action != "cli-stats" && body.Action != "antifraud-events" && body.Action != "sync-keys" {
		writeError(w, http.StatusBadRequest, "email is required")
		return
	}

	if body.Action != "antifraud-events" {
		r.log.Info("received internal sync request", "action", body.Action, "email", body.Email)
	} else {
		r.log.Debug("received internal sync request", "action", body.Action, "email", body.Email)
	}

	switch body.Action {
	case "antifraud-events":
		// Slave → master IP aggregation: slave sends batched IP events so that
		// master can maintain a global view across all nodes for fraud detection.
		if r.ingestEvents == nil {
			writeError(w, http.StatusServiceUnavailable, "antifraud not enabled on this node")
			return
		}
		var payloadReq struct {
			Events []domain.FraudEvent `json:"events"`
		}
		if err := json.Unmarshal([]byte(body.Payload), &payloadReq); err != nil || len(payloadReq.Events) == 0 {
			writeError(w, http.StatusBadRequest, "invalid or empty events payload")
			return
		}
		r.ingestEvents(getClientIP(req), payloadReq.Events)
		writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "count": len(payloadReq.Events)})
		return

	case "sync-keys":
		if body.Payload == "" {
			writeError(w, http.StatusBadRequest, "payload is required for sync-keys")
			return
		}
		if err := os.MkdirAll(filepath.Dir(r.cfg.Reality.KeysFilepath), 0755); err != nil {
			r.log.Error("internal sync: failed to create keys directory", "err", err)
			writeError(w, http.StatusInternalServerError, "failed to create keys directory")
			return
		}
		if err := os.WriteFile(r.cfg.Reality.KeysFilepath, []byte(body.Payload), 0600); err != nil {
			r.log.Error("internal sync: failed to save keys", "err", err)
			writeError(w, http.StatusInternalServerError, "failed to save keys")
			return
		}

		type realityKeySyncer interface {
			SyncRealityKeys(ctx context.Context, keysBytes []byte) error
		}
		if syncer, ok := r.engine.(realityKeySyncer); ok {
			if err := syncer.SyncRealityKeys(req.Context(), []byte(body.Payload)); err != nil {
				r.log.Error("internal sync: failed to apply and rebuild reality keys", "err", err)
				writeError(w, http.StatusInternalServerError, "failed to apply reality keys")
				return
			}
		}

		r.log.Info("internal sync: Reality keys successfully synced from master and rebuilt in running engine")
		writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
		return

	case "sync-users":
		var users []domain.VPNUserConfig
		if err := json.Unmarshal([]byte(body.Payload), &users); err != nil {
			r.log.Error("internal sync: invalid payload for sync-users", "err", err)
			writeError(w, http.StatusBadRequest, "invalid payload")
			return
		}

		result, err := r.engine.SyncUsers(req.Context(), users, true)
		if err != nil {
			r.log.Error("internal sync: failed to sync users", "err", err)
			writeError(w, http.StatusInternalServerError, "failed to sync users")
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result) //nolint:errcheck
		return

	case "cli-stats":
		result := stats.GenerateLocalStats(r.cfg, r.engine)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result) //nolint:errcheck
		return

	case "newuser":
		if body.UUID == "" {
			writeError(w, http.StatusBadRequest, "uuid is required for newuser sync")
			return
		}

		var limitPtr *float64
		if body.Limit != "" {
			if limit, err := user.ParseLimit(body.Limit); err == nil {
				limitPtr = limit
			}
		}

		reqCreate := user.CreateUserRequest{
			Email:   body.Email,
			UUID:    body.UUID,
			Subfile: body.Subfile,
			Expire:  body.Expire,
			Auth:    body.Auth,
			Limit:   limitPtr,
			SkipDB:  true,
		}
		if _, err := r.userSvc.CreateUser(req.Context(), reqCreate); err != nil {
			r.log.Error("internal sync: failed to create user", "err", err)
			writeError(w, http.StatusInternalServerError, "failed to create user")
			return
		}

	case "rmuser":
		if err := r.userSvc.BlockOrRemoveUser(req.Context(), user.ModifyUserRequest{
			Email:  body.Email,
			Action: "rm",
			Legacy: false,
			SkipDB: true,
		}); err != nil {
			r.log.Error("internal sync: rmuser failed", "err", err)
		}

	case "limit":
		if err := r.userSvc.BlockOrRemoveUser(req.Context(), user.ModifyUserRequest{
			Email:  body.Email,
			Action: "limit",
			Legacy: false,
			SkipDB: true,
		}); err != nil {
			r.log.Error("internal sync: limit failed", "err", err)
		}

	case "setlimit":
		limit, err := user.ParseLimit(body.Limit)
		if err != nil {
			r.log.Error("internal sync: invalid limit", "err", err)
			writeError(w, http.StatusBadRequest, "invalid limit")
			return
		}
		if err := r.userSvc.UpdateLimit(req.Context(), user.UpdateLimitRequest{
			Email:  body.Email,
			Limit:  limit,
			SkipDB: true,
		}); err != nil {
			r.log.Error("internal sync: setlimit failed", "err", err)
		}

	case "setexpire":
		if err := r.userSvc.SetExpire(req.Context(), user.SetExpireRequest{
			Email:  body.Email,
			Expire: body.Expire,
			SkipDB: true,
		}); err != nil {
			r.log.Error("internal sync: setexpire failed", "err", err)
		}

	case "unlimit":
		limit, err := user.ParseLimit(body.Limit)
		if err != nil {
			r.log.Error("internal sync: invalid limit", "err", err)
			writeError(w, http.StatusBadRequest, "invalid limit")
			return
		}
		if _, err := r.userSvc.UnlimitUser(req.Context(), user.UnlimitUserRequest{
			Email:   body.Email,
			UUID:    body.UUID,
			Subfile: body.Subfile,
			Expire:  body.Expire,
			Auth:    body.Auth,
			Limit:   limit,
			Legacy:  false,
			SkipDB:  true,
		}); err != nil {
			r.log.Error("internal sync: unlimit failed", "err", err)
		}

	default:
		r.log.Warn("internal sync: unknown action", "action", body.Action)
		writeError(w, http.StatusBadRequest, "unknown action")
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{"status": "success", "ok": true})
}
