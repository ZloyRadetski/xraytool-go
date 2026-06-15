// Package server wires up the xraytool REST API.
//
// Route layout:
//
//	Public (no auth):
//	  GET  /client                        — subscription config (alias for legacy clients)
//	  GET  /api/v1/sub                    — subscription config (new path)
//
//	Protected (requires X-API-Key header):
//	  POST /api/v1/users/register         — create user
//	  GET  /api/v1/users                  — list all users
//	  GET  /api/v1/users/admins           — list admin telegram IDs
//	  GET  /api/v1/users/telegram/{id}    — get user by telegram ID
//	  GET  /api/v1/users/ref/{code}       — get user by referral code
//	  POST /api/v1/users/telegram/{id}/balance       — adjust balance
//	  POST /api/v1/users/telegram/{id}/max-devices   — set device limit
//	  POST /api/v1/users/telegram/{id}/auto-renew-toggle
//	  POST /api/v1/users/telegram/{id}/auto-renew    — execute auto-renew
//	  POST /api/v1/users/telegram/{id}/metadata      — set metadata key
//	  POST /api/v1/payments/create        — create payment record
//	  GET  /api/v1/payments               — list payments (with filters)
//	  GET  /api/v1/payments/{id}          — get payment by ID
//	  POST /api/v1/payments/{id}/status   — update payment status (atomic)
//	  POST /api/v1/payments/platega/callback — Platega webhook
//
//	Admin-only (requires X-API-Key header):
//	  POST /api/v1/admin/users/{email}/block       — block xray user
//	  POST /api/v1/admin/users/{email}/unblock     — unblock xray user
//	  POST /api/v1/admin/users/{email}/set-expire  — set expiry date
package server

import (
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"

	"xraytool/internal/appconfig"
	"xraytool/internal/events"
	"xraytool/internal/subscription"
)

// Router holds all server-wide dependencies and the configured mux.
type Router struct {
	mux        *http.ServeMux
	apiKey     string
	cfg        *appconfig.Config
	dispatcher *events.Dispatcher
	log        *slog.Logger
	cm         *subscription.CacheManager
}

// New constructs a Router, registers all routes and middleware, and returns the
// underlying http.Handler ready to pass to http.ListenAndServe.
func New(cfg *appconfig.Config, apiKey string, cm *subscription.CacheManager) *Router {
	r := &Router{
		mux:        http.NewServeMux(),
		apiKey:     apiKey,
		cfg:        cfg,
		dispatcher: events.NewDispatcher(cfg),
		log:        slog.Default(),
		cm:         cm,
	}

	r.registerRoutes()
	return r
}

// ServeHTTP implements http.Handler.
func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mux.ServeHTTP(w, req)
}

