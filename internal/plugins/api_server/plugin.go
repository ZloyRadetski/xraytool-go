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
	"xraytool/internal/plugins/core"
	"xraytool/internal/plugins/core/server"
	"xraytool/internal/plugins/core/subscription"
	usersvc "xraytool/internal/plugins/core/user"
)

const ServiceHTTPHandler = "api_http_handler"

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
			{Name: "auth_middleware"},
			{Name: "protected_middleware"},
			{Name: ServiceHTTPHandler},
		},
		Requires: []pluginapi.ServiceRef{
			{Name: core.ServiceCoreProvider},
			{Name: core.ServiceDomainRegistry},
			{Name: core.ServiceDomainEngine},
			{Name: core.ServiceUserService},
			{Name: core.ServiceSubscriptionCache},
			{Name: core.ServiceEventDispatcher},
			{Name: pluginapi.ServiceIdentityProvider, Optional: true},
		},
	}
}

func (p *Plugin) Init(_ context.Context, _ pluginapi.RawConfig, reg pluginapi.ServiceResolver) error {
	if p.cfg == nil {
		return fmt.Errorf("api_server: app config must not be nil")
	}
	coreValue, err := reg.Resolve(core.ServiceCoreProvider)
	if err != nil {
		return err
	}
	coreProvider, ok := coreValue.(pluginapi.AuthMiddlewareBinder)
	if !ok || coreProvider == nil {
		return fmt.Errorf("api_server: %s has unexpected type %T", core.ServiceCoreProvider, coreValue)
	}
	registryValue, err := reg.Resolve(core.ServiceDomainRegistry)
	if err != nil {
		return err
	}
	registry, ok := registryValue.(domain.Registry)
	if !ok || registry == nil {
		return fmt.Errorf("api_server: %s has unexpected type %T", core.ServiceDomainRegistry, registryValue)
	}
	engineValue, err := reg.Resolve(core.ServiceDomainEngine)
	if err != nil {
		return err
	}
	engine, ok := engineValue.(domain.Engine)
	if !ok || engine == nil {
		return fmt.Errorf("api_server: %s has unexpected type %T", core.ServiceDomainEngine, engineValue)
	}
	userValue, err := reg.Resolve(core.ServiceUserService)
	if err != nil {
		return err
	}
	userService, ok := userValue.(*usersvc.Service)
	if !ok || userService == nil {
		return fmt.Errorf("api_server: %s has unexpected type %T", core.ServiceUserService, userValue)
	}
	cacheValue, err := reg.Resolve(core.ServiceSubscriptionCache)
	if err != nil {
		return err
	}
	cache, ok := cacheValue.(*subscription.CacheManager)
	if !ok || cache == nil {
		return fmt.Errorf("api_server: %s has unexpected type %T", core.ServiceSubscriptionCache, cacheValue)
	}
	dispatcherValue, err := reg.Resolve(core.ServiceEventDispatcher)
	if err != nil {
		return err
	}
	dispatcher, ok := dispatcherValue.(*events.Dispatcher)
	if !ok || dispatcher == nil {
		return fmt.Errorf("api_server: %s has unexpected type %T", core.ServiceEventDispatcher, dispatcherValue)
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
	coreProvider.SetAuthMiddleware(p.AuthMiddleware)
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
		"auth_middleware":      p.AuthMiddleware,
		"protected_middleware": p.ProtectedMiddleware,
		ServiceHTTPHandler:     http.Handler(p.router),
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
