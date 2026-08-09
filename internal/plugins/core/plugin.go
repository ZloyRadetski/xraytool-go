// Package core implements the mandatory core plugin.
//
// The core plugin owns the user/subscription/device/plan/payment domain.
// It wraps the existing domain.Registry, user.Service and payment.Service —
// the business logic is NOT rewritten here; only the Plugin lifecycle glue is
// added so that these services are managed by the Plugin Host.
//
// Design decisions:
//  1. "Mechanical wrap, not rewrite" (plan §6, Phase 1): the existing services
//     are constructed inside Init() exactly as they were in cmd/root.go.
//     No logic changes until tests prove the structural refactor is stable.
//  2. The core plugin publishes the following service names to ServiceRegistry
//     so that other plugins can Resolve() them without a direct import:
//     - "user_repository"
//     - "subscription_repository"
//     - "device_repository"
//     - "plan_repository"
//     - "payment_recorder" (domain.Registry.Payments accessor)
//     - "subscription_lifecycle" (a narrow interface for ExtendSubscription)
//     - "user_service"    (full *user.Service for server.Router during Phase 1)
//     - "payment_service" (full *payment.Service for server.Router during Phase 1)
//     - "event_dispatcher"
//     - "domain_registry" (the full domain.Registry — for handlers that still need it directly;
//     will be narrowed per-consumer in later phases)
//  3. Legacy workers remain in the kernel until the EngineProvider migration;
//     this avoids changing their engine behaviour during the core extraction.
//  4. The HTTP router (server.Router) is NOT constructed here in Phase 1.
//     It remains in cmd/server_kernel.go which calls core.HTTPHandler() to obtain
//     the mux. This allows the gradual migration: handler files in internal/server/
//     are not moved until a later phase.
package core

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"xraytool/internal/appconfig"
	"xraytool/internal/domain"
	"xraytool/internal/events"
	"xraytool/internal/pluginapi"
	"xraytool/internal/plugins/core/server"
	"xraytool/internal/plugins/core/subscription"
	usersvc "xraytool/internal/plugins/core/user"
)

// ServiceName constants are the stable registry keys published by this plugin.
// Other plugins MUST use these constants in their Metadata().Requires declarations.
const (
	ServiceUserRepository         = "user_repository"
	ServiceSubscriptionRepository = "subscription_repository"
	ServiceDeviceRepository       = "device_repository"
	ServicePlanRepository         = "plan_repository"
	ServicePaymentRecorder        = "payment_recorder"
	ServiceSubscriptionLifecycle  = "subscription_lifecycle"
	ServiceUserService            = "user_service"
	ServiceEventDispatcher        = "event_dispatcher"
	ServiceDomainRegistry         = "domain_registry"
	ServiceDomainEngine           = "domain_engine"
	ServiceEventPropagator        = "event_propagator"
)

// SubscriptionLifecycle is the narrow port for subscription-extension business
// logic. Payment plugins Resolve() this service name to call ExtendSubscription
// without depending on *payment.Service directly.
//
// ExtendSubscription and ApplyReferralReward deliberately live in the core
// plugin (plan §2.2.2) — payment plugins must NOT be able to credit subscriptions
// outside this contract.
type SubscriptionLifecycle interface {
	ExtendSubscription(ctx context.Context, subscriptionID string, months int) error
}

type noopEventPropagator struct{}

func (noopEventPropagator) PropagateAll(string, map[string]string) {}

// Runtime contains the kernel-owned resources that the core plugin needs to
// construct its services. The kernel owns the database connection lifecycle;
// the core plugin owns the domain services built on top of it.
//
// This is deliberately a transition boundary: the database handle will become
// a scoped PluginDBHandle once per-plugin migrations are introduced. Passing
// these dependencies at construction time still lets Init publish real values
// before any dependent plugin is initialised.
type Runtime struct {
	Registry               domain.Registry
	Engine                 domain.Engine
	Propagator             domain.EventPropagator
	UsePluginNotifications bool
	UsePluginEventSinks    bool
}

