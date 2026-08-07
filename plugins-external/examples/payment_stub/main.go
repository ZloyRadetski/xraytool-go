// payment_stub is a runnable, deliberately tiny reference external plugin.
// It demonstrates the v1 payment_provider transport without containing any
// gateway credentials or production payment logic.
package main

import (
	"context"
	"fmt"

	pluginrpc "github.com/ZloyRadetski/xraytool-go/plugins-external/sdk"
)

func main() {
	pluginrpc.Serve(&paymentStub{})
}

type paymentStub struct{}

func (*paymentStub) Describe(context.Context) (pluginrpc.Metadata, error) {
	return pluginrpc.Metadata{
		Name:        "payment_stub",
		Kind:        "payment",
		Version:     "0.1.0",
		APIVersion:  "1",
		Description: "reference external payment provider",
		Publishes:   []pluginrpc.ServiceRef{{Name: "payment_provider.stub"}},
		Capabilities: map[string]any{
			"method_id": "stub",
		},
	}, nil
}

func (*paymentStub) Init(context.Context, pluginrpc.InitRequest) error { return nil }
func (*paymentStub) Start(ctx context.Context) error                   { <-ctx.Done(); return nil }
func (*paymentStub) Stop(context.Context) error                        { return nil }
func (*paymentStub) Health(context.Context) error                      { return nil }

func (*paymentStub) Call(_ context.Context, request pluginrpc.CallRequest) (pluginrpc.CallResponse, error) {
	if request.Service != "payment_provider" {
		return pluginrpc.CallResponse{}, fmt.Errorf("unsupported service %q", request.Service)
	}
	switch request.Method {
	case "create_intent":
		return pluginrpc.CallResponse{Payload: map[string]any{
			"external_id":  "stub-payment-id",
			"payment_url":  "https://example.invalid/pay/stub-payment-id",
			"raw_response": map[string]any{"provider": "stub"},
		}}, nil
	case "verify_callback":
		return pluginrpc.CallResponse{Payload: map[string]any{
			"external_id": "stub-payment-id",
			"status":      "completed",
			"amount":      0,
			"currency":    "RUB",
		}}, nil
	case "refund":
		return pluginrpc.CallResponse{}, nil
	default:
		return pluginrpc.CallResponse{}, fmt.Errorf("unsupported payment_provider method %q", request.Method)
	}
}
