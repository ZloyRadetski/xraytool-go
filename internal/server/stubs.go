package server

import "net/http"

// handleSubscription is implemented in handlers_sub.go (Этап 4.3).
// For now this stub is kept to prevent compile errors.
func (r *Router) handleSubscription(w http.ResponseWriter, req *http.Request) {
	writeError(w, http.StatusNotImplemented, "subscription handler not yet implemented")
}