// ─────────────────────────────────────────────────────────────────────────────
// Plugin struct
// ─────────────────────────────────────────────────────────────────────────────

// Plugin is the mandatory core plugin. It wraps the existing services and
// publishes them via ServiceRegistry so other plugins can Resolve() them.
type Plugin struct {
	cfg     *appconfig.Config
	runtime Runtime

	// Populated during Init()
	registry   domain.Registry
	dispatcher *events.Dispatcher
	userSvc    *usersvc.Service
	cacheMgr   *subscription.CacheManager
	httpRouter *server.Router
	log        *slog.Logger
	// Retained for shutdown compatibility while legacy callers may construct the
	// core plugin directly. Core no longer starts lifecycle workers itself.
	cancel      context.CancelFunc
	workersDone chan struct{}

	// ServiceResolver reference stored during Init() for Publish calls
	reg pluginapi.ServiceResolver
}

// New creates an uninitialized core plugin. Call Host.Load() to initialise it.
// cfg is the application config; the plugin reads it during Init().
func New(cfg *appconfig.Config) *Plugin {
	return &Plugin{cfg: cfg}
}

// NewWithRuntime creates a core plugin ready for normal Host.Load lifecycle.
// Runtime resources must be supplied by the kernel before Init so that every
// declared service can be published before a dependent plugin is initialised.
func NewWithRuntime(cfg *appconfig.Config, runtime Runtime) *Plugin {
	return &Plugin{cfg: cfg, runtime: runtime}
}

// ─────────────────────────────────────────────────────────────────────────────
// pluginapi.Plugin implementation
// ─────────────────────────────────────────────────────────────────────────────

func (p *Plugin) Metadata() pluginapi.Metadata {
	return pluginapi.Metadata{
		Name:        "core",
		Kind:        "core",
		Version:     "1.0.0",
		APIVersion:  pluginapi.CurrentAPIVersion,
		Description: "Mandatory core plugin: user/subscription/device/plan/payment domain.",
		Mandatory:   true,
		Publishes: []pluginapi.ServiceRef{
			{Name: ServiceUserRepository},
			{Name: ServiceSubscriptionRepository},
			{Name: ServiceDeviceRepository},
			{Name: ServicePlanRepository},
			{Name: ServicePaymentRecorder},
			{Name: ServiceSubscriptionLifecycle},
			{Name: ServiceUserService},
			{Name: ServiceEventDispatcher},
			{Name: ServiceDomainRegistry},
			{Name: ServiceDomainEngine},
			{Name: ServiceEventPropagator},
			{Name: "auth_middleware"},
			{Name: "protected_middleware"},
		},
		// Core has no Requires — it is the root of the dependency graph.
		Requires: nil,
	}
}

// Init constructs all core services and publishes them to the ServiceRegistry.
// It mirrors the logic from cmd/root.go loadDependencies() without the engine
// construction (engines are separate EngineProvider plugins, resolved later by
// cmd/server_kernel.go).
//
// NOTE: p.cfg is already populated by New(); the ServiceResolver is used only
// for Publish() — core has no required services to Resolve().
func (p *Plugin) Init(_ context.Context, _ pluginapi.RawConfig, reg pluginapi.ServiceResolver) error {
	if p.cfg == nil {
		return fmt.Errorf("core plugin: app config must not be nil")
	}
	if p.runtime.Registry == nil {
		return fmt.Errorf("core plugin: runtime registry must be provided before Init")
	}
	if p.runtime.Engine == nil {
		return fmt.Errorf("core plugin: runtime engine must be provided before Init")
	}
	p.reg = reg
	p.log = slog.Default().With("plugin", "core")

	// ── Events dispatcher ──────────────────────────────────────────────
	p.dispatcher = events.NewDispatcher(&events.Config{
		OnDispatch: func(eventType string, data map[string]interface{}, userMeta map[string]interface{}) {
			if p.reg != nil {
				p.reg.EmitEvent(eventType, data, userMeta)
			}
		},
	})

	return p.initServices(p.runtime.Registry, p.runtime.Engine, p.runtime.Propagator)
}

