// Package payment_platega exposes the legacy Platega HTTP integration through
// the payment-provider plugin contract.
package payment_platega

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrRefundUnsupported is returned by the legacy gateway adapter. The former
// Platega integration only exposes transaction creation, so inventing a refund
// HTTP endpoint here would be unsafe. Installations that need refunds can pass
// a Gateway backed by an official Platega SDK to NewWithGateway without changing
// the plugin or the core payment flow.
var ErrRefundUnsupported = errors.New("payment_platega: refunds are not supported by the legacy Platega client")

// Gateway is the small provider-specific boundary used by Plugin. It makes the
// old package's function-based API testable and is the integration point for a
// future Platega SDK that supports context-aware requests and refunds.
type Gateway interface {
	CreatePayment(ctx context.Context, req GatewayPaymentRequest) (GatewayPaymentResult, error)
	Refund(ctx context.Context, externalID string, amount int) error
}

// GatewayPaymentRequest contains the subset of a payment intent understood by
// Platega. Amount is deliberately passed through unchanged: that preserves the
// units used by the existing internal/payment/platega client and legacy routes.
type GatewayPaymentRequest struct {
	MerchantID  string
	Secret      string
	UserID      string
	UserName    string
	Amount      int
	Currency    string
	Description string
	ReturnURL   string
	FailedURL   string
	ExternalRef string
}

// GatewayPaymentResult is the provider-specific result converted to the
// transport-neutral pluginapi.PaymentIntentResult by Plugin.CreateIntent.
type GatewayPaymentResult struct {
	ExternalID  string
	PaymentURL  string
	RawResponse map[string]any
}

// legacyGateway delegates to the existing Platega package. That package has a
// fixed RUB request format and no cancellation-aware API, therefore validation
// is performed in Plugin before this adapter is invoked.
type legacyGateway struct{}

func (legacyGateway) CreatePayment(_ context.Context, req GatewayPaymentRequest) (GatewayPaymentResult, error) {
	if currency := strings.ToUpper(strings.TrimSpace(req.Currency)); currency != "" && currency != "RUB" {
		return GatewayPaymentResult{}, fmt.Errorf("payment_platega: legacy gateway supports RUB only, got %q", req.Currency)
	}

	url, externalID, err := CreatePayment(
		req.MerchantID,
		req.Secret,
		req.UserID,
		req.UserName,
		req.Amount,
		req.Description,
		req.ReturnURL,
		req.FailedURL,
	)
	if err != nil {
		return GatewayPaymentResult{}, err
	}

	return GatewayPaymentResult{
		ExternalID: externalID,
		PaymentURL: url,
		RawResponse: map[string]any{
			"gateway": "platega",
		},
	}, nil
}

func (legacyGateway) Refund(_ context.Context, _ string, _ int) error {
	return ErrRefundUnsupported
}
