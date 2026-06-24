package server

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"

	"xraytool/internal/slave"
	"xraytool/internal/xrayapi"
	"xraytool/internal/xrayconfig"
)

type internalSyncRequest struct {
	Action  string `json:"action"`
	Email   string `json:"email"`
	UUID    string `json:"uuid,omitempty"`
	Payload string `json:"payload,omitempty"`
}

func (r *Router) handleInternalXraySync(w http.ResponseWriter, req *http.Request) {
	var body internalSyncRequest
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		r.log.Warn("internal sync: invalid json", "err", err)
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}

	if body.Email == "" && body.Action != "usersnapshot" && body.Action != "apply-batch" {
		writeError(w, http.StatusBadRequest, "email is required")
		return
	}

	r.log.Info("received internal sync request", "action", body.Action, "email", body.Email)

	switch body.Action {
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
		out, err := exec.Command(os.Args[0], "apply-batch", "--payload", body.Payload).Output()
		if err != nil {
			r.log.Error("internal sync: failed to run apply-batch", "err", err, "out", string(out))
			writeError(w, http.StatusInternalServerError, "failed to run apply-batch")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(out)
		return

	case "newuser":
		if body.UUID == "" {
			writeError(w, http.StatusBadRequest, "uuid is required for newuser sync")
			return
		}

		xrayCfg, err := xrayconfig.Read(r.cfg.Paths.XrayConfig)
		if err != nil {
			r.log.Error("internal sync: failed to read xray config", "err", err)
			writeError(w, http.StatusInternalServerError, "failed to read xray config")
			return
		}

		params := xrayconfig.ClientParams{
			Email: body.Email,
			UUID:  body.UUID,
		}

		clientsPayload, err := xrayconfig.BuildForAllInbounds(xrayCfg, params)
		if err != nil {
			r.log.Error("internal sync: failed to build clients", "err", err)
			writeError(w, http.StatusInternalServerError, "failed to build payload")
			return
		}

		if err := xrayconfig.AddUserToInbounds(xrayCfg, clientsPayload); err != nil {
			r.log.Error("internal sync: failed to modify inbounds", "err", err)
			writeError(w, http.StatusInternalServerError, "failed to modify config")
			return
		}

		if err := xrayconfig.Write(r.cfg.Paths.XrayConfig, xrayCfg); err != nil {
			r.log.Error("internal sync: failed to write xray config", "err", err)
			writeError(w, http.StatusInternalServerError, "failed to write config")
			return
		}

		apiClient := xrayapi.NewGRPCClient(r.cfg.Xray.APIAddr)
		_ = apiClient.AddUser(clientsPayload, r.cfg.Paths.XrayConfig)

	case "rmuser":
		var tags []string
		modErr := xrayconfig.Modify(r.cfg.Paths.XrayConfig, func(cfg xrayconfig.RawConfig) error {
			t, _ := xrayconfig.InboundTagsForUser(cfg, body.Email)
			tags = t
			return xrayconfig.RemoveUserFromAllInbounds(cfg, body.Email)
		})
		if modErr != nil {
			r.log.Error("internal sync: rmuser config mod failed", "err", modErr)
		}

		if len(tags) > 0 {
			apiClient := xrayapi.NewGRPCClient(r.cfg.Xray.APIAddr)
			_ = apiClient.RemoveUser(body.Email, tags)
		}

	case "setlimit", "setexpire", "unlimit":
		// Limits and expirations are now purely database-driven. 
		// The Slave nodes read from the same Postgres DB (or handle local sqlite).
		// Xray core does not care about limits/expire (they are checked on /client config fetch or by worker).
		// So these commands don't strictly need Xray core reload. We just acknowledge them.
		r.log.Debug("internal sync: ignoring command that doesn't need xray reload", "action", body.Action)

	default:
		r.log.Warn("internal sync: unknown action", "action", body.Action)
		writeError(w, http.StatusBadRequest, "unknown action")
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{"status": "success", "ok": true})
}
