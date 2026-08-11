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
			{Name: pluginapi.ServiceSubscriptionTemplateProcessor},
			{Name: pluginapi.ServiceClientConfigContributor, Optional: true},
			{Name: pluginapi.ServiceSubscriptionConfigProvider, Optional: true},
			{Name: pluginapi.ServiceTrafficProvider, Optional: true},
			{Name: pluginapi.ServiceTrafficQuotaProvider, Optional: true},
			{Name: pluginapi.ServiceSubscriptionFormatProvider, Optional: true},
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
	processorValue, err := reg.Resolve(pluginapi.ServiceSubscriptionTemplateProcessor)
	if err != nil {
		return err
	}
	processor, ok := processorValue.(pluginapi.SubscriptionTemplateProcessor)
	if !ok || processor == nil {
		return fmt.Errorf("subscription_runtime: %s has unexpected type %T", pluginapi.ServiceSubscriptionTemplateProcessor, processorValue)
	}
	p.cache.SetSubscriptionTemplateProcessor(processor)
	if value, resolveErr := reg.Resolve(pluginapi.ServiceClientConfigContributor); resolveErr == nil {
		if contributor, ok := value.(pluginapi.ClientConfigContributor); ok && contributor != nil {
			p.cache.SetClientConfigContributor(contributor)
		}
	}
	if value, resolveErr := reg.Resolve(pluginapi.ServiceSubscriptionConfigProvider); resolveErr == nil {
		if provider, ok := value.(pluginapi.SubscriptionConfigProvider); ok && provider != nil {
			p.cache.SetSubscriptionConfigProvider(provider)
		}
	}
	trafficValue, trafficErr := reg.Resolve(pluginapi.ServiceTrafficProvider)
	quotaValue, quotaErr := reg.Resolve(pluginapi.ServiceTrafficQuotaProvider)
	traffic, trafficOK := trafficValue.(pluginapi.TrafficProvider)
	quota, quotaOK := quotaValue.(pluginapi.TrafficQuotaProvider)
	if (trafficErr == nil && trafficOK) || (quotaErr == nil && quotaOK) {
		p.cache.SetTrafficProviders(traffic, quota)
	}
	if value, resolveErr := reg.Resolve(pluginapi.ServiceSubscriptionFormatProvider); resolveErr == nil {
		if provider, ok := value.(pluginapi.SubscriptionFormatProvider); ok && provider != nil {
			p.cache.SetSubscriptionFormatProvider(provider)
		}
	}
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