func (p *Plugin) initServices(
	registry domain.Registry,
	engine domain.Engine,
	propagator domain.EventPropagator,
) error {
	p.registry = registry

	// ── user.Service ───────────────────────────────────────────────────
	p.userSvc = usersvc.NewService(
		registry,
		usersvc.Config{
			IsMaster: p.cfg.IsMaster(),
			Domain:   p.cfg.Server.Domain,
		},
		engine,
		propagator,
		slog.Default(),
	)

	// ── subscription cache manager ─────────────────────────────────────
	p.cacheMgr = subscription.NewCacheManager(p.cfg, engine)
	p.cacheMgr.Refresh()

	return nil
}

// PublishedServices exposes the concrete services named in Metadata. The Host
// owns the actual publication and verifies that this list exactly matches the
// plugin metadata before it starts any dependent plugin.
func (p *Plugin) PublishedServices() map[string]any {
	if p.registry == nil || p.userSvc == nil || p.dispatcher == nil {
		return nil
	}
	return map[string]any{
		ServiceUserRepository:         p.UserRepository(),
		ServiceSubscriptionRepository: p.SubscriptionRepository(),
		ServiceDeviceRepository:       p.DeviceRepository(),
		ServicePlanRepository:         p.PlanRepository(),
		ServicePaymentRecorder:        p.registry.Payments(),
		ServiceSubscriptionLifecycle:  p,
		ServiceUserService:            p.userSvc,
		ServiceEventDispatcher:        p.dispatcher,
		ServiceDomainRegistry:         p.registry,
		ServiceDomainEngine:           p.runtime.Engine,
		ServiceEventPropagator:        p.eventPropagator(),
		"auth_middleware":             p.AuthMiddleware,
		"protected_middleware":        p.ProtectedMiddleware,
	}
}

func (p *Plugin) eventPropagator() domain.EventPropagator {
	if p.runtime.Propagator != nil {
		return p.runtime.Propagator
	}
	return noopEventPropagator{}
}

// InitHTTPRouter constructs the server.Router after InitWithRegistry() and
// engine are available. Separated so the kernel can inject the engine reference
// (which may itself be a MultiEngine assembled from loaded engine plugins)
// without coupling core to the concrete vpn.Adapter type.
func (p *Plugin) InitHTTPRouter(
	engine domain.Engine,
	apiKey string,
) {
	p.httpRouter = server.NewWithOptions(
		p.cfg,
		apiKey,
		p.cacheMgr,
		engine,
		p.userSvc,
		p.dispatcher,
		slog.Default(),
		p.registry,
		server.Options{DisableLegacyMailer: p.runtime.UsePluginNotifications},
	)
}

func (p *Plugin) AuthMiddleware(next http.Handler) http.Handler {
	return p.httpRouter.AuthMiddleware(next)
}

// ProtectedMiddleware exposes the complete API protection chain to HTTP
// contributor plugins, including transparent gzip request/response handling.
func (p *Plugin) ProtectedMiddleware(next http.Handler) http.Handler {
	return p.httpRouter.ProtectedMiddleware(next)
}

