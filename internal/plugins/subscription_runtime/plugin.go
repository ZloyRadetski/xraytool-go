// Package subscription_runtime owns the in-memory subscription cache and its
// integration points for traffic and format providers.
package subscription_runtime

import (
	"context"
	"fmt"

	"xraytool/internal/appconfig"
	"xraytool/internal/domain"
	"xraytool/internal/pluginapi"
	"xraytool/internal/plugins/core"
	"xraytool/internal/plugins/core/subscription"
)

type Plugin struct {
	cfg   *appconfig.Config
	cache *subscription.CacheManager
}

func New(cfg *appconfig.Config) *Plugin { return &Plugin{cfg: cfg} }

func (p *Plugin) Metadata() pluginapi.Metadata {
	return pluginapi.Metadata{
		Name:        "subscription_runtime",
		Kind:        "subscription",
		Version:     "1.0.0",
		APIVersion:  pluginapi.CurrentAPIVersion,
		Description: "Cached subscription delivery runtime and provider integration point.",
		Publishes: []pluginapi.ServiceRef{
			{Name: core.ServiceSubscriptionCache},
		},
		Requires: []pluginapi.ServiceRef{
			{Name: core.ServiceDomainEngine},
		},
	}
}

func (p *Plugin) Init(_ context.Context, _ pluginapi.RawConfig, reg pluginapi.ServiceResolver) error {
	if p.cfg == nil {
		return fmt.Errorf("subscription_runtime: app config must not be nil")
	}
	engineValue, err := reg.Resolve(core.ServiceDomainEngine)
	if err != nil {
		return err
	}
	engine, ok := engineValue.(domain.Engine)
	if !ok || engine == nil {
		return fmt.Errorf("subscription_runtime: %s has unexpected type %T", core.ServiceDomainEngine, engineValue)
	}
	p.cache = subscription.NewCacheManager(p.cfg, engine)
	p.cache.Refresh()
	return nil
}

func (p *Plugin) Start(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

func (p *Plugin) Stop(_ context.Context) error { return nil }

func (p *Plugin) Health(_ context.Context) error {
	if p.cache == nil {
		return fmt.Errorf("subscription_runtime: not initialized")
	}
	return nil
}

func (p *Plugin) PublishedServices() map[string]any {
	if p.cache == nil {
		return nil
	}
	return map[string]any{core.ServiceSubscriptionCache: p.cache}
}

func (p *Plugin) CacheManager() *subscription.CacheManager { return p.cache }

var _ pluginapi.Plugin = (*Plugin)(nil)
var _ pluginapi.ServiceProvider = (*Plugin)(nil)
