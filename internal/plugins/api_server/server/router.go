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
	"context"
	"crypto/subtle"
	json "github.com/goccy/go-json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"xraytool/internal/appconfig"
	"xraytool/internal/domain"
	"xraytool/internal/events"
	"xraytool/internal/pluginapi"
	"xraytool/internal/plugins/subscription_runtime/runtime"
	usersvc "xraytool/internal/plugins/user_management/service"
)

// Router holds all server-wide dependencies and the configured mux.
type Router struct {
	mux        *http.ServeMux
	apiKey     string
	cfg        *appconfig.Config
	dispatcher *events.Dispatcher
	log        *slog.Logger
	cm         *subscription.CacheManager
	// notificationProviders are injected by the plugin-host composition root.
	notificationProviders []pluginapi.NotificationProvider
	engine                domain.Engine
	userSvc               *usersvc.Service
	// paymentProviders are keyed by their normalized MethodID. They are
	// injected after PluginHost.Load, so the router never imports a concrete
	// gateway implementation.
	paymentProviders map[string]pluginapi.PaymentProvider
	identityProvider pluginapi.IdentityProvider
	// registry is set on slave nodes so that sync handlers can read/write SyncEvents.
	registry domain.Registry
	// antifraud hooks — nil when the module is disabled.
	isBanned    func(email string) bool
	forceUnban  func(context.Context, string) error
	getSnapshot func(context.Context) (map[string]any, error)
	// ingestEvents is called when master receives IP events from a slave node.
	// nil when the module is disabled or when running in slave mode.
	ingestEvents func(context.Context, string, []pluginapi.FraudEvent) error

	bgTasks      sync.WaitGroup
	shutdownMu   sync.Mutex
	shuttingDown bool
	webRegMu     sync.Mutex //nolint:unused
}

// Options is retained for source compatibility while router integrations are
// moved to plugin extension points.
type Options struct {
	// DisableLegacyMailer is obsolete. Notification delivery is always supplied
	// through WithNotificationProviders; the field is intentionally a no-op.
	DisableLegacyMailer bool
}

// Shutdown waits for all background tasks (like webhooks and xray unbans) to complete.
func (r *Router) Shutdown() {
	r.shutdownMu.Lock()
	r.shuttingDown = true
	r.shutdownMu.Unlock()
	if r.dispatcher != nil {
		r.dispatcher.Shutdown()
	}
	r.bgTasks.Wait()
}

func (r *Router) beginBackgroundTask() bool {
	r.shutdownMu.Lock()
	defer r.shutdownMu.Unlock()
	if r.shuttingDown {
		return false
	}
	r.bgTasks.Add(1)
	return true
}

// New constructs a Router, registers all routes and middleware, and returns the
// underlying http.Handler ready to pass to http.ListenAndServe.
func New(cfg *appconfig.Config, apiKey string, cm *subscription.CacheManager, engine domain.Engine, userSvc *usersvc.Service, dispatcher *events.Dispatcher, log *slog.Logger, registry domain.Registry) *Router {
	return NewWithOptions(cfg, apiKey, cm, engine, userSvc, dispatcher, log, registry, Options{})
}

// NewWithOptions constructs a Router with explicit transition wiring options.
// It is used by the plugin-host composition root; New remains source-compatible
// for the legacy command path.
func NewWithOptions(cfg *appconfig.Config, apiKey string, cm *subscription.CacheManager, engine domain.Engine, userSvc *usersvc.Service, dispatcher *events.Dispatcher, log *slog.Logger, registry domain.Registry, options Options) *Router {
	if log == nil {
		log = slog.Default()
	}
	r := &Router{
		mux:        http.NewServeMux(),
		apiKey:     apiKey,
		cfg:        cfg,
		dispatcher: dispatcher,
		log:        log.With("component", "http-server"),
		cm:         cm,
		engine:     engine,
		userSvc:    userSvc,
		registry:   registry,
	}

	r.registerRoutes()
	return r
}

