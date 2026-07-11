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
	"compress/gzip"
	"crypto/subtle"
	json "github.com/goccy/go-json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"regexp"
	"strings"
	"sync"

	"xraytool/internal/antifraud"
	"xraytool/internal/appconfig"
	"xraytool/internal/domain"
	"xraytool/internal/events"
	"xraytool/internal/mailer"
	"xraytool/internal/payment"
	"xraytool/internal/statesync"
	"xraytool/internal/subscription"
	usersvc "xraytool/internal/user"
)

// Router holds all server-wide dependencies and the configured mux.
type Router struct {
	mux        *http.ServeMux
	apiKey     string
	cfg        *appconfig.Config
	dispatcher *events.Dispatcher
	log        *slog.Logger
	cm         *subscription.CacheManager
	mailer     mailer.Mailer
	engine     domain.Engine
	userSvc    *usersvc.Service
	paymentSvc *payment.Service
	// registry is set on slave nodes so that sync handlers can read/write SyncEvents.
	registry domain.Registry
	// syncSvc is set on master nodes to serve snapshot/state endpoints.
	syncSvc *statesync.Service
	// antifraud hooks — nil when the module is disabled.
	isBanned    func(email string) bool
	forceUnban  func(email string)
	getSnapshot func() antifraud.SnapshotData
	// ingestEvents is called when master receives IP events from a slave node.
	// nil when the module is disabled or when running in slave mode.
	ingestEvents func(slaveID string, events []domain.FraudEvent)

	bgTasks  sync.WaitGroup
	webRegMu sync.Mutex //nolint:unused
}

// Shutdown waits for all background tasks (like webhooks and xray unbans) to complete.
func (r *Router) Shutdown() {
	r.dispatcher.Shutdown()
	r.bgTasks.Wait()
}

// New constructs a Router, registers all routes and middleware, and returns the
// underlying http.Handler ready to pass to http.ListenAndServe.
func New(cfg *appconfig.Config, apiKey string, cm *subscription.CacheManager, engine domain.Engine, userSvc *usersvc.Service, paymentSvc *payment.Service, dispatcher *events.Dispatcher, log *slog.Logger) *Router {
	r := &Router{
		mux:        http.NewServeMux(),
		apiKey:     apiKey,
		cfg:        cfg,
		dispatcher: dispatcher,
		log:        log.With("component", "http-server"),
		cm:         cm,
		engine:     engine,
		userSvc:    userSvc,
		paymentSvc: paymentSvc,
	}

	// Wire up mailer if configured.
	if cfg.Mailer.Enabled && cfg.Mailer.ResendAPIKey != "" {
		r.mailer = mailer.New(cfg.Mailer.ResendAPIKey, cfg.Mailer.FromEmail)
		r.log.Info("mailer: resend email delivery enabled", "from", cfg.Mailer.FromEmail)
	} else {
		r.log.Info("mailer: disabled (set mailer.enabled=true in config to enable email delivery)")
	}

	r.registerRoutes()
	return r
}

