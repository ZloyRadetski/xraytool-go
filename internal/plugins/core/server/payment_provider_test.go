package server_test

import (
	"context"
	"errors"
	json "github.com/goccy/go-json"
	"io"
	"net/http"
	"strings"

	"xraytool/internal/pluginapi"
)

// testPaymentProvider exercises the same generic router boundary used by a
// real gateway plugin without making server tests depend on network credentials.
type testPaymentProvider struct{}

func newTestPaymentProvider() pluginapi.PaymentProvider { return testPaymentProvider{} }

func (testPaymentProvider) Metadata() pluginapi.Metadata {
	return pluginapi.Metadata{Name: "test_platega", Kind: "payment", APIVersion: pluginapi.CurrentAPIVersion}
}

func (testPaymentProvider) Init(context.Context, pluginapi.RawConfig, pluginapi.ServiceResolver) error {
	return nil
}
func (testPaymentProvider) Start(ctx context.Context) error {
	<-ctx.Done()
	return nil
}
func (testPaymentProvider) Stop(context.Context) error   { return nil }
func (testPaymentProvider) Health(context.Context) error { return nil }
func (testPaymentProvider) MethodID() string             { return "platega" }

func (testPaymentProvider) CreateIntent(_ context.Context, req pluginapi.PaymentIntentRequest) (*pluginapi.PaymentIntentResult, error) {
	if strings.TrimSpace(req.UserID) == "" {
		return nil, errors.New("user ID is required")
	}
	externalID := "test-ext-" + req.ExternalRef
	return &pluginapi.PaymentIntentResult{
		ExternalID:  externalID,
		PaymentURL:  "https://payments.example.test/" + externalID,
		RawResponse: map[string]any{"test": true},
	}, nil
}

func (testPaymentProvider) VerifyCallback(_ context.Context, req *http.Request) (*pluginapi.PaymentCallbackResult, error) {
	if req == nil {
		return nil, errors.New("callback request is required")
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	if req.Header.Get("X-Secret") != "test-platega-secret" {
		return nil, errors.New("invalid callback secret")
	}
	externalID, _ := payload["external_id"].(string)
	if externalID == "" {
		externalID, _ = payload["id"].(string)
	}
	status, _ := payload["status"].(string)
	if externalID == "" || status == "" {
		return nil, errors.New("callback result is incomplete")
	}
	return &pluginapi.PaymentCallbackResult{ExternalID: externalID, Status: strings.ToLower(status), CustomData: payload}, nil
}

func (testPaymentProvider) Refund(context.Context, string, int) error {
	return errors.New("refund unsupported in test provider")
}