// StartMaintenance runs the OTP cleanup loop for the lifetime of the API
// plugin. It is intentionally owned by the host lifecycle rather than init(),
// so tests and graceful shutdown never leave a permanent goroutine behind.
func (r *Router) StartMaintenance(ctx context.Context) {
	if ctx == nil || !r.beginBackgroundTask() {
		return
	}
	go func() {
		defer r.bgTasks.Done()
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				otpCache.sweepExpired()
			}
		}
	}()
}

// AntifraudHooks provides a compatibility bridge for the legacy server
// composition root. New code should use WithAntifraudProvider instead.
type AntifraudHooks struct {
	IsBanned     func(string) bool
	ForceUnban   func(context.Context, string) error
	Snapshot     func(context.Context) (map[string]any, error)
	IngestEvents func(context.Context, string, []pluginapi.FraudEvent) error
}

// WithAntifraudHooks injects legacy anti-fraud callbacks into the router.
// Call this before the server starts serving requests.
func (r *Router) WithAntifraudHooks(hooks AntifraudHooks) *Router {
	r.isBanned = hooks.IsBanned
	r.forceUnban = hooks.ForceUnban
	r.getSnapshot = hooks.Snapshot
	r.ingestEvents = hooks.IngestEvents
	return r
}

// WithAntifraudProvider wires an anti-fraud extension point into the router.
// hotPathLookup should be the kernel-owned local cache. If nil, the provider's
// IsBanned is used for compatibility with in-process providers. Event ingest
// is enabled only on master nodes.
func (r *Router) WithAntifraudProvider(
	provider pluginapi.AntifraudProvider,
	hotPathLookup func(string) bool,
	acceptEvents bool,
) *Router {
	if provider == nil {
		return r.WithAntifraudHooks(AntifraudHooks{})
	}
	if hotPathLookup == nil {
		hotPathLookup = provider.IsBanned
	}
	hooks := AntifraudHooks{
		IsBanned:   hotPathLookup,
		ForceUnban: provider.ForceUnban,
		Snapshot:   provider.Snapshot,
	}
	if acceptEvents {
		hooks.IngestEvents = provider.IngestEvents
	}
	return r.WithAntifraudHooks(hooks)
}

// WithNotificationProviders injects channel-aware delivery plugins. It must be
// called before the router begins serving requests.
func (r *Router) WithNotificationProviders(providers ...pluginapi.NotificationProvider) *Router {
	r.notificationProviders = r.notificationProviders[:0]
	for _, provider := range providers {
		if provider != nil {
			r.notificationProviders = append(r.notificationProviders, provider)
		}
	}
	return r
}

// WithPaymentProviders injects gateway implementations published by the
// plugin host. It must be called before serving requests. A copy is kept so a
// caller cannot mutate the router's dispatch table after wiring.
func (r *Router) WithPaymentProviders(providers map[string]pluginapi.PaymentProvider) *Router {
	r.paymentProviders = make(map[string]pluginapi.PaymentProvider, len(providers))
	for _, provider := range providers {
		if provider == nil {
			continue
		}
		method := strings.ToLower(strings.TrimSpace(provider.MethodID()))
		if method == "" {
			continue
		}
		r.paymentProviders[method] = provider
	}
	return r
}

// WithIdentityProvider injects the OTP/session provider after PluginHost.Load.
// A nil value preserves the legacy in-process OTP cache for the non-plugin
// server command and for backwards compatibility.
func (r *Router) WithIdentityProvider(provider pluginapi.IdentityProvider) *Router {
	r.identityProvider = provider
	return r
}

