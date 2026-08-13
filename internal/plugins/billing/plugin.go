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
	"time"

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
	engine         domain.Engine
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
			{Name: pluginapi.ServiceDomainEngine},
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
	engineService, err := reg.Resolve(pluginapi.ServiceDomainEngine)
	if err != nil {
		return err
	}
	engine, ok := engineService.(domain.Engine)
	if !ok || engine == nil {
		return fmt.Errorf("billing: %s has unexpected type %T", pluginapi.ServiceDomainEngine, engineService)
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
	p.engine = engine
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
	if p.engine == nil || sub.Status != "active" || sub.Email == "" || sub.UUID == "" {
		return
	}

	// A completed payment is not permission to override a global admin ban.
	// Check the current user state before restoring the runtime entry.
	user, err := p.registry.Users().FindByID(context.Background(), sub.UserID)
	if err != nil || user == nil || user.IsBlocked {
		if err != nil && p.log != nil {
			p.log.Warn("billing: skip runtime restore; user lookup failed", "user_id", sub.UserID, "err", err)
		}
		return
	}

	subfile := ""
	if sub.Metadata != nil {
		subfile, _ = sub.Metadata["subfile"].(string)
	}
	expire := ""
	if sub.EndsAt != nil {
		expire = sub.EndsAt.UTC().Format("02.01.2006")
	}
	userConfig := domain.VPNUserConfig{
		Email:      sub.Email,
		UUID:       sub.UUID,
		Subfile:    subfile,
		Expire:     expire,
		MaxDevices: sub.MaxDevices,
	}
	engine := p.engine
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := engine.AddUser(ctx, userConfig); err != nil && p.log != nil {
			p.log.Error("billing: failed to restore paid user in engine", "email", sub.Email, "err", err)
		}
	}()
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

// getClientIP accepts X-Real-IP only from a local/private reverse proxy.
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