// WithAntiFraud injects the anti-fraud module hooks into the router.
// Call this before the server starts serving requests.
func (r *Router) WithAntiFraud(
	isBanned func(string) bool,
	forceUnban func(string),
	getSnapshot func() antifraud.SnapshotData,
	ingestEvents func(string, []domain.FraudEvent),
) *Router {
	r.isBanned = isBanned
	r.forceUnban = forceUnban
	r.getSnapshot = getSnapshot
	r.ingestEvents = ingestEvents
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
	// All protected routes are gzip-aware:
	//   - responses are compressed when the client sends Accept-Encoding: gzip
	//   - request bodies are transparently decompressed when Content-Encoding: gzip
	protected := func(h http.HandlerFunc) http.Handler {
		return r.gzipMiddleware(r.authMiddleware(h))
	}

	// Files & Webhooks
	r.mux.Handle("POST /api/rest/update-links", protected(r.handleUpdateLinks))
	r.mux.Handle("POST /api/rest/upload", protected(r.handleUpload))
	r.mux.Handle("GET /api/rest/download", protected(r.handleDownload))

	// Users
	r.mux.Handle("POST /api/v1/users/register", protected(r.handleRegisterUser))
	r.mux.Handle("POST /api/v1/users/request_code", protected(r.handleRequestCode))
	r.mux.Handle("POST /api/v1/users/verify_code", protected(r.handleVerifyCode))
	r.mux.Handle("GET /api/v1/users", protected(r.handleListUsers))
	r.mux.Handle("GET /api/v1/users/admins", protected(r.handleListAdmins))
	r.mux.Handle("GET /api/v1/users/{platform}/{id}", protected(r.handleGetUserByPlatform))
	r.mux.Handle("GET /api/v1/users/{platform}/{id}/devices", protected(r.handleGetDevices))
	r.mux.Handle("DELETE /api/v1/users/{platform}/{id}/devices/{device_id}", protected(r.handleDeleteDevice))
	r.mux.Handle("GET /api/v1/users/ref/{code}", protected(r.handleGetUserByRef))
	r.mux.Handle("POST /api/v1/users/{platform}/{id}/balance", protected(r.handleAdjustBalance))
	r.mux.Handle("POST /api/v1/users/{platform}/{id}/max-devices", protected(r.handleSetMaxDevices))
	r.mux.Handle("POST /api/v1/users/{platform}/{id}/auto-renew-toggle", protected(r.handleAutoRenewToggle))
	r.mux.Handle("POST /api/v1/users/{platform}/{id}/auto-renew", protected(r.handleAutoRenew))
	r.mux.Handle("POST /api/v1/users/{platform}/{id}/metadata", protected(r.handleSetMetadata))

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
	r.mux.Handle("POST /api/v1/admin/users/{platform}/{id}/global-ban", protected(r.handleAdminGlobalBan))
	r.mux.Handle("POST /api/v1/admin/users/{platform}/{id}/global-unban", protected(r.handleAdminGlobalUnban))
	r.mux.Handle("DELETE /api/v1/admin/users/{platform}/{id}", protected(r.handleAdminDeleteUser))
	// Anti-Fraud
	r.mux.Handle("GET /api/v1/admin/antifraud/state", protected(r.handleAdminAntiFraudState))

	// Admin Promocodes
	r.mux.Handle("POST /api/v1/admin/promocodes", protected(r.handleAdminCreatePromoCode))
	r.mux.Handle("GET /api/v1/admin/promocodes", protected(r.handleAdminListPromoCodes))
	r.mux.Handle("PUT /api/v1/admin/promocodes/{id}", protected(r.handleAdminEditPromoCode))
	r.mux.Handle("DELETE /api/v1/admin/promocodes/{id}", protected(r.handleAdminDeletePromoCode))

	// Internal — existing single-action sync endpoint (kept for backward compat).
	r.mux.Handle("POST /api/v1/internal/xray/sync", protected(r.handleInternalXraySync))

	// ── New sync endpoints (master serves, slave pulls) ───────────────────────
	// Master-side: slave calls these to get state info and snapshot chunks.
	r.mux.Handle("GET /api/v1/internal/xray/sync/snapshot", protected(r.handleSyncSnapshot))
	r.mux.Handle("GET /api/v1/internal/xray/sync/state", protected(r.handleSyncState))

	// ── Catch-all ─────────────────────────────────────────────────────────────
	r.mux.HandleFunc("/", r.handleNotFound)
}

// ─────────────────────────────────────────────────────────────────────────────
// Middleware
// ─────────────────────────────────────────────────────────────────────────────

// gzipResponseWriter wraps http.ResponseWriter to transparently compress
// response bodies using gzip. WriteHeader is forwarded as-is so that
// status codes (including 204/304) are still sent correctly.
type gzipResponseWriter struct {
	http.ResponseWriter
	writer *gzip.Writer
}

