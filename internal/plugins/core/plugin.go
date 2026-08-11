// Package core implements the mandatory domain-foundation plugin.
//
// It owns stable repository adapters, the event dispatcher, and the engine
// bridge. User management, subscription runtime, and HTTP composition are
// separate plugins so they can evolve independently.
package core

import (
	"context"
	"fmt"

	"xraytool/internal/appconfig"
	"xraytool/internal/domain"
	"xraytool/internal/events"
	"xraytool/internal/pluginapi"
)

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
			{Name: pluginapi.ServiceUserRepository},
			{Name: pluginapi.ServiceSubscriptionRepository},
			{Name: pluginapi.ServiceDeviceRepository},
			{Name: pluginapi.ServicePlanRepository},
			{Name: pluginapi.ServicePaymentRecorder},
			{Name: pluginapi.ServiceEventDispatcher},
			{Name: pluginapi.ServiceDomainRegistry},
			{Name: pluginapi.ServiceDomainEngine},
			{Name: pluginapi.ServiceEventPropagator},
			{Name: pluginapi.ServiceCoreProvider},
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
		pluginapi.ServiceUserRepository:         p.UserRepository(),
		pluginapi.ServiceSubscriptionRepository: p.SubscriptionRepository(),
		pluginapi.ServiceDeviceRepository:       p.DeviceRepository(),
		pluginapi.ServicePlanRepository:         p.PlanRepository(),
		pluginapi.ServicePaymentRecorder:        p.registry.Payments(),
		pluginapi.ServiceEventDispatcher:        p.dispatcher,
		pluginapi.ServiceDomainRegistry:         p.registry,
		pluginapi.ServiceDomainEngine:           p.runtime.Engine,
		pluginapi.ServiceEventPropagator:        p.eventPropagator(),
		pluginapi.ServiceCoreProvider:           p,
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

var _ pluginapi.Plugin = (*Plugin)(nil)
var _ pluginapi.ServiceProvider = (*Plugin)(nil)
var _ pluginapi.CoreProvider = (*Plugin)(nil)
