package pricing_default

import (
	"context"
	"errors"
	"testing"
	"time"

	"xraytool/internal/pluginapi"
)

func TestCalculatePrice_SelectsCheapestPromo(t *testing.T) {
	now := time.Date(2026, time.January, 15, 12, 0, 0, 0, time.UTC)
	plugin := initializedPlugin(t)

	result, err := plugin.CalculatePrice(context.Background(), pluginapi.PricingRequest{
		Plan: &pluginapi.Plan{
			Months:                1,
			BasePrice:             1000,
			GlobalDiscountPercent: 10,
		},
		MaxDevices: 5, // 80 units for two extra devices
		PromoCode:  " promo20 ",
		Platform:   "BOT",
		Promo: &pluginapi.PromoCode{
			ID:              42,
			Code:            "PROMO20",
			DiscountPercent: 20,
			TargetPlatform:  "bot",
			IsActive:        true,
		},
		Now: now,
	})
	if err != nil {
		t.Fatalf("CalculatePrice() error = %v", err)
	}
	if result.FinalPrice != 864 { // (1000 + 80) - 20%
		t.Fatalf("FinalPrice = %d, want 864", result.FinalPrice)
	}
	if result.DiscountPercent != 20 || result.AppliedPromo != "PROMO20" || result.AppliedPromoID == nil || *result.AppliedPromoID != 42 {
		t.Fatalf("unexpected selected promo result: %#v", result)
	}
}

func TestCalculatePrice_GlobalDiscountWinsAndClearsPromo(t *testing.T) {
	plugin := initializedPlugin(t)

	result, err := plugin.CalculatePrice(context.Background(), pluginapi.PricingRequest{
		Plan:      &pluginapi.Plan{Months: 1, BasePrice: 1000, GlobalDiscountPercent: 10},
		PromoCode: "PROMO5",
		Platform:  "bot",
		Promo: &pluginapi.PromoCode{
			ID:              7,
			Code:            "PROMO5",
			DiscountPercent: 5,
			TargetPlatform:  "bot",
			IsActive:        true,
		},
	})
	if err != nil {
		t.Fatalf("CalculatePrice() error = %v", err)
	}
	if result.FinalPrice != 900 || result.DiscountPercent != 10 {
		t.Fatalf("global discount result = %#v, want final price 900 / 10%%", result)
	}
	if result.AppliedPromoID != nil || result.AppliedPromo != "" {
		t.Fatalf("global discount must not persist a promo: %#v", result)
	}
}

func TestCalculatePrice_EqualDiscountsPreferPromoLikeLegacyService(t *testing.T) {
	plugin := initializedPlugin(t)

	result, err := plugin.CalculatePrice(context.Background(), pluginapi.PricingRequest{
		Plan:      &pluginapi.Plan{Months: 1, BasePrice: 1000, GlobalDiscountPercent: 20},
		PromoCode: "PROMO20",
		Platform:  "web",
		Promo: &pluginapi.PromoCode{
			ID:              8,
			Code:            "PROMO20",
			DiscountPercent: 20,
			TargetPlatform:  "all",
			IsActive:        true,
		},
	})
	if err != nil {
		t.Fatalf("CalculatePrice() error = %v", err)
	}
	if result.FinalPrice != 800 || result.AppliedPromoID == nil || *result.AppliedPromoID != 8 {
		t.Fatalf("equal discounts must retain the promo: %#v", result)
	}
}

