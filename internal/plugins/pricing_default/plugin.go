// Package pricing_default provides the built-in, backwards-compatible pricing
// policy. Its arithmetic is intentionally a mechanical extraction from
// payment.Service so installations can replace it without changing payment
// persistence or subscription lifecycle rules in core.
package pricing_default

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"time"

	"xraytool/internal/pluginapi"
)

var ErrPlanSnapshotRequired = errors.New("pricing_default: plan snapshot is required for a plan payment")

// Plugin is the default internal PricingEngine. It is deliberately pure: all
// persistence reads are made by core and passed as immutable snapshots in the
// request. That avoids a core <-> pricing dependency cycle and keeps the same
// contract suitable for future external plugins.
type Plugin struct {
	mu          sync.RWMutex
	initialized bool
}

// New creates an uninitialised default pricing plugin.
func New() *Plugin {
	return &Plugin{}
}

func (p *Plugin) Metadata() pluginapi.Metadata {
	return pluginapi.Metadata{
		Name:        "pricing_default",
		Kind:        "pricing",
		Version:     "1.0.0",
		APIVersion:  pluginapi.CurrentAPIVersion,
		Description: "Built-in subscription pricing, device upgrades, and promo selection.",
		Publishes: []pluginapi.ServiceRef{
			{Name: "pricing_engine"},
		},
	}
}

// Init has no dependencies: the core provides all pricing inputs as snapshots.
func (p *Plugin) Init(_ context.Context, _ pluginapi.RawConfig, _ pluginapi.ServiceResolver) error {
	p.mu.Lock()
	p.initialized = true
	p.mu.Unlock()
	return nil
}

// PublishedServices exposes the PricingEngine under its stable service name.
func (p *Plugin) PublishedServices() map[string]any {
	p.mu.RLock()
	initialized := p.initialized
	p.mu.RUnlock()
	if !initialized {
		return nil
	}
	return map[string]any{"pricing_engine": p}
}

// Start has no background work; it remains alive until the host shuts down.
func (p *Plugin) Start(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

func (p *Plugin) Stop(_ context.Context) error { return nil }

func (p *Plugin) Health(_ context.Context) error {
	p.mu.RLock()
	initialized := p.initialized
	p.mu.RUnlock()
	if !initialized {
		return errors.New("pricing_default: plugin is not initialized")
	}
	return nil
}

// CalculatePrice implements the historical price calculation exactly:
//
//   - three devices are included in every plan;
//   - extra devices cost 40 units per device per plan month;
//   - increasing the device limit of an active subscription also charges the
//     remaining months (only when more than seven days remain);
//   - the cheaper of a plan-wide discount and an eligible promo wins; ties use
//     the promo, matching the former payment.Service branch;
//   - a promo on a non-plan payment is recorded but does not change its amount.
func (p *Plugin) CalculatePrice(_ context.Context, req pluginapi.PricingRequest) (pluginapi.PricingResult, error) {
	now := req.Now
	if now.IsZero() {
		now = time.Now()
	}

	if req.Plan == nil {
		if req.PlanID != 0 {
			return pluginapi.PricingResult{}, ErrPlanSnapshotRequired
		}
		result := pluginapi.PricingResult{FinalPrice: req.Amount}
		if promo := eligiblePromo(req, now); promo != nil {
			result.AppliedPromo = promo.Code
			result.AppliedPromoID = int64Ptr(promo.ID)
		}
		return result, nil
	}

	requestedMaxDevices := req.MaxDevices
	// ExtraDevices was part of the first PricingRequest version. Honour it for
	// callers that have not yet switched to MaxDevices.
	if requestedMaxDevices == 0 && req.ExtraDevices > 0 {
		requestedMaxDevices = 3 + req.ExtraDevices
	}

	extraDevicesCost := 0
	if requestedMaxDevices > 3 {
		extraDevicesCost = (requestedMaxDevices - 3) * 40 * req.Plan.Months
	}

	if sub := req.CurrentSubscription; sub != nil && sub.EndsAt != nil && sub.EndsAt.After(now) && requestedMaxDevices > sub.MaxDevices {
		remainingDuration := sub.EndsAt.Sub(now)
		remainingDays := float64(remainingDuration.Hours() / 24.0)
		if remainingDays > 7 {
			upgradeMonths := int(math.Ceil(remainingDays / 30.0))
			currentDevices := sub.MaxDevices
			if currentDevices < 3 {
				currentDevices = 3
			}
			newExtraDevices := requestedMaxDevices - currentDevices
			if newExtraDevices > 0 {
				extraDevicesCost += newExtraDevices * 40 * upgradeMonths
			}
		}
	}

	baseAmount := req.Plan.BasePrice + extraDevicesCost
	globalPrice := baseAmount
	if req.Plan.GlobalDiscountPercent > 0 {
		globalPrice = baseAmount - (baseAmount * req.Plan.GlobalDiscountPercent / 100)
	}

	result := pluginapi.PricingResult{FinalPrice: baseAmount}
	promoPrice := baseAmount
	promo := eligiblePromo(req, now)
	if promo != nil {
		promoPrice = baseAmount - (baseAmount * promo.DiscountPercent / 100)
	}

	// Keep the strict comparison from payment.Service: when two prices are
	// equal, an eligible promo is selected and persisted.
	if globalPrice < promoPrice {
		result.FinalPrice = globalPrice
		result.DiscountPercent = req.Plan.GlobalDiscountPercent
		return result, nil
	}

	result.FinalPrice = promoPrice
	if promo != nil {
		result.DiscountPercent = promo.DiscountPercent
		result.AppliedPromo = promo.Code
		result.AppliedPromoID = int64Ptr(promo.ID)
	}
	return result, nil
}

func eligiblePromo(req pluginapi.PricingRequest, now time.Time) *pluginapi.PromoCode {
	// Keep the legacy guard intentionally exact. The caller resolves the code
	// using strings.TrimSpace/ToUpper, but a non-empty whitespace-only input was
	// still a lookup attempt in payment.Service.
	if req.PromoCode == "" || req.Promo == nil {
		return nil
	}
	promo := req.Promo
	if !promo.IsActive || (promo.ExpiresAt != nil && !now.Before(*promo.ExpiresAt)) {
		return nil
	}

	platform := strings.ToLower(strings.TrimSpace(req.Platform))
	if promo.TargetPlatform != "all" && promo.TargetPlatform != platform {
		return nil
	}
	return promo
}

func int64Ptr(value int64) *int64 {
	return &value
}

var _ pluginapi.PricingEngine = (*Plugin)(nil)
var _ pluginapi.ServiceProvider = (*Plugin)(nil)
