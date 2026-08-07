package billing

import (
	"context"
	"log/slog"
	"net/http"
	"net"
	"strings"
	json "github.com/goccy/go-json"
	"io"

	"xraytool/internal/appconfig"
	"xraytool/internal/domain"
	"xraytool/internal/events"
	"xraytool/internal/pluginapi"
	usersvc "xraytool/internal/plugins/core/user"
)

type Plugin struct {
	cfg            *appconfig.Config
	paymentSvc     *Service
	userSvc        *usersvc.Service
	dispatcher     *events.Dispatcher
	log            pluginapi.Logger
	registry       domain.Registry
	providers      map[string]pluginapi.PaymentProvider
	authMiddleware func(http.Handler) http.Handler
}

func NewPlugin(cfg *appconfig.Config) *Plugin {
	return &Plugin{
		cfg:       cfg,
		providers: make(map[string]pluginapi.PaymentProvider),
	}
}

func (p *Plugin) Metadata() pluginapi.Metadata {
	return pluginapi.Metadata{
		Name:        "billing",
		Kind:        "payment",
		Version:     "1.0.0",
		APIVersion:  pluginapi.CurrentAPIVersion,
		Description: "Billing plugin",
		Publishes: []pluginapi.ServiceRef{
			{Name: "payment_service"},
		},
		Requires: []pluginapi.ServiceRef{
			{Name: "domain_registry"},
			{Name: "event_dispatcher"},
			{Name: "user_service"},
			{Name: "auth_middleware"},
		},
	}
}

func (p *Plugin) Init(ctx context.Context, rawCfg pluginapi.RawConfig, reg pluginapi.ServiceResolver) error {
	p.log = reg.Logger()
	
	domainReg, err := reg.Resolve("domain_registry")
	if err != nil {
		return err
	}
	p.registry = domainReg.(domain.Registry)
	
	dispatcher, err := reg.Resolve("event_dispatcher")
	if err != nil {
		return err
	}
	p.dispatcher = dispatcher.(*events.Dispatcher)
	
	userSvc, err := reg.Resolve("user_service")
	if err != nil {
		return err
	}
	p.userSvc = userSvc.(*usersvc.Service)

	authMw, err := reg.Resolve("auth_middleware")
	if err != nil {
		return err
	}
	p.authMiddleware = authMw.(func(http.Handler) http.Handler)

	p.paymentSvc = NewService(p.registry, p.dispatcher, slog.Default())
	return nil
}

func (p *Plugin) Start(ctx context.Context) error {
	scrubber := NewScrubberWorker(p.paymentSvc, slog.Default())
	go scrubber.Run(ctx)
	return nil
}

func (p *Plugin) Stop(ctx context.Context) error {
	return nil
}

func (p *Plugin) Health(ctx context.Context) error {
	return nil
}

func (p *Plugin) SetPaymentProviders(providers map[string]pluginapi.PaymentProvider) {
	p.providers = providers
}

func (p *Plugin) PublishedServices() map[string]any {
	return map[string]any{
		"payment_service": p.paymentSvc,
	}
}

func (p *Plugin) RegisterRoutes(mux *http.ServeMux) {
	protected := func(h http.HandlerFunc) http.Handler {
		return p.authMiddleware(h)
	}

	mux.Handle("POST /api/v1/payments/create", protected(p.handleCreatePayment))
	mux.Handle("GET /api/v1/payments", protected(p.handleListPayments))
	mux.Handle("GET /api/v1/payments/{id}", protected(p.handleGetPayment))
	mux.Handle("POST /api/v1/payments/{id}/status", protected(p.handleUpdatePaymentStatus))
	mux.Handle("GET /api/v1/admin/payments/stats", protected(p.handleAdminPaymentsStats))
	
	// Callbacks are called by external payment providers without X-API-Key.
	// They must verify their own signatures instead.
	mux.Handle("POST /api/v1/payments/{method}/callback", http.HandlerFunc(p.handlePaymentCallback))
}

func (p *Plugin) paymentProvider(method string) (pluginapi.PaymentProvider, bool) {
	provider, ok := p.providers[strings.ToLower(strings.TrimSpace(method))]
	return provider, ok
}

func (p *Plugin) unbanUserInXrayAsync(sub domain.Subscription) {
	// Simple stub for now. Ideally use core logic.
}

// Helpers

func readBody(w http.ResponseWriter, req *http.Request, v interface{}) bool {
	raw, err := io.ReadAll(http.MaxBytesReader(w, req.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return false
	}
	if err := json.Unmarshal(raw, v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

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