func TestCalculatePrice_ChargesRemainingMonthsForDeviceUpgrade(t *testing.T) {
	now := time.Date(2026, time.January, 15, 12, 0, 0, 0, time.UTC)
	endsAt := now.Add(61 * 24 * time.Hour)
	plugin := initializedPlugin(t)

	result, err := plugin.CalculatePrice(context.Background(), pluginapi.PricingRequest{
		Plan:                &pluginapi.Plan{Months: 3, BasePrice: 1000},
		MaxDevices:          6,
		CurrentSubscription: &pluginapi.Subscription{MaxDevices: 4, EndsAt: &endsAt},
		Now:                 now,
	})
	if err != nil {
		t.Fatalf("CalculatePrice() error = %v", err)
	}
	// Base: 1000. New plan's three extra devices: 3 * 40 * 3 = 360.
	// Upgrade remaining time: ceil(61/30) * (6-4) * 40 = 240.
	if result.FinalPrice != 1600 {
		t.Fatalf("FinalPrice = %d, want 1600", result.FinalPrice)
	}
}

func TestCalculatePrice_IgnoresIneligiblePromoAndRecordsEligibleBalancePromo(t *testing.T) {
	now := time.Date(2026, time.January, 15, 12, 0, 0, 0, time.UTC)
	plugin := initializedPlugin(t)

	expiredAt := now
	planResult, err := plugin.CalculatePrice(context.Background(), pluginapi.PricingRequest{
		Plan:      &pluginapi.Plan{Months: 1, BasePrice: 1000},
		PromoCode: "EXPIRED",
		Platform:  "web",
		Promo: &pluginapi.PromoCode{
			ID:              1,
			Code:            "EXPIRED",
			DiscountPercent: 50,
			TargetPlatform:  "all",
			ExpiresAt:       &expiredAt,
			IsActive:        true,
		},
		Now: now,
	})
	if err != nil {
		t.Fatalf("CalculatePrice() error = %v", err)
	}
	if planResult.FinalPrice != 1000 || planResult.AppliedPromoID != nil {
		t.Fatalf("expired promo must be ignored: %#v", planResult)
	}

	balanceResult, err := plugin.CalculatePrice(context.Background(), pluginapi.PricingRequest{
		Amount:    250,
		PromoCode: "BALANCE",
		Platform:  "web",
		Promo: &pluginapi.PromoCode{
			ID:              9,
			Code:            "BALANCE",
			DiscountPercent: 90,
			TargetPlatform:  "all",
			IsActive:        true,
		},
		Now: now,
	})
	if err != nil {
		t.Fatalf("CalculatePrice() error = %v", err)
	}
	if balanceResult.FinalPrice != 250 || balanceResult.AppliedPromoID == nil || *balanceResult.AppliedPromoID != 9 {
		t.Fatalf("non-plan promo must preserve amount and be recorded: %#v", balanceResult)
	}
}

func TestCalculatePrice_RequiresSnapshotForPlanID(t *testing.T) {
	plugin := initializedPlugin(t)
	_, err := plugin.CalculatePrice(context.Background(), pluginapi.PricingRequest{PlanID: 123})
	if !errors.Is(err, ErrPlanSnapshotRequired) {
		t.Fatalf("error = %v, want ErrPlanSnapshotRequired", err)
	}
}

func TestLifecyclePublishesPricingEngineAfterInit(t *testing.T) {
	plugin := New()
	if err := plugin.Health(context.Background()); err == nil {
		t.Fatal("Health before Init() unexpectedly succeeded")
	}
	if services := plugin.PublishedServices(); services != nil {
		t.Fatalf("services before Init() = %#v, want nil", services)
	}
	if err := plugin.Init(context.Background(), nil, nil); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	services := plugin.PublishedServices()
	engine, ok := services["pricing_engine"].(pluginapi.PricingEngine)
	if !ok || engine != plugin {
		t.Fatalf("published pricing engine = %#v, want plugin", services["pricing_engine"])
	}
	if metadata := plugin.Metadata(); metadata.Name != "pricing_default" || len(metadata.Publishes) != 1 || metadata.Publishes[0].Name != "pricing_engine" {
		t.Fatalf("unexpected metadata: %#v", metadata)
	}
}

func initializedPlugin(t *testing.T) *Plugin {
	t.Helper()
	plugin := New()
	if err := plugin.Init(context.Background(), nil, nil); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	return plugin
}