func (w *gzipResponseWriter) Write(b []byte) (int, error) {
	return w.writer.Write(b)
}

// gzipReadCloser wraps a gzip.Reader together with the original body so that
// both are properly closed when the request body is drained.
type gzipReadCloser struct {
	*gzip.Reader
	underlying io.Closer
}

func (g *gzipReadCloser) Close() error {
	if err := g.Reader.Close(); err != nil {
		_ = g.underlying.Close()
		return err
	}
	return g.underlying.Close()
}

// gzipMiddleware adds bidirectional gzip support:
//   - Decompresses request bodies when Content-Encoding: gzip is present.
//   - Compresses response bodies when the client advertises Accept-Encoding: gzip.
func (r *Router) gzipMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// ── 1. Decompress incoming request body ───────────────────────────────
		if strings.Contains(req.Header.Get("Content-Encoding"), "gzip") {
			gzReader, err := gzip.NewReader(req.Body)
			if err != nil {
				writeError(w, http.StatusBadRequest, "failed to decode gzip request body")
				return
			}
			req.Body = &gzipReadCloser{Reader: gzReader, underlying: req.Body}
			req.Header.Del("Content-Encoding")
		}

		// ── 2. Compress response if client supports it ────────────────────────
		if !strings.Contains(req.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, req)
			return
		}

		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Add("Vary", "Accept-Encoding")

		gzWriter, err := gzip.NewWriterLevel(w, gzip.BestSpeed)
		if err != nil {
			// Fallback: serve uncompressed if gzip writer fails to init.
			w.Header().Del("Content-Encoding")
			next.ServeHTTP(w, req)
			return
		}
		defer gzWriter.Close()

		next.ServeHTTP(&gzipResponseWriter{ResponseWriter: w, writer: gzWriter}, req)
	})
}

// authMiddleware rejects requests that don't carry a valid X-API-Key header.
func (r *Router) authMiddleware(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		key := req.Header.Get("X-API-Key")

		isValid := subtle.ConstantTimeCompare([]byte(key), []byte(r.apiKey)) == 1
		if !isValid && r.cfg != nil {
			for _, srv := range r.cfg.SlaveServers {
				if srv.APIKey != "" && subtle.ConstantTimeCompare([]byte(key), []byte(srv.APIKey)) == 1 {
					isValid = true
					break
				}
			}
		}

		if !isValid {
			r.logIntruder(req, "invalid or missing X-API-Key")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"unauthorized"}`)) //nolint:errcheck
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
func getClientIP(r *http.Request) string {
	remoteIP, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		remoteIP = r.RemoteAddr
	}

	if remoteIP == "127.0.0.1" || remoteIP == "::1" || remoteIP == "localhost" {
		ip := r.Header.Get("X-Real-IP")
		if ip != "" && net.ParseIP(ip) != nil {
			return ip
		}
	}
	return remoteIP
}

// logIntruder logs a security warning and dumps the request.
func (r *Router) logIntruder(req *http.Request, reason string) {
	ip := getClientIP(req)
	dump, _ := httputil.DumpRequest(req, false)
	dumpStr := string(dump)
	// Mask sensitive headers in logs
	dumpStr = regexp.MustCompile(`(?i)(X-API-Key|X-Secret):\s*[^\r\n]+`).ReplaceAllString(dumpStr, "$1: ***")
	r.log.Warn("INTRUDER",
		"reason", reason,
		"ip", ip,
		"dump", strings.TrimSpace(dumpStr),
	)
}

// handleNotFound is the catch-all that logs and returns 404.
func (r *Router) handleNotFound(w http.ResponseWriter, req *http.Request) {
	r.logIntruder(req, "hit undefined route")
	http.NotFound(w, req)
}

// Config returns the configuration. Useful for tests.
func (r *Router) Config() *appconfig.Config {
	return r.cfg
}
