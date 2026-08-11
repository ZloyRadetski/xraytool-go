// Package user_management owns user lifecycle operations independently from
// the mandatory domain-foundation plugin.
package user_management

import (
	"context"
	"fmt"
	"log/slog"

	"xraytool/internal/appconfig"
	"xraytool/internal/domain"
	"xraytool/internal/pluginapi"
	usersvc "xraytool/internal/plugins/user_management/service"
)

type Plugin struct {
	cfg     *appconfig.Config
	service *usersvc.Service
}

func New(cfg *appconfig.Config) *Plugin { return &Plugin{cfg: cfg} }

func (p *Plugin) Metadata() pluginapi.Metadata {
	return pluginapi.Metadata{
		Name:        "user_management",
		Kind:        "user_management",
		Version:     "1.0.0",
		APIVersion:  pluginapi.CurrentAPIVersion,
		Description: "User lifecycle service backed by the core domain registry.",
		Publishes: []pluginapi.ServiceRef{
			{Name: pluginapi.ServiceUserManagement},
		},
		Requires: []pluginapi.ServiceRef{
			{Name: pluginapi.ServiceDomainRegistry},
			{Name: pluginapi.ServiceDomainEngine},
			{Name: pluginapi.ServiceEventPropagator},
		},
	}
}

func (p *Plugin) Init(_ context.Context, _ pluginapi.RawConfig, reg pluginapi.ServiceResolver) error {
	if p.cfg == nil {
		return fmt.Errorf("user_management: app config must not be nil")
	}
	registryValue, err := reg.Resolve(pluginapi.ServiceDomainRegistry)
	if err != nil {
		return err
	}
	registry, ok := registryValue.(domain.Registry)
	if !ok || registry == nil {
		return fmt.Errorf("user_management: %s has unexpected type %T", pluginapi.ServiceDomainRegistry, registryValue)
	}
	engineValue, err := reg.Resolve(pluginapi.ServiceDomainEngine)
	if err != nil {
		return err
	}
	engine, ok := engineValue.(domain.Engine)
	if !ok || engine == nil {
		return fmt.Errorf("user_management: %s has unexpected type %T", pluginapi.ServiceDomainEngine, engineValue)
	}
	propagatorValue, err := reg.Resolve(pluginapi.ServiceEventPropagator)
	if err != nil {
		return err
	}
	propagator, ok := propagatorValue.(domain.EventPropagator)
	if !ok || propagator == nil {
		return fmt.Errorf("user_management: %s has unexpected type %T", pluginapi.ServiceEventPropagator, propagatorValue)
	}

	p.service = usersvc.NewService(registry, usersvc.Config{
		IsMaster: p.cfg.IsMaster(),
		Domain:   p.cfg.Server.Domain,
	}, engine, propagator, slog.Default())
	return nil
}

func (p *Plugin) Start(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

func (p *Plugin) Stop(_ context.Context) error {
	if p.service != nil {
		p.service.Wait()
	}
	return nil
}

func (p *Plugin) Health(_ context.Context) error {
	if p.service == nil {
		return fmt.Errorf("user_management: not initialized")
	}
	return nil
}

func (p *Plugin) PublishedServices() map[string]any {
	if p.service == nil {
		return nil
	}
	return map[string]any{pluginapi.ServiceUserManagement: p.service}
}

func (p *Plugin) Service() *usersvc.Service { return p.service }

var _ pluginapi.Plugin = (*Plugin)(nil)
var _ pluginapi.ServiceProvider = (*Plugin)(nil)
