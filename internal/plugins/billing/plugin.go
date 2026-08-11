package billing

import (
	"context"
	"fmt"
	json "github.com/goccy/go-json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"xraytool/internal/appconfig"
	"xraytool/internal/domain"
	"xraytool/internal/events"
	"xraytool/internal/pluginapi"
	usersvc "xraytool/internal/plugins/user_management/service"
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
		Description: "Billing API and payment orchestration.",
		Publishes: []pluginapi.ServiceRef{
			{Name: pluginapi.ServicePaymentService},
		},
		Requires: []pluginapi.ServiceRef{
			{Name: pluginapi.ServiceDomainRegistry},
			{Name: pluginapi.ServiceEventDispatcher},
			{Name: pluginapi.ServiceUserManagement},
			{Name: pluginapi.ServiceProtectedMiddleware},
		},
	}
}

func (p *Plugin) Init(ctx context.Context, rawCfg pluginapi.RawConfig, reg pluginapi.ServiceResolver) error {
	if p.cfg == nil {
		return fmt.Errorf("billing: app config must not be nil")
	}
	p.log = reg.Logger()

	domainReg, err := reg.Resolve(pluginapi.ServiceDomainRegistry)
	if err != nil {
		return err
	}
	registry, ok := domainReg.(domain.Registry)
	if !ok || registry == nil {
		return fmt.Errorf("billing: %s has unexpected type %T", pluginapi.ServiceDomainRegistry, domainReg)
	}

	dispatcher, err := reg.Resolve(pluginapi.ServiceEventDispatcher)
	if err != nil {
		return err
	}
	resolvedDispatcher, ok := dispatcher.(*events.Dispatcher)
	if !ok || resolvedDispatcher == nil {
		return fmt.Errorf("billing: %s has unexpected type %T", pluginapi.ServiceEventDispatcher, dispatcher)
	}

	userSvc, err := reg.Resolve(pluginapi.ServiceUserManagement)
	if err != nil {
		return err
	}
	resolvedUserService, ok := userSvc.(*usersvc.Service)
	if !ok || resolvedUserService == nil {
		return fmt.Errorf("billing: %s has unexpected type %T", pluginapi.ServiceUserManagement, userSvc)
	}

	authMw, err := reg.Resolve(pluginapi.ServiceProtectedMiddleware)
	if err != nil {
		return err
	}
	protected, ok := authMw.(func(http.Handler) http.Handler)
	if !ok || protected == nil {
		return fmt.Errorf("billing: %s has unexpected type %T", pluginapi.ServiceProtectedMiddleware, authMw)
	}
	p.registry = registry
	p.dispatcher = resolvedDispatcher
	p.userSvc = resolvedUserService
	p.authMiddleware = protected

	p.paymentSvc = NewService(p.registry, p.dispatcher, slog.Default())
	return nil
}

func (p *Plugin) Start(ctx context.Context) error {
	if p.paymentSvc == nil {
		return fmt.Errorf("billing: not initialized")
	}
	scrubber := NewScrubberWorker(p.paymentSvc, slog.Default())
	scrubber.Run(ctx)
	return nil
}

func (p *Plugin) Stop(ctx context.Context) error {
	return nil
}

func (p *Plugin) Health(ctx context.Context) error {
	if p.paymentSvc == nil || p.authMiddleware == nil {
		return fmt.Errorf("billing: not initialized")
	}
	return nil
}

func (p *Plugin) SetPaymentProviders(providers map[string]pluginapi.PaymentProvider) {
	p.providers = providers
}

func (p *Plugin) PublishedServices() map[string]any {
	return map[string]any{
		pluginapi.ServicePaymentService: p.paymentSvc,
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
