package promo

import (
	"context"
	"net/http"

	"xraytool/internal/domain"
	"xraytool/internal/pluginapi"
	usersvc "xraytool/internal/plugins/core/user"
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
		Kind:        "engine", // Just standard plugin
		Version:     "1.0.0",
		APIVersion:  pluginapi.CurrentAPIVersion,
		Description: "Promo code management plugin",
		Requires: []pluginapi.ServiceRef{
			{Name: "domain_registry"},
			{Name: "user_service"},
			{Name: "protected_middleware"},
		},
	}
}

func (p *Plugin) Init(ctx context.Context, rawCfg pluginapi.RawConfig, reg pluginapi.ServiceResolver) error {
	p.log = reg.Logger()

	domainReg, err := reg.Resolve("domain_registry")
	if err != nil {
		return err
	}
	p.registry = domainReg.(domain.Registry)

	userService, err := reg.Resolve("user_service")
	if err != nil {
		return err
	}
	p.userSvc = userService.(*usersvc.Service)

	authMiddleware, err := reg.Resolve("protected_middleware")
	if err != nil {
		return err
	}
	p.authMiddleware = authMiddleware.(func(http.Handler) http.Handler)

	return nil
}

func (p *Plugin) Start(ctx context.Context) error {
	return nil
}

func (p *Plugin) Stop(ctx context.Context) error {
	return nil
}

func (p *Plugin) Health(ctx context.Context) error {
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
