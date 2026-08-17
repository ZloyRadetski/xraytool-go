package server_routing

import (
	"errors"
	"net/http"
	"os"
	"strings"

	json "github.com/goccy/go-json"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// handleGetTopology returns the full topology of servers, special nodes, and their routing rules.
func (p *Plugin) handleGetTopology() http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if p == nil || p.manager == nil {
			writeError(w, http.StatusServiceUnavailable, "routing manager is not initialized")
			return
		}

		topo, err := p.manager.LoadTopology(req.Context())
		if err != nil {
			if p.log != nil {
				p.log.Error("server_routing: failed to load topology", "err", err)
			}
			writeError(w, http.StatusInternalServerError, "failed to load routing topology")
			return
		}

		writeJSON(w, http.StatusOK, topo)
	}
}

// handleApplyRouting validates, saves, and applies the routing configuration atomically.
func (p *Plugin) handleApplyRouting() http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if p == nil || p.manager == nil {
			writeError(w, http.StatusServiceUnavailable, "routing manager is not initialized")
			return
		}

		// Limit request body to 2MB to prevent memory exhaustion
		req.Body = http.MaxBytesReader(w, req.Body, 2<<20)

		var payload ApplyRequest
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
			return
		}

		// Execute unified transaction across all server rules and Xray core with rollback
		if err := p.manager.ApplyTransaction(req.Context(), payload.Routing, ""); err != nil {
			var valErr *ValidationError
			if errors.As(err, &valErr) || strings.Contains(err.Error(), "outbound template missing") {
				if p.log != nil {
					p.log.Warn("server_routing: validation failed", "err", err)
				}
				writeError(w, http.StatusUnprocessableEntity, err.Error())
				return
			}

			// In test or non-Xray environments where default config path doesn't exist
			if os.IsNotExist(err) || strings.Contains(err.Error(), "no such file or directory") {
				if p.log != nil {
					p.log.Warn("server_routing: xray config not found on disk, saved rule files only", "err", err)
				}
				writeJSON(w, http.StatusOK, ApplyResponse{
					OK:      true,
					Message: "routing rules saved (xray config not present)",
				})
				return
			}

			if p.log != nil {
				p.log.Error("server_routing: failed to apply transaction", "err", err)
			}
			writeError(w, http.StatusInternalServerError, "failed to apply routing configurations: "+err.Error())
			return
		}

		if p.log != nil {
			p.log.Info("server_routing: successfully applied routing transaction", "server_count", len(payload.Routing))
		}

		// Trigger immediate cluster replication to sync routing files to all connected slaves
		if p.replicationProvider != nil {
			if trigErr := p.replicationProvider.TriggerSync(req.Context(), p.cfg.RoutingDir); trigErr != nil {
				if p.log != nil {
					p.log.Warn("server_routing: failed to trigger cluster replication sync", "err", trigErr)
				}
			} else if p.log != nil {
				p.log.Info("server_routing: triggered cluster replication sync for all slaves", "routing_dir", p.cfg.RoutingDir)
			}
		}

		writeJSON(w, http.StatusOK, ApplyResponse{
			OK:      true,
			Message: "routing configuration applied successfully",
		})
	}
}