func (r *Router) paymentProvider(method string) (pluginapi.PaymentProvider, bool) {
	provider, ok := r.paymentProviders[strings.ToLower(strings.TrimSpace(method))]
	return provider, ok
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
		return r.gzipMiddleware(r.AuthMiddleware(h))
	}

	// Users
	r.mux.Handle("POST /api/v1/users/register", protected(r.handleRegisterUser))
	r.mux.Handle("POST /api/v1/users/request_code", protected(r.handleRequestCode))
	r.mux.Handle("POST /api/v1/users/verify_code", protected(r.handleVerifyCode))
	r.mux.Handle("POST /api/v1/users/link_session", protected(r.handleLinkSession))
	r.mux.Handle("POST /api/v1/users/verify_session", protected(r.handleVerifySession))
	r.mux.Handle("POST /api/v1/users/link/telegram", protected(r.handleLinkTelegram))
	r.mux.Handle("POST /api/v1/users/link/email", protected(r.handleLinkEmail))
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

	// Gateway callbacks are authenticated by their selected provider, not by
	// the admin API-key middleware. This lets a newly installed provider expose
	// its conventional /payments/<method>/callback endpoint without adding a
	// concrete route to the API router.
	// (Payments are now handled by the billing plugin)

	// Admin
	r.mux.Handle("GET /api/v1/admin/users", protected(r.handleAdminListUsers))
	r.mux.Handle("POST /api/v1/admin/users/{email}/block", protected(r.handleAdminBlockUser))
	r.mux.Handle("POST /api/v1/admin/users/{email}/unblock", protected(r.handleAdminUnblockUser))
	r.mux.Handle("POST /api/v1/admin/users/{email}/set-expire", protected(r.handleAdminSetExpire))
	r.mux.Handle("POST /api/v1/admin/users/{platform}/{id}/global-ban", protected(r.handleAdminGlobalBan))
	r.mux.Handle("POST /api/v1/admin/users/{platform}/{id}/global-unban", protected(r.handleAdminGlobalUnban))
	r.mux.Handle("DELETE /api/v1/admin/users/{platform}/{id}", protected(r.handleAdminDeleteUser))
	// Anti-Fraud
	r.mux.Handle("GET /api/v1/admin/antifraud/state", protected(r.handleAdminAntiFraudState))

	// Internal — existing single-action sync endpoint (kept for backward compat).

	// ── New sync endpoints (master serves, slave pulls) ───────────────────────
	// Master-side: slave calls these to get state info and snapshot chunks.

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

// AuthMiddleware rejects requests that don't carry a valid X-API-Key header.
func (r *Router) AuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		key := req.Header.Get("X-API-Key")

		isValid := subtle.ConstantTimeCompare([]byte(key), []byte(r.apiKey)) == 1

		if !isValid {
			r.logIntruder(req, "invalid or missing X-API-Key")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"unauthorized"}`)) //nolint:errcheck
			return
		}
		next.ServeHTTP(w, req)
	})
}

// ProtectedMiddleware is the standard wrapper for plugin HTTP routes. It
// preserves the API-key and gzip behaviour of routes owned directly by core.
func (r *Router) ProtectedMiddleware(next http.Handler) http.Handler {
	return r.gzipMiddleware(r.AuthMiddleware(next))
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

// getClientIP accepts X-Real-IP only from a local/private reverse proxy. This
// covers loopback, Docker bridge, and Kubernetes/ingress networks while a
// public client cannot spoof its address by sending the header directly.
func getClientIP(r *http.Request) string {
	remoteIP, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		remoteIP = r.RemoteAddr
	}

	proxyIP := net.ParseIP(strings.TrimSpace(remoteIP))
	if remoteIP == "localhost" || (proxyIP != nil && (proxyIP.IsLoopback() || proxyIP.IsPrivate())) {
		if ip := strings.TrimSpace(r.Header.Get("X-Real-IP")); ip != "" && net.ParseIP(ip) != nil {
			return ip
		}
	}
	return remoteIP
}

// logIntruder records only the minimum data needed to investigate an invalid
// request. Query values and headers may contain subscription secrets, API keys,
// cookies or credentials, so they are deliberately never logged.
func (r *Router) logIntruder(req *http.Request, reason string) {
	r.log.Warn("INTRUDER",
		"reason", reason,
		"ip", getClientIP(req),
		"method", req.Method,
		"path", req.URL.Path,
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