// Start launches core workers (expiry, scrubber) and keeps the plugin alive
// until the host cancels its context.
//
// The ExpiryWorker and ScrubberWorker are domain concerns of the core plugin
// (subscription lifecycle, payment privacy), so they run here — not in the
// kernel composition root.
func (p *Plugin) Start(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

// Stop performs graceful shutdown: cancels worker context and waits for
// in-flight webhook deliveries and worker goroutines.
func (p *Plugin) Stop(ctx context.Context) error {
	if p.cancel != nil {
		p.cancel()
	}
	// Start is asynchronous, so shutdown can arrive before it has stored its
	// completion channel. In that case there is no core-owned work to await.
	if p.workersDone != nil {
		select {
		case <-p.workersDone:
		case <-ctx.Done():
			return fmt.Errorf("core plugin: stop timeout — lifecycle did not exit cleanly")
		}
	}
	// Drain in-flight webhook deliveries.
	if p.httpRouter != nil {
		p.httpRouter.Shutdown()
	} else if p.dispatcher != nil {
		p.dispatcher.Shutdown()
	}
	// Wait for any async user service propagations.
	if p.userSvc != nil {
		p.userSvc.Wait()
	}
	return nil
}

// Health returns nil if all core services are operational.
func (p *Plugin) Health(_ context.Context) error {
	if p.registry == nil {
		return fmt.Errorf("core plugin: registry not initialised")
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Service accessors — also published in ServiceRegistry by key
// ─────────────────────────────────────────────────────────────────────────────

// Registry returns the domain.Registry. For internal use and for the kernel
// to publish as ServiceDomainRegistry.
func (p *Plugin) Registry() domain.Registry { return p.registry }

// UserRepository exposes the plugin API adapter for the legacy user port.
func (p *Plugin) UserRepository() pluginapi.UserRepository {
	if p.registry == nil {
		return nil
	}
	return userRepository{repo: p.registry.Users()}
}

// SubscriptionRepository exposes the plugin API adapter for subscriptions.
func (p *Plugin) SubscriptionRepository() pluginapi.SubscriptionRepository {
	if p.registry == nil {
		return nil
	}
	return subscriptionRepository{repo: p.registry.Subscriptions()}
}

// DeviceRepository exposes the plugin API adapter for device tracking.
func (p *Plugin) DeviceRepository() pluginapi.DeviceRepository {
	if p.registry == nil {
		return nil
	}
	return deviceRepository{repo: p.registry.Devices()}
}

// PlanRepository exposes the plugin API adapter for plans.
func (p *Plugin) PlanRepository() pluginapi.PlanRepository {
	if p.registry == nil {
		return nil
	}
	return planRepository{repo: p.registry.Plans()}
}

// Dispatcher returns the events.Dispatcher.
func (p *Plugin) Dispatcher() *events.Dispatcher { return p.dispatcher }

// UserSvc returns the user.Service.
func (p *Plugin) UserSvc() *usersvc.Service { return p.userSvc }

// ExtendSubscription applies the core's engine-agnostic extension rule to one
// subscription. Payment providers use this narrow service rather than writing
// subscription rows themselves.
func (p *Plugin) ExtendSubscription(ctx context.Context, subscriptionID string, months int) error {
	if months <= 0 {
		return fmt.Errorf("core plugin: extension months must be positive")
	}
	if p.registry == nil {
		return fmt.Errorf("core plugin: registry not initialised")
	}

	return p.registry.WithTx(ctx, func(tx domain.Registry) error {
		sub, err := tx.Subscriptions().FindByID(ctx, subscriptionID)
		if err != nil {
			return err
		}

		base := time.Now()
		if sub.EndsAt != nil && sub.EndsAt.After(base) {
			base = *sub.EndsAt
		}
		endsAt := base.AddDate(0, months, 0)
		sub.EndsAt = &endsAt
		sub.Status = "active"
		return tx.Subscriptions().Update(ctx, sub)
	})
}

// CacheManager returns the subscription.CacheManager.
func (p *Plugin) CacheManager() *subscription.CacheManager { return p.cacheMgr }

// HTTPHandler returns the http.Handler for the REST API mux.
// Returns nil if InitHTTPRouter() has not been called yet.
func (p *Plugin) HTTPHandler() http.Handler {
	if p.httpRouter != nil {
		return p.httpRouter
	}
	return nil
}

// HTTPRouter returns the *server.Router, needed to call WithAntiFraud,
// WithSyncService, etc. after the router is constructed.
func (p *Plugin) HTTPRouter() *server.Router { return p.httpRouter }

var _ pluginapi.Plugin = (*Plugin)(nil)
var _ pluginapi.ServiceProvider = (*Plugin)(nil)
var _ pluginapi.CoreProvider = (*Plugin)(nil)
var _ SubscriptionLifecycle = (*Plugin)(nil)
