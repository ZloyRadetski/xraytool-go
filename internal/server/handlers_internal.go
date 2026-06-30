package server

import (
	"encoding/json"
	"net/http"

	"xraytool/internal/antifraud"
	"xraytool/internal/slave"
	"xraytool/internal/stats"
	"xraytool/internal/subscription"
	"xraytool/internal/xrayconfig"
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

	if body.Email == "" && body.Action != "usersnapshot" && body.Action != "apply-batch" && body.Action != "cli-stats" && body.Action != "antifraud-events" {
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
			Events []antifraud.SlaveIPEvent `json:"events"`
		}
		if err := json.Unmarshal([]byte(body.Payload), &payloadReq); err != nil || len(payloadReq.Events) == 0 {
			writeError(w, http.StatusBadRequest, "invalid or empty events payload")
			return
		}
		r.ingestEvents(getClientIP(req), payloadReq.Events)
		writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "count": len(payloadReq.Events)})
		return

	case "usersnapshot":
		xrayCfg, err := xrayconfig.Read(r.cfg.Paths.XrayConfig)
		if err != nil {
			r.log.Error("internal sync: failed to read xray config for snapshot", "err", err)
			writeError(w, http.StatusInternalServerError, "failed to read xray config")
			return
		}
		snap := slave.BuildMasterSnapshot(xrayCfg)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(snap)
		return

	case "apply-batch":
		var payload slave.BatchPayload
		if err := json.Unmarshal([]byte(body.Payload), &payload); err != nil {
			r.log.Error("internal sync: invalid payload", "err", err)
			writeError(w, http.StatusBadRequest, "invalid payload")
			return
		}
		result := subscription.ApplyBatchOperations(r.cfg, payload)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result)
		return

	case "cli-stats":
		result := stats.GenerateLocalStats(r.cfg)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result)
		return

	case "newuser":
		if body.UUID == "" {
			writeError(w, http.StatusBadRequest, "uuid is required for newuser sync")
			return
		}

		req := user.CreateUserRequest{
			Email:  body.Email,
			UUID:   body.UUID,
			SkipDB: true, // Internal sync shouldn't save to DB directly if slaves share DB, or just follows Xray propagation.
		}
		svc := user.NewService(r.db, r.cfg)
		if _, err := svc.CreateUser(req); err != nil {
			r.log.Error("internal sync: failed to create user", "err", err)
			writeError(w, http.StatusInternalServerError, "failed to create user")
			return
		}

	case "rmuser":
		req := user.ModifyUserRequest{
			Email:  body.Email,
			Action: "rm",
			Legacy: false,
			SkipDB: true,
		}
		svc := user.NewService(r.db, r.cfg)
		if err := svc.BlockOrRemoveUser(req); err != nil {
			r.log.Error("internal sync: rmuser failed", "err", err)
		}

	case "limit":
		req := user.ModifyUserRequest{
			Email:  body.Email,
			Action: "limit",
			Legacy: false,
			SkipDB: true,
		}
		svc := user.NewService(r.db, r.cfg)
		if err := svc.BlockOrRemoveUser(req); err != nil {
			r.log.Error("internal sync: limit failed", "err", err)
		}

	case "setlimit":
		limit, err := user.ParseLimit(body.Limit)
		if err != nil {
			r.log.Error("internal sync: invalid limit", "err", err)
			writeError(w, http.StatusBadRequest, "invalid limit")
			return
		}
		req := user.UpdateLimitRequest{
			Email:  body.Email,
			Limit:  limit,
			SkipDB: true,
		}
		svc := user.NewService(r.db, r.cfg)
		if err := svc.UpdateLimit(req); err != nil {
			r.log.Error("internal sync: setlimit failed", "err", err)
		}

	case "setexpire":
		req := user.SetExpireRequest{
			Email:  body.Email,
			Expire: body.Expire,
			SkipDB: true,
		}
		svc := user.NewService(r.db, r.cfg)
		if err := svc.SetExpire(req); err != nil {
			r.log.Error("internal sync: setexpire failed", "err", err)
		}

	case "unlimit":
		limit, err := user.ParseLimit(body.Limit)
		if err != nil {
			r.log.Error("internal sync: invalid limit", "err", err)
			writeError(w, http.StatusBadRequest, "invalid limit")
			return
		}
		req := user.UnlimitUserRequest{
			Email:   body.Email,
			UUID:    body.UUID,
			Subfile: body.Subfile,
			Expire:  body.Expire,
			Auth:    body.Auth,
			Limit:   limit,
			Legacy:  false,
			SkipDB:  true,
		}
		svc := user.NewService(r.db, r.cfg)
		if _, err := svc.UnlimitUser(req); err != nil {
			r.log.Error("internal sync: unlimit failed", "err", err)
		}

	default:
		r.log.Warn("internal sync: unknown action", "action", body.Action)
		writeError(w, http.StatusBadRequest, "unknown action")
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{"status": "success", "ok": true})
}
