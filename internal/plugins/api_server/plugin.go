// Package api_server owns HTTP composition, routes, and authentication
// middleware. It preserves the legacy Router while removing HTTP ownership
// from the mandatory core plugin.
package api_server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"xraytool/internal/appconfig"
	"xraytool/internal/domain"
	"xraytool/internal/events"
	"xraytool/internal/pluginapi"
	"xraytool/internal/plugins/api_server/server"
	"xraytool/internal/plugins/subscription_runtime/runtime"
	usersvc "xraytool/internal/plugins/user_management/service"
)

type Plugin struct {
	cfg    *appconfig.Config
	router *server.Router
}

func New(cfg *appconfig.Config) *Plugin { return &Plugin{cfg: cfg} }

func (p *Plugin) Metadata() pluginapi.Metadata {
	return pluginapi.Metadata{
		Name:        "api_server",
		Kind:        "api",
		Version:     "1.0.0",
		APIVersion:  pluginapi.CurrentAPIVersion,
		Description: "HTTP API routes, authentication middleware, and plugin route composition.",
		Publishes: []pluginapi.ServiceRef{
			{Name: pluginapi.ServiceAuthMiddleware},
			{Name: pluginapi.ServiceProtectedMiddleware},
			{Name: pluginapi.ServiceHTTPHandler},
		},
		Requires: []pluginapi.ServiceRef{
			{Name: pluginapi.ServiceDomainRegistry},
			{Name: pluginapi.ServiceDomainEngine},
			{Name: pluginapi.ServiceUserManagement},
			{Name: pluginapi.ServiceSubscriptionRuntime},
			{Name: pluginapi.ServiceEventDispatcher},
			{Name: pluginapi.ServiceIdentityProvider, Optional: true},
		},
	}
}

func (p *Plugin) Init(_ context.Context, _ pluginapi.RawConfig, reg pluginapi.ServiceResolver) error {
	if p.cfg == nil {
		return fmt.Errorf("api_server: app config must not be nil")
	}
	registryValue, err := reg.Resolve(pluginapi.ServiceDomainRegistry)
	if err != nil {
		return err
	}
	registry, ok := registryValue.(domain.Registry)
	if !ok || registry == nil {
		return fmt.Errorf("api_server: %s has unexpected type %T", pluginapi.ServiceDomainRegistry, registryValue)
	}
	engineValue, err := reg.Resolve(pluginapi.ServiceDomainEngine)
	if err != nil {
		return err
	}
	engine, ok := engineValue.(domain.Engine)
	if !ok || engine == nil {
		return fmt.Errorf("api_server: %s has unexpected type %T", pluginapi.ServiceDomainEngine, engineValue)
	}
	userValue, err := reg.Resolve(pluginapi.ServiceUserManagement)
	if err != nil {
		return err
	}
	userService, ok := userValue.(*usersvc.Service)
	if !ok || userService == nil {
		return fmt.Errorf("api_server: %s has unexpected type %T", pluginapi.ServiceUserManagement, userValue)
	}
	cacheValue, err := reg.Resolve(pluginapi.ServiceSubscriptionRuntime)
	if err != nil {
		return err
	}
	cache, ok := cacheValue.(*subscription.CacheManager)
	if !ok || cache == nil {
		return fmt.Errorf("api_server: %s has unexpected type %T", pluginapi.ServiceSubscriptionRuntime, cacheValue)
	}
	dispatcherValue, err := reg.Resolve(pluginapi.ServiceEventDispatcher)
	if err != nil {
		return err
	}
	dispatcher, ok := dispatcherValue.(*events.Dispatcher)
	if !ok || dispatcher == nil {
		return fmt.Errorf("api_server: %s has unexpected type %T", pluginapi.ServiceEventDispatcher, dispatcherValue)
	}

	p.router = server.NewWithOptions(
		p.cfg,
		p.cfg.Server.APIKey,
		cache,
		engine,
		userService,
		dispatcher,
		slog.Default(),
		registry,
		server.Options{},
	)
	if value, resolveErr := reg.Resolve(pluginapi.ServiceIdentityProvider); resolveErr == nil {
		if provider, providerOK := value.(pluginapi.IdentityProvider); providerOK && provider != nil {
			p.router.WithIdentityProvider(provider)
		}
	}
	return nil
}

func (p *Plugin) Start(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

func (p *Plugin) Stop(_ context.Context) error {
	if p.router != nil {
		p.router.Shutdown()
	}
	return nil
}

func (p *Plugin) Health(_ context.Context) error {
	if p.router == nil {
		return fmt.Errorf("api_server: not initialized")
	}
	return nil
}

func (p *Plugin) PublishedServices() map[string]any {
	if p.router == nil {
		return nil
	}
	return map[string]any{
		pluginapi.ServiceAuthMiddleware:      p.AuthMiddleware,
		pluginapi.ServiceProtectedMiddleware: p.ProtectedMiddleware,
		pluginapi.ServiceHTTPHandler:         http.Handler(p.router),
	}
}

func (p *Plugin) AuthMiddleware(next http.Handler) http.Handler {
	return p.router.AuthMiddleware(next)
}

func (p *Plugin) ProtectedMiddleware(next http.Handler) http.Handler {
	return p.router.ProtectedMiddleware(next)
}

func (p *Plugin) HTTPHandler() http.Handler {
	if p.router == nil {
		return nil
	}
	return p.router
}

func (p *Plugin) Router() *server.Router { return p.router }

var _ pluginapi.Plugin = (*Plugin)(nil)
var _ pluginapi.ServiceProvider = (*Plugin)(nil)
