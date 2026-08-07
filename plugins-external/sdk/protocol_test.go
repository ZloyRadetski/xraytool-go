package pluginrpc

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

func TestValidateCallRequest(t *testing.T) {
	t.Run("rejects an empty service", func(t *testing.T) {
		err := ValidateCallRequest(CallRequest{Method: "send"})
		if err == nil {
			t.Fatal("ValidateCallRequest accepted an empty service")
		}
	})

	t.Run("rejects an empty method", func(t *testing.T) {
		err := ValidateCallRequest(CallRequest{Service: "notification_provider"})
		if err == nil {
			t.Fatal("ValidateCallRequest accepted an empty method")
		}
	})

	if err := ValidateCallRequest(CallRequest{Service: "payment_provider", Method: "create_intent"}); err != nil {
		t.Fatalf("ValidateCallRequest rejected a valid request: %v", err)
	}
}

func TestExternalServerWireEnvelopes(t *testing.T) {
	impl := sdkTestImplementation{}
	server := &externalServer{impl: impl}

	metadata, err := server.Describe(context.Background(), &emptypb.Empty{})
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	var decodedMetadata Metadata
	if err := fromStruct(metadata, &decodedMetadata); err != nil {
		t.Fatalf("decode Describe response: %v", err)
	}
	if decodedMetadata.Name != "sdk_test" || len(decodedMetadata.Publishes) != 1 || decodedMetadata.Publishes[0].Name != "payment_provider.test" {
		t.Fatalf("unexpected Describe response: %#v", decodedMetadata)
	}

	request, err := toStruct(CallRequest{
		Service: "payment_provider",
		Method:  "create_intent",
		Payload: map[string]any{"amount": 123},
	})
	if err != nil {
		t.Fatalf("encode Call request: %v", err)
	}
	response, err := server.Call(context.Background(), request)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	var decoded CallResponse
	if err := fromStruct(response, &decoded); err != nil {
		t.Fatalf("decode Call response: %v", err)
	}
	if decoded.Payload["external_id"] != "sdk-ext-123" {
		t.Fatalf("unexpected Call response: %#v", decoded.Payload)
	}

	invalid, err := toStruct(CallRequest{Service: "payment_provider"})
	if err != nil {
		t.Fatalf("encode invalid Call request: %v", err)
	}
	_, err = server.Call(context.Background(), invalid)
	if err == nil || status.Code(err) != codes.InvalidArgument {
		t.Fatalf("invalid Call error = %v, want InvalidArgument", err)
	}
}

func TestHealthUsesStructuredUnhealthyResponse(t *testing.T) {
	server := &externalServer{impl: sdkTestImplementation{healthErr: errors.New("backend unavailable")}}
	response, err := server.Health(context.Background(), &emptypb.Empty{})
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if response.AsMap()["healthy"] != false || response.AsMap()["error"] != "backend unavailable" {
		t.Fatalf("unexpected Health response: %#v", response.AsMap())
	}
}

type sdkTestImplementation struct {
	healthErr error
}

func (sdkTestImplementation) Describe(context.Context) (Metadata, error) {
	return Metadata{
		Name:       "sdk_test",
		Kind:       "payment",
		Version:    "test",
		APIVersion: "1",
		Publishes:  []ServiceRef{{Name: "payment_provider.test"}},
	}, nil
}

func (sdkTestImplementation) Init(context.Context, InitRequest) error { return nil }
func (sdkTestImplementation) Start(context.Context) error             { return nil }
func (sdkTestImplementation) Stop(context.Context) error              { return nil }
func (p sdkTestImplementation) Health(context.Context) error          { return p.healthErr }

func (sdkTestImplementation) Call(_ context.Context, request CallRequest) (CallResponse, error) {
	if request.Service != "payment_provider" || request.Method != "create_intent" {
		return CallResponse{}, errors.New("unsupported call")
	}
	return CallResponse{Payload: map[string]any{"external_id": "sdk-ext-123"}}, nil
}
