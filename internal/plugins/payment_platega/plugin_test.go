package payment_platega

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"xraytool/internal/pluginapi"
)

type fakeGateway struct {
	createRequest GatewayPaymentRequest
	createResult  GatewayPaymentResult
	createErr     error

	refundExternalID string
	refundAmount     int
	refundErr        error
}

func (g *fakeGateway) CreatePayment(_ context.Context, req GatewayPaymentRequest) (GatewayPaymentResult, error) {
	g.createRequest = req
	if g.createErr != nil {
		return GatewayPaymentResult{}, g.createErr
	}
	return g.createResult, nil
}

func (g *fakeGateway) Refund(_ context.Context, externalID string, amount int) error {
	g.refundExternalID = externalID
	g.refundAmount = amount
	return g.refundErr
}

type fakeResolver struct {
	service any
	err     error
}

func (r fakeResolver) Resolve(name string) (any, error) {
	if name != "payment_recorder" {
		return nil, errors.New("unexpected service " + name)
	}
	return r.service, r.err
}

func (fakeResolver) Logger() pluginapi.Logger                         { return discardLogger{} }
func (fakeResolver) EmitEvent(string, map[string]any, map[string]any) {}
func (fakeResolver) DB() pluginapi.PluginDBHandle                     { return nil }

type discardLogger struct{}

func (discardLogger) Debug(string, ...any) {}
func (discardLogger) Info(string, ...any)  {}
func (discardLogger) Warn(string, ...any)  {}
func (discardLogger) Error(string, ...any) {}

func validConfig() pluginapi.RawConfig {
	return pluginapi.RawConfig{
		"merchant_id": "merchant-1",
		"secret":      "shared-secret",
		"return_url":  "https://example.test/success",
	}
}

func TestMetadataAndPublishedService(t *testing.T) {
	p := NewWithGateway(&fakeGateway{})
	metadata := p.Metadata()
	if metadata.Name != "payment_platega" || metadata.Kind != "payment" {
		t.Fatalf("unexpected metadata: %#v", metadata)
	}
	if len(metadata.Requires) != 1 || metadata.Requires[0].Name != "payment_recorder" {
		t.Fatalf("requires = %#v, want payment_recorder", metadata.Requires)
	}
	if len(metadata.Publishes) != 1 || metadata.Publishes[0].Name != ServiceName {
		t.Fatalf("publishes = %#v, want %q", metadata.Publishes, ServiceName)
	}
	if services := p.PublishedServices(); services != nil {
		t.Fatalf("services before Init = %#v, want nil", services)
	}

	if err := p.Init(context.Background(), validConfig(), fakeResolver{service: struct{}{}}); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	services := p.PublishedServices()
	provider, ok := services[ServiceName].(pluginapi.PaymentProvider)
	if !ok || provider != p {
		t.Fatalf("published provider = %#v, want plugin", services[ServiceName])
	}
}

func TestInitValidatesConfigurationAndDependency(t *testing.T) {
	tests := []struct {
		name     string
		config   pluginapi.RawConfig
		resolver pluginapi.ServiceResolver
		wantErr  bool
	}{
		{
			name:    "missing merchant",
			config:  pluginapi.RawConfig{"secret": "secret"},
			wantErr: true,
		},
		{
			name:    "wrong secret type",
			config:  pluginapi.RawConfig{"merchant_id": "merchant", "secret": 1},
			wantErr: true,
		},
		{
			name:     "resolver error",
			config:   validConfig(),
			resolver: fakeResolver{err: errors.New("not published")},
			wantErr:  true,
		},
		{
			name:     "nil recorder",
			config:   validConfig(),
			resolver: fakeResolver{},
			wantErr:  true,
		},
		{
			name:     "success without direct resolver",
			config:   validConfig(),
			resolver: nil,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			p := NewWithGateway(&fakeGateway{})
			err := p.Init(context.Background(), test.config, test.resolver)
			if (err != nil) != test.wantErr {
				t.Fatalf("Init() error = %v, want error=%v", err, test.wantErr)
			}
		})
	}
}

