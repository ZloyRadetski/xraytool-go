// Package subscription_lifecycle owns scheduled subscription maintenance.
// Core retains subscription transactions; this plugin owns expiry timing,
// warning delivery, and periodic device-limit enforcement.
package subscription_lifecycle

import (
	"context"
	"fmt"
	"log/slog"

	"xraytool/internal/appconfig"
	"xraytool/internal/domain"
	"xraytool/internal/events"
	"xraytool/internal/pluginapi"
	"xraytool/internal/plugins/core"
	"xraytool/internal/plugins/core/worker"
)

type Plugin struct {
	cfg        *appconfig.Config
	registry   domain.Registry
	dispatcher *events.Dispatcher
	engine     domain.Engine
	propagator domain.EventPropagator
}

func New(cfg *appconfig.Config) *Plugin { return &Plugin{cfg: cfg} }

func (p *Plugin) Metadata() pluginapi.Metadata {
	return pluginapi.Metadata{
		Name:        "subscription_lifecycle",
		Kind:        "lifecycle",
		Version:     "1.0.0",
		APIVersion:  pluginapi.CurrentAPIVersion,
		Description: "Expiry, warning, and device-limit maintenance worker.",
		Requires: []pluginapi.ServiceRef{
			{Name: core.ServiceDomainRegistry},
			{Name: core.ServiceEventDispatcher},
			{Name: core.ServiceDomainEngine},
			{Name: core.ServiceEventPropagator},
		},
	}
}

func (p *Plugin) Init(_ context.Context, _ pluginapi.RawConfig, reg pluginapi.ServiceResolver) error {
	if p.cfg == nil {
		return fmt.Errorf("subscription_lifecycle: app config must not be nil")
	}
	registry, err := reg.Resolve(core.ServiceDomainRegistry)
	if err != nil {
		return err
	}
	dispatcher, err := reg.Resolve(core.ServiceEventDispatcher)
	if err != nil {
		return err
	}
	engine, err := reg.Resolve(core.ServiceDomainEngine)
	if err != nil {
		return err
	}
	propagator, err := reg.Resolve(core.ServiceEventPropagator)
	if err != nil {
		return err
	}
	p.registry = registry.(domain.Registry)
	p.dispatcher = dispatcher.(*events.Dispatcher)
	p.engine = engine.(domain.Engine)
	p.propagator = propagator.(domain.EventPropagator)
	return nil
}

func (p *Plugin) Start(ctx context.Context) error {
	if !p.cfg.Worker.Enabled {
		<-ctx.Done()
		return nil
	}
	worker.NewExpiryWorker(p.registry, p.cfg, p.dispatcher, p.engine, slog.Default(), p.propagator).Run(ctx)
	return nil
}

func (p *Plugin) Stop(_ context.Context) error { return nil }

func (p *Plugin) Health(_ context.Context) error {
	if p.registry == nil || p.engine == nil {
		return fmt.Errorf("subscription_lifecycle: not initialized")
	}
	return nil
}

var _ pluginapi.Plugin = (*Plugin)(nil)
