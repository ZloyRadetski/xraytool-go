package payment

import (
	"context"
	"log/slog"
	"testing"

	"xraytool/internal/database"
	"xraytool/internal/domain"
	"xraytool/internal/events"
	"xraytool/internal/pluginapi"
	"xraytool/internal/plugins/pricing_default"
)

func TestPaymentService_DefaultPricingPluginMatchesLegacyCalculation(t *testing.T) {
	registry, planID := pricingTestRegistry(t)
	request := CreatePaymentRequest{
		Email:       "buyer@example.com",
		PaymentType: "subscription",
		Method:      "card",
		PlanID:      &planID,
		MaxDevices:  5,
		PromoCode:   "promo20",
		Platform:    "bot",
	}

	legacy := NewService(registry, events.NewDispatcher(&events.Config{}), slog.Default())
	legacyAmount, legacyPromoID, err := legacy.calculatePaymentPrice(context.Background(), "buyer", request)
	if err != nil {
		t.Fatalf("legacy calculation error = %v", err)
	}

	pricing := pricing_default.New()
	if err := pricing.Init(context.Background(), nil, nil); err != nil {
		t.Fatalf("pricing plugin Init() error = %v", err)
	}
	withPlugin := NewServiceWithPricing(registry, events.NewDispatcher(&events.Config{}), slog.Default(), pricing)
	pluginAmount, pluginPromoID, err := withPlugin.calculatePaymentPrice(context.Background(), "buyer", request)
	if err != nil {
		t.Fatalf("plugin calculation error = %v", err)
	}

	if pluginAmount != legacyAmount {
		t.Fatalf("plugin amount = %d, legacy amount = %d", pluginAmount, legacyAmount)
	}
	if legacyPromoID == nil || pluginPromoID == nil || *pluginPromoID != *legacyPromoID {
		t.Fatalf("plugin promo = %v, legacy promo = %v", pluginPromoID, legacyPromoID)
	}

	created, err := withPlugin.CreatePayment(context.Background(), request)
	if err != nil {
		t.Fatalf("CreatePayment() with pricing plugin error = %v", err)
	}
	if created.Amount != pluginAmount || created.PromoCodeID == nil || *created.PromoCodeID != *pluginPromoID {
		t.Fatalf("created payment did not persist plugin result: %#v", created)
	}
}

func TestPaymentService_DelegatesSnapshotsToInjectedPricingEngine(t *testing.T) {
	registry, planID := pricingTestRegistry(t)
	engine := &recordingPricingEngine{
		result: pluginapi.PricingResult{FinalPrice: 777},
	}
	service := NewServiceWithPricing(registry, events.NewDispatcher(&events.Config{}), slog.Default(), engine)

	amount, promoID, err := service.calculatePaymentPrice(context.Background(), "buyer", CreatePaymentRequest{
		Email:       "buyer@example.com",
		PaymentType: "subscription",
		Method:      "card",
		PlanID:      &planID,
		MaxDevices:  4,
		PromoCode:   " promo20 ",
		Platform:    "BOT",
	})
	if err != nil {
		t.Fatalf("calculatePaymentPrice() error = %v", err)
	}
	if amount != 777 || promoID != nil {
		t.Fatalf("pricing engine output was not used: amount=%d promo=%v", amount, promoID)
	}
	if engine.request.Plan == nil || engine.request.Plan.ID != planID {
		t.Fatalf("plan snapshot was not supplied: %#v", engine.request.Plan)
	}
	if engine.request.Promo == nil || engine.request.Promo.Code != "PROMO20" {
		t.Fatalf("promo snapshot was not supplied: %#v", engine.request.Promo)
	}
	if engine.request.UserID != "buyer" || engine.request.MaxDevices != 4 || engine.request.Platform != "BOT" {
		t.Fatalf("unexpected pricing request: %#v", engine.request)
	}
}

func pricingTestRegistry(t *testing.T) (domain.Registry, int64) {
	t.Helper()
	db, err := database.NewConnection(database.Config{
		Driver:      "sqlite",
		SQLitePath:  ":memory:",
		Silent:      true,
		AutoMigrate: true,
	})
	if err != nil {
		t.Fatalf("NewConnection() error = %v", err)
	}

	if err := db.Create(&database.User{
		ID:       "buyer",
		Username: "buyer@example.com",
		Metadata: database.Metadata{"email": "buyer@example.com"},
	}).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	plan := database.Plan{Months: 1, BasePrice: 1000, GlobalDiscountPercent: 10, IsActive: true}
	if err := db.Create(&plan).Error; err != nil {
		t.Fatalf("create plan: %v", err)
	}
	if err := db.Create(&database.PromoCode{
		Code:            "PROMO20",
		DiscountPercent: 20,
		TargetPlatform:  "bot",
		IsActive:        true,
	}).Error; err != nil {
		t.Fatalf("create promo: %v", err)
	}

	return database.NewRegistry(db), plan.ID
}

type recordingPricingEngine struct {
	request pluginapi.PricingRequest
	result  pluginapi.PricingResult
}

func (p *recordingPricingEngine) Metadata() pluginapi.Metadata {
	return pluginapi.Metadata{Name: "recording", Kind: "pricing"}
}
func (p *recordingPricingEngine) Init(context.Context, pluginapi.RawConfig, pluginapi.ServiceResolver) error {
	return nil
}
func (p *recordingPricingEngine) Start(ctx context.Context) error {
	<-ctx.Done()
	return nil
}
func (p *recordingPricingEngine) Stop(context.Context) error   { return nil }
func (p *recordingPricingEngine) Health(context.Context) error { return nil }
func (p *recordingPricingEngine) CalculatePrice(_ context.Context, req pluginapi.PricingRequest) (pluginapi.PricingResult, error) {
	p.request = req
	return p.result, nil
}

var _ pluginapi.PricingEngine = (*recordingPricingEngine)(nil)