func TestCreateIntentDelegatesToGateway(t *testing.T) {
	gateway := &fakeGateway{createResult: GatewayPaymentResult{
		ExternalID:  "transaction-42",
		PaymentURL:  "https://pay.example.test/transaction-42",
		RawResponse: map[string]any{"provider_request_id": "abc"},
	}}
	p := NewWithGateway(gateway)
	if err := p.Init(context.Background(), validConfig(), nil); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	result, err := p.CreateIntent(context.Background(), pluginapi.PaymentIntentRequest{
		UserID:      "user-1",
		Amount:      199,
		Currency:    "rub",
		Description: "Subscription",
		ExternalRef: "payment-17",
		CustomData: map[string]any{
			"user_name":  "Buyer",
			"failed_url": "https://example.test/failure",
		},
	})
	if err != nil {
		t.Fatalf("CreateIntent() error = %v", err)
	}
	if result.ExternalID != "transaction-42" || result.PaymentURL != "https://pay.example.test/transaction-42" {
		t.Fatalf("CreateIntent() result = %#v", result)
	}
	if result.RawResponse["external_ref"] != "payment-17" {
		t.Fatalf("RawResponse external_ref = %#v", result.RawResponse)
	}

	got := gateway.createRequest
	if got.MerchantID != "merchant-1" || got.Secret != "shared-secret" || got.UserID != "user-1" {
		t.Fatalf("gateway request credentials/user = %#v", got)
	}
	if got.Currency != "RUB" || got.UserName != "Buyer" || got.FailedURL != "https://example.test/failure" {
		t.Fatalf("gateway request details = %#v", got)
	}
	if got.ReturnURL != "https://example.test/success" || got.ExternalRef != "payment-17" {
		t.Fatalf("gateway request URLs/ref = %#v", got)
	}
}

func TestCreateIntentRejectsInvalidRequest(t *testing.T) {
	p := NewWithGateway(&fakeGateway{})
	if err := p.Init(context.Background(), validConfig(), nil); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	for _, req := range []pluginapi.PaymentIntentRequest{
		{Amount: 1, Currency: "RUB"},
		{UserID: "user", Amount: 0, Currency: "RUB"},
		{UserID: "user", Amount: 1, Currency: "USD"},
	} {
		if _, err := p.CreateIntent(context.Background(), req); err == nil {
			t.Fatalf("CreateIntent(%#v) unexpectedly succeeded", req)
		}
	}
}

func TestVerifyCallbackAuthenticatesAndNormalizes(t *testing.T) {
	p := NewWithGateway(&fakeGateway{})
	if err := p.Init(context.Background(), validConfig(), nil); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/callback", strings.NewReader(`{
		"transactionId":"transaction-42",
		"status":"SUCCESS",
		"amount":199,
		"paymentDetails":{"currency":"rub"}
	}`))
	req.Header.Set("X-Secret", "shared-secret")

	result, err := p.VerifyCallback(context.Background(), req)
	if err != nil {
		t.Fatalf("VerifyCallback() error = %v", err)
	}
	if result.ExternalID != "transaction-42" || result.Status != "completed" || result.Amount != 199 || result.Currency != "RUB" {
		t.Fatalf("VerifyCallback() = %#v", result)
	}
	if result.CustomData["status"] != "SUCCESS" {
		t.Fatalf("raw callback data missing: %#v", result.CustomData)
	}

	invalid := httptest.NewRequest(http.MethodPost, "/callback", strings.NewReader(`{"id":"transaction-42","status":"completed"}`))
	invalid.Header.Set("X-Secret", "wrong-secret")
	if _, err := p.VerifyCallback(context.Background(), invalid); err == nil {
		t.Fatal("VerifyCallback() accepted an invalid secret")
	}
}

func TestRefundDelegatesAndLegacyAdapterReportsUnsupported(t *testing.T) {
	gateway := &fakeGateway{}
	p := NewWithGateway(gateway)
	if err := p.Init(context.Background(), validConfig(), nil); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.Refund(context.Background(), "transaction-42", 99); err != nil {
		t.Fatalf("Refund() error = %v", err)
	}
	if gateway.refundExternalID != "transaction-42" || gateway.refundAmount != 99 {
		t.Fatalf("gateway refund = %q, %d", gateway.refundExternalID, gateway.refundAmount)
	}

	legacy := New()
	if err := legacy.Init(context.Background(), validConfig(), nil); err != nil {
		t.Fatalf("legacy Init() error = %v", err)
	}
	if err := legacy.Refund(context.Background(), "transaction-42", 99); !errors.Is(err, ErrRefundUnsupported) {
		t.Fatalf("legacy Refund() error = %v, want ErrRefundUnsupported", err)
	}
}
