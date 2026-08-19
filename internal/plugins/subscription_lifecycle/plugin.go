// Package subscription_lifecycle owns scheduled subscription maintenance.
// It owns subscription-expiry policy, warning delivery, and periodic
// device-limit enforcement.
package subscription_lifecycle

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"xraytool/internal/appconfig"
	"xraytool/internal/domain"
	"xraytool/internal/events"
	"xraytool/internal/pluginapi"
	"xraytool/internal/plugins/subscription_lifecycle/worker"
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
		Publishes: []pluginapi.ServiceRef{
			{Name: pluginapi.ServiceSubscriptionLifecycle},
		},
		Requires: []pluginapi.ServiceRef{
			{Name: pluginapi.ServiceDomainRegistry},
			{Name: pluginapi.ServiceEventDispatcher},
			{Name: pluginapi.ServiceDomainEngine},
			{Name: pluginapi.ServiceEventPropagator},
		},
	}
}

func (p *Plugin) Init(_ context.Context, _ pluginapi.RawConfig, reg pluginapi.ServiceResolver) error {
	if p.cfg == nil {
		return fmt.Errorf("subscription_lifecycle: app config must not be nil")
	}
	registry, err := reg.Resolve(pluginapi.ServiceDomainRegistry)
	if err != nil {
		return err
	}
	dispatcher, err := reg.Resolve(pluginapi.ServiceEventDispatcher)
	if err != nil {
		return err
	}
	engine, err := reg.Resolve(pluginapi.ServiceDomainEngine)
	if err != nil {
		return err
	}
	propagator, err := reg.Resolve(pluginapi.ServiceEventPropagator)
	if err != nil {
		return err
	}
	resolvedRegistry, ok := registry.(domain.Registry)
	if !ok || resolvedRegistry == nil {
		return fmt.Errorf("subscription_lifecycle: %s has unexpected type %T", pluginapi.ServiceDomainRegistry, registry)
	}
	resolvedDispatcher, ok := dispatcher.(*events.Dispatcher)
	if !ok || resolvedDispatcher == nil {
		return fmt.Errorf("subscription_lifecycle: %s has unexpected type %T", pluginapi.ServiceEventDispatcher, dispatcher)
	}
	resolvedEngine, ok := engine.(domain.Engine)
	if !ok || resolvedEngine == nil {
		return fmt.Errorf("subscription_lifecycle: %s has unexpected type %T", pluginapi.ServiceDomainEngine, engine)
	}
	resolvedPropagator, ok := propagator.(domain.EventPropagator)
	if !ok || resolvedPropagator == nil {
		return fmt.Errorf("subscription_lifecycle: %s has unexpected type %T", pluginapi.ServiceEventPropagator, propagator)
	}
	p.registry = resolvedRegistry
	p.dispatcher = resolvedDispatcher
	p.engine = resolvedEngine
	p.propagator = resolvedPropagator
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

func (p *Plugin) PublishedServices() map[string]any {
	if p.registry == nil {
		return nil
	}
	return map[string]any{pluginapi.ServiceSubscriptionLifecycle: pluginapi.SubscriptionLifecycle(p)}
}

// ExtendSubscription is the transaction boundary used by payment providers.
// Keeping it with the lifecycle plugin makes subscription business policy
// independent from the mandatory repository foundation.
func (p *Plugin) ExtendSubscription(ctx context.Context, subscriptionID string, months int) error {
	if months <= 0 {
		return fmt.Errorf("subscription_lifecycle: extension months must be positive")
	}
	if p.registry == nil {
		return fmt.Errorf("subscription_lifecycle: not initialized")
	}
	return p.registry.WithTx(ctx, func(tx domain.Registry) error {
		subscription, err := tx.Subscriptions().FindByID(ctx, subscriptionID)
		if err != nil {
			return err
		}
		base := time.Now()
		if subscription.EndsAt != nil && subscription.EndsAt.After(base) {
			base = *subscription.EndsAt
		}
		endsAt := base.AddDate(0, months, 0)
		subscription.EndsAt = &endsAt
		subscription.Status = "active"
		if err := tx.Notifications().ResetForEndsAt(ctx, subscription.ID, &endsAt, nil); err != nil {
			return err
		}
		return tx.Subscriptions().Update(ctx, subscription)
	})
}

var _ pluginapi.Plugin = (*Plugin)(nil)
var _ pluginapi.ServiceProvider = (*Plugin)(nil)
var _ pluginapi.SubscriptionLifecycle = (*Plugin)(nil)
