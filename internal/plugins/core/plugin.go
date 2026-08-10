// Package core implements the mandatory domain-foundation plugin.
//
// It owns stable repository adapters, the event dispatcher, and the engine
// bridge. User management, subscription runtime, and HTTP composition are
// separate plugins so they can evolve independently.
package core

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"xraytool/internal/appconfig"
	"xraytool/internal/domain"
	"xraytool/internal/events"
	"xraytool/internal/pluginapi"
)

// Service names are stable registry keys shared by built-in and external
// plugins. Consumers must declare them in Metadata.Requires.
const (
	ServiceUserRepository         = "user_repository"
	ServiceSubscriptionRepository = "subscription_repository"
	ServiceDeviceRepository       = "device_repository"
	ServicePlanRepository         = "plan_repository"
	ServicePaymentRecorder        = "payment_recorder"
	ServiceSubscriptionLifecycle  = "subscription_lifecycle"
	ServiceUserService            = "user_service"
	ServiceSubscriptionCache      = "subscription_cache"
	ServiceEventDispatcher        = "event_dispatcher"
	ServiceDomainRegistry         = "domain_registry"
	ServiceDomainEngine           = "domain_engine"
	ServiceEventPropagator        = "event_propagator"
	ServiceCoreProvider           = "core_provider"
)

// SubscriptionLifecycle is the narrow transaction port used by payment
// providers. They cannot mutate subscriptions outside this contract.
type SubscriptionLifecycle interface {
	ExtendSubscription(ctx context.Context, subscriptionID string, months int) error
}

type noopEventPropagator struct{}

func (noopEventPropagator) PropagateAll(string, map[string]string) {}

// Runtime contains kernel-owned resources. Core publishes them as explicit
// services after validating that both the registry and engine are available.
type Runtime struct {
	Registry               domain.Registry
	Engine                 domain.Engine
	Propagator             domain.EventPropagator
	UsePluginNotifications bool // retained for source compatibility
	UsePluginEventSinks    bool // retained for source compatibility
}

// Plugin is intentionally small: it is the only mandatory plugin and has no
// dependencies, which makes it the root of every plugin graph.
type Plugin struct {
	cfg        *appconfig.Config
	runtime    Runtime
	registry   domain.Registry
	dispatcher *events.Dispatcher
	reg        pluginapi.ServiceResolver
	auth       func(http.Handler) http.Handler
}

func New(cfg *appconfig.Config) *Plugin {
	return &Plugin{cfg: cfg}
}

func NewWithRuntime(cfg *appconfig.Config, runtime Runtime) *Plugin {
	return &Plugin{cfg: cfg, runtime: runtime}
}

func (p *Plugin) Metadata() pluginapi.Metadata {
	return pluginapi.Metadata{
		Name:        "core",
		Kind:        "core",
		Version:     "1.0.0",
		APIVersion:  pluginapi.CurrentAPIVersion,
		Description: "Mandatory domain foundations: repositories, events, and engine bridge.",
		Mandatory:   true,
		Publishes: []pluginapi.ServiceRef{
			{Name: ServiceUserRepository},
			{Name: ServiceSubscriptionRepository},
			{Name: ServiceDeviceRepository},
			{Name: ServicePlanRepository},
			{Name: ServicePaymentRecorder},
			{Name: ServiceSubscriptionLifecycle},
			{Name: ServiceEventDispatcher},
			{Name: ServiceDomainRegistry},
			{Name: ServiceDomainEngine},
			{Name: ServiceEventPropagator},
			{Name: ServiceCoreProvider},
		},
	}
}

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
	p.registry = p.runtime.Registry
	p.dispatcher = events.NewDispatcher(&events.Config{
		OnDispatch: func(eventType string, data map[string]interface{}, userMeta map[string]interface{}) {
			if p.reg != nil {
				p.reg.EmitEvent(eventType, data, userMeta)
			}
		},
	})
	return nil
}

func (p *Plugin) PublishedServices() map[string]any {
	if p.registry == nil || p.dispatcher == nil {
		return nil
	}
	return map[string]any{
		ServiceUserRepository:         p.UserRepository(),
		ServiceSubscriptionRepository: p.SubscriptionRepository(),
		ServiceDeviceRepository:       p.DeviceRepository(),
		ServicePlanRepository:         p.PlanRepository(),
		ServicePaymentRecorder:        p.registry.Payments(),
		ServiceSubscriptionLifecycle:  p,
		ServiceEventDispatcher:        p.dispatcher,
		ServiceDomainRegistry:         p.registry,
		ServiceDomainEngine:           p.runtime.Engine,
		ServiceEventPropagator:        p.eventPropagator(),
		ServiceCoreProvider:           p,
	}
}

func (p *Plugin) eventPropagator() domain.EventPropagator {
	if p.runtime.Propagator != nil {
		return p.runtime.Propagator
	}
	return noopEventPropagator{}
}

func (p *Plugin) Start(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

func (p *Plugin) Stop(_ context.Context) error {
	if p.dispatcher != nil {
		p.dispatcher.Shutdown()
	}
	return nil
}

func (p *Plugin) Health(_ context.Context) error {
	if p.registry == nil {
		return fmt.Errorf("core plugin: registry not initialised")
	}
	return nil
}

func (p *Plugin) Registry() domain.Registry { return p.registry }

func (p *Plugin) UserRepository() pluginapi.UserRepository {
	if p.registry == nil {
		return nil
	}
	return userRepository{repo: p.registry.Users()}
}

func (p *Plugin) SubscriptionRepository() pluginapi.SubscriptionRepository {
	if p.registry == nil {
		return nil
	}
	return subscriptionRepository{repo: p.registry.Subscriptions()}
}

func (p *Plugin) DeviceRepository() pluginapi.DeviceRepository {
	if p.registry == nil {
		return nil
	}
	return deviceRepository{repo: p.registry.Devices()}
}

func (p *Plugin) PlanRepository() pluginapi.PlanRepository {
	if p.registry == nil {
		return nil
	}
	return planRepository{repo: p.registry.Plans()}
}

func (p *Plugin) Dispatcher() *events.Dispatcher { return p.dispatcher }

// AuthMiddleware preserves the CoreProvider compatibility method while the
// actual middleware is owned and configured by api_server.
func (p *Plugin) AuthMiddleware(next http.Handler) http.Handler {
	if p.auth != nil {
		return p.auth(next)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "API server is not initialized", http.StatusServiceUnavailable)
	})
}

// SetAuthMiddleware is called by api_server during its Init phase. Keeping the
// bridge here avoids breaking existing CoreProvider consumers while moving all
// router implementation out of core.
func (p *Plugin) SetAuthMiddleware(middleware func(http.Handler) http.Handler) {
	p.auth = middleware
}

// ExtendSubscription applies the engine-independent transaction rule used by
// payment providers. It remains a domain foundation rather than a gateway API.
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

var _ pluginapi.Plugin = (*Plugin)(nil)
var _ pluginapi.ServiceProvider = (*Plugin)(nil)
var _ pluginapi.CoreProvider = (*Plugin)(nil)
var _ pluginapi.AuthMiddlewareBinder = (*Plugin)(nil)
var _ SubscriptionLifecycle = (*Plugin)(nil)
