package promo

import (
	"context"
	"fmt"
	"net/http"

	"xraytool/internal/domain"
	"xraytool/internal/pluginapi"
	usersvc "xraytool/internal/plugins/user_management/service"
)

type Plugin struct {
	log            pluginapi.Logger
	registry       domain.Registry
	userSvc        *usersvc.Service
	authMiddleware func(http.Handler) http.Handler
}

func NewPlugin() *Plugin {
	return &Plugin{}
}

func (p *Plugin) Metadata() pluginapi.Metadata {
	return pluginapi.Metadata{
		Name:        "promo",
		Kind:        "api",
		Version:     "1.0.0",
		APIVersion:  pluginapi.CurrentAPIVersion,
		Description: "Promo-code management API.",
		Requires: []pluginapi.ServiceRef{
			{Name: pluginapi.ServiceDomainRegistry},
			{Name: pluginapi.ServiceUserManagement},
			{Name: pluginapi.ServiceProtectedMiddleware},
		},
	}
}

func (p *Plugin) Init(ctx context.Context, rawCfg pluginapi.RawConfig, reg pluginapi.ServiceResolver) error {
	p.log = reg.Logger()

	domainReg, err := reg.Resolve(pluginapi.ServiceDomainRegistry)
	if err != nil {
		return err
	}
	registry, ok := domainReg.(domain.Registry)
	if !ok || registry == nil {
		return fmt.Errorf("promo: %s has unexpected type %T", pluginapi.ServiceDomainRegistry, domainReg)
	}

	userService, err := reg.Resolve(pluginapi.ServiceUserManagement)
	if err != nil {
		return err
	}
	resolvedUserService, ok := userService.(*usersvc.Service)
	if !ok || resolvedUserService == nil {
		return fmt.Errorf("promo: %s has unexpected type %T", pluginapi.ServiceUserManagement, userService)
	}

	authMiddleware, err := reg.Resolve(pluginapi.ServiceProtectedMiddleware)
	if err != nil {
		return err
	}
	protected, ok := authMiddleware.(func(http.Handler) http.Handler)
	if !ok || protected == nil {
		return fmt.Errorf("promo: %s has unexpected type %T", pluginapi.ServiceProtectedMiddleware, authMiddleware)
	}
	p.registry = registry
	p.userSvc = resolvedUserService
	p.authMiddleware = protected

	return nil
}

func (p *Plugin) Start(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

func (p *Plugin) Stop(ctx context.Context) error {
	return nil
}

func (p *Plugin) Health(ctx context.Context) error {
	if p.registry == nil || p.userSvc == nil || p.authMiddleware == nil {
		return fmt.Errorf("promo: not initialized")
	}
	return nil
}

func (p *Plugin) RegisterRoutes(mux *http.ServeMux) {
	protected := func(handler http.HandlerFunc) http.Handler { return p.authMiddleware(handler) }
	mux.Handle("POST /api/v1/admin/promocodes", protected(p.handleAdminCreatePromoCode))
	mux.Handle("GET /api/v1/admin/promocodes", protected(p.handleAdminListPromoCodes))
	mux.Handle("PUT /api/v1/admin/promocodes/{id}", protected(p.handleAdminEditPromoCode))
	mux.Handle("DELETE /api/v1/admin/promocodes/{id}", protected(p.handleAdminDeletePromoCode))
	mux.Handle("GET /api/v1/promocodes/validate", protected(p.handleValidatePromoCode))
}