// registerRoutes wires all routes to their handlers.
func (r *Router) registerRoutes() {
	// ── Public routes (no API key required) ──────────────────────────────────
	r.mux.Handle("GET /client", http.HandlerFunc(r.handleSubscriptionV2))
	r.mux.Handle("GET /api/v1/sub", http.HandlerFunc(r.handleSubscriptionV2))

	// New subscription format path
	r.mux.Handle("GET /api/v2/sub", http.HandlerFunc(r.handleSubscriptionV2))

	// ── Protected routes ─────────────────────────────────────────────────────
	protected := r.authMiddleware

	// Users
	r.mux.Handle("POST /api/v1/users/register", protected(r.handleRegisterUser))
	r.mux.Handle("GET /api/v1/users", protected(r.handleListUsers))
	r.mux.Handle("GET /api/v1/users/admins", protected(r.handleListAdmins))
	r.mux.Handle("GET /api/v1/users/telegram/{id}", protected(r.handleGetUserByTelegram))
	r.mux.Handle("GET /api/v1/users/telegram/{id}/devices", protected(r.handleGetDevices))
	r.mux.Handle("DELETE /api/v1/users/telegram/{id}/devices/{device_id}", protected(r.handleDeleteDevice))
	r.mux.Handle("GET /api/v1/users/uuid/{id}", protected(r.handleGetUserByUUID))
	r.mux.Handle("GET /api/v1/users/ref/{code}", protected(r.handleGetUserByRef))
	r.mux.Handle("POST /api/v1/users/telegram/{id}/balance", protected(r.handleAdjustBalance))
	r.mux.Handle("POST /api/v1/users/telegram/{id}/max-devices", protected(r.handleSetMaxDevices))
	r.mux.Handle("POST /api/v1/users/telegram/{id}/auto-renew-toggle", protected(r.handleAutoRenewToggle))
	r.mux.Handle("POST /api/v1/users/telegram/{id}/auto-renew", protected(r.handleAutoRenew))
	r.mux.Handle("POST /api/v1/users/telegram/{id}/metadata", protected(r.handleSetMetadata))

	// Plans & Promocodes
	r.mux.Handle("GET /api/v1/plans", protected(r.handleGetPlans))
	r.mux.Handle("GET /api/v1/promocodes/validate", protected(r.handleValidatePromoCode))

	// Payments
	r.mux.Handle("POST /api/v1/payments/create", protected(r.handleCreatePayment))
	r.mux.Handle("GET /api/v1/payments", protected(r.handleListPayments))
	r.mux.Handle("GET /api/v1/payments/{id}", protected(r.handleGetPayment))
	r.mux.Handle("POST /api/v1/payments/{id}/status", protected(r.handleUpdatePaymentStatus))

	// Platega callback (protected by its own signature check)
	r.mux.Handle("POST /api/v1/payments/platega/callback", http.HandlerFunc(r.handlePlatgaCallback))

	// Admin
	r.mux.Handle("GET /api/v1/admin/users", protected(r.handleAdminListUsers))
	r.mux.Handle("GET /api/v1/admin/payments/stats", protected(r.handleAdminPaymentsStats))
	r.mux.Handle("POST /api/v1/admin/users/{email}/block", protected(r.handleAdminBlockUser))
	r.mux.Handle("POST /api/v1/admin/users/{email}/unblock", protected(r.handleAdminUnblockUser))
	r.mux.Handle("POST /api/v1/admin/users/{email}/set-expire", protected(r.handleAdminSetExpire))
	r.mux.Handle("POST /api/v1/admin/users/telegram/{id}/global-ban", protected(r.handleAdminGlobalBan))
	r.mux.Handle("POST /api/v1/admin/users/telegram/{id}/global-unban", protected(r.handleAdminGlobalUnban))
	
	// Admin Promocodes
	r.mux.Handle("POST /api/v1/admin/promocodes", protected(r.handleAdminCreatePromoCode))
	r.mux.Handle("GET /api/v1/admin/promocodes", protected(r.handleAdminListPromoCodes))
	r.mux.Handle("PUT /api/v1/admin/promocodes/{id}", protected(r.handleAdminEditPromoCode))
	r.mux.Handle("DELETE /api/v1/admin/promocodes/{id}", protected(r.handleAdminDeletePromoCode))

	// ── Catch-all ─────────────────────────────────────────────────────────────
	r.mux.HandleFunc("/", r.handleNotFound)
}

// ─────────────────────────────────────────────────────────────────────────────
// Middleware
// ─────────────────────────────────────────────────────────────────────────────

// authMiddleware rejects requests that don't carry a valid X-API-Key header.
func (r *Router) authMiddleware(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		key := req.Header.Get("X-API-Key")
		if subtle.ConstantTimeCompare([]byte(key), []byte(r.apiKey)) != 1 {
			r.logIntruder(req, "invalid or missing X-API-Key")
			http.NotFound(w, req)
			return
		}
		next(w, req)
	})
}


// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

// writeJSON serialises v as JSON and writes it with the given HTTP status code.
func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError is a shorthand for writing a standard error JSON body.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// getClientIP parses X-Real-IP safely.
func getClientIP(req *http.Request) string {
	ip := req.Header.Get("X-Real-IP")
	if ip != "" && net.ParseIP(ip) != nil {
		return ip
	}
	return req.RemoteAddr
}

// logIntruder logs a security warning and dumps the request.
func (r *Router) logIntruder(req *http.Request, reason string) {
	ip := getClientIP(req)
	dump, _ := httputil.DumpRequest(req, false)
	r.log.Warn("INTRUDER",
		"reason", reason,
		"ip", ip,
		"dump", strings.TrimSpace(string(dump)),
	)
}

// handleNotFound is the catch-all that logs and returns 404.
func (r *Router) handleNotFound(w http.ResponseWriter, req *http.Request) {
	r.logIntruder(req, "hit undefined route")
	http.NotFound(w, req)
}
