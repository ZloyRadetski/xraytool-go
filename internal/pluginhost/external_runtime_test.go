package pluginhost_test

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"xraytool/internal/pluginapi"
	"xraytool/internal/pluginhost"
	"xraytool/pluginrpc"
)

// TestExternalPluginHelperProcess is run in a subprocess by go-plugin. It is
// intentionally a test-only executable: Phase 4 tests exercise the real
// handshake, gRPC service and restart path without depending on a production
// payment/antifraud binary.
func TestExternalPluginHelperProcess(t *testing.T) {
	if os.Getenv("XRAYTOOL_EXTERNAL_PLUGIN_HELPER") != "1" {
		return
	}
	pluginrpc.Serve(&externalPluginHelper{})
}

type externalPluginHelper struct{}

func (externalPluginHelper) Describe(context.Context) (pluginrpc.Metadata, error) {
	mode := os.Getenv("XRAYTOOL_EXTERNAL_PLUGIN_MODE")
	metadata := pluginrpc.Metadata{
		Name:         "remote_pricing",
		Kind:         "pricing",
		Version:      "test",
		APIVersion:   "1",
		Description:  "test-only external pricing plugin",
		Publishes:    []pluginrpc.ServiceRef{{Name: "pricing_engine"}, {Name: "payment_provider.test_gateway"}},
		Requires:     []pluginrpc.ServiceRef{{Name: "echo"}},
		Capabilities: map[string]any{"method_id": "test_gateway"},
	}
	if mode == "unsupported-publication" {
		metadata.Publishes = []pluginrpc.ServiceRef{{Name: "subscription_repository"}}
	}
	if mode == "unsupported-requirement" {
		metadata.Requires = []pluginrpc.ServiceRef{{Name: "non_serializable"}}
	}
	return metadata, nil
}

func (externalPluginHelper) Init(ctx context.Context, request pluginrpc.InitRequest) error {
	if os.Getenv("XRAYTOOL_EXTERNAL_PLUGIN_MODE") == "unsupported-requirement" {
		return nil // Host must reject the local non-serializable service first.
	}
	if request.Services == nil {
		return errors.New("test external plugin did not receive ServiceProxy")
	}
	response, err := request.Services.Call(ctx, pluginrpc.CallRequest{
		Service: "echo", Method: "ping", Payload: map[string]any{"value": "from-external"},
	})
	if err != nil {
		return err
	}
	if response.Payload["value"] != "from-host" {
		return errors.New("test external plugin received invalid ServiceProxy response")
	}
	return nil
}

func (externalPluginHelper) Start(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

func (externalPluginHelper) Stop(context.Context) error   { return nil }
func (externalPluginHelper) Health(context.Context) error { return nil }

func (externalPluginHelper) Call(_ context.Context, request pluginrpc.CallRequest) (pluginrpc.CallResponse, error) {
	if request.Service == "pricing_engine" && request.Method == "calculate_price" {
		return pluginrpc.CallResponse{Payload: map[string]any{
			"final_price":      777,
			"discount_percent": 0,
			"description":      "external-test-price",
		}}, nil
	}
	if request.Service == "payment_provider" {
		switch request.Method {
		case "create_intent":
			return pluginrpc.CallResponse{Payload: map[string]any{
				"external_id": "ext-123", "payment_url": "https://pay.example/ext-123",
			}}, nil
		case "verify_callback":
			if request.Payload["method"] != http.MethodPost || request.Payload["path"] != "/callback" {
				return pluginrpc.CallResponse{}, errors.New("callback method/path was not serialized")
			}
			encodedBody, ok := request.Payload["body_base64"].(string)
			if !ok {
				return pluginrpc.CallResponse{}, errors.New("callback body encoding is missing")
			}
			body, err := base64.StdEncoding.DecodeString(encodedBody)
			if err != nil || string(body) != "signed-body" {
				return pluginrpc.CallResponse{}, errors.New("callback body was not serialized")
			}
			return pluginrpc.CallResponse{Payload: map[string]any{
				"external_id": "ext-123", "status": "completed", "amount": 777, "currency": "RUB",
			}}, nil
		case "refund":
			return pluginrpc.CallResponse{}, nil
		}
	}
	return pluginrpc.CallResponse{}, errors.New("unsupported test external method")
}

func TestHostLoadExternalPlugin_ServiceProxyAndRestart(t *testing.T) {
	t.Setenv("XRAYTOOL_EXTERNAL_PLUGIN_HELPER", "1")
	t.Setenv("XRAYTOOL_EXTERNAL_PLUGIN_MODE", "")

	core := externalTestCore(map[string]any{
		"echo": pluginrpc.ServiceHandlerFunc(func(_ context.Context, request pluginrpc.CallRequest) (pluginrpc.CallResponse, error) {
			if request.Method != "ping" || request.Payload["value"] != "from-external" {
				return pluginrpc.CallResponse{}, errors.New("unexpected reverse ServiceProxy request")
			}
			return pluginrpc.CallResponse{Payload: map[string]any{"value": "from-host"}}, nil
		}),
	})
	host := newExternalTestHost(core, pluginhost.RestartPolicy{MaxRestarts: 1})

	loadCtx, cancelLoad := context.WithCancel(context.Background())
	defer cancelLoad()
	require.NoError(t, host.Load(loadCtx))
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		assert.NoError(t, host.Shutdown(shutdownCtx))
	})

	service, err := host.ResolveService("pricing_engine")
	require.NoError(t, err)
	pricing, ok := service.(pluginapi.PricingEngine)
	require.True(t, ok)
	result, err := pricing.CalculatePrice(context.Background(), pluginapi.PricingRequest{Amount: 100})
	require.NoError(t, err)
	assert.Equal(t, 777, result.FinalPrice)
	assert.Equal(t, "external-test-price", result.Description)
	payment := host.PaymentProviders()["test_gateway"]
	require.NotNil(t, payment)
	intent, err := payment.CreateIntent(context.Background(), pluginapi.PaymentIntentRequest{Amount: 777, Currency: "RUB"})
	require.NoError(t, err)
	assert.Equal(t, "ext-123", intent.ExternalID)
	callback := httptest.NewRequest(http.MethodPost, "https://host.example/callback", strings.NewReader("signed-body"))
	callback.Header.Set("X-Signature", "test")
	callbackResult, err := payment.VerifyCallback(context.Background(), callback)
	require.NoError(t, err)
	assert.Equal(t, "completed", callbackResult.Status)
	bodyAfter, err := io.ReadAll(callback.Body)
	require.NoError(t, err)
	assert.Equal(t, "signed-body", string(bodyAfter))

	restartCtx, cancelRestart := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelRestart()
	require.NoError(t, host.Restart(restartCtx, "remote_pricing"))
	result, err = pricing.CalculatePrice(context.Background(), pluginapi.PricingRequest{Amount: 100})
	require.NoError(t, err)
	assert.Equal(t, 777, result.FinalPrice)

	err = host.Restart(restartCtx, "remote_pricing")
	require.Error(t, err)
	assert.ErrorContains(t, err, "restart limit exhausted")
}

func TestHostLoadExternalPlugin_RejectsUnsupportedTransportServices(t *testing.T) {
	t.Run("publication", func(t *testing.T) {
		t.Setenv("XRAYTOOL_EXTERNAL_PLUGIN_HELPER", "1")
		t.Setenv("XRAYTOOL_EXTERNAL_PLUGIN_MODE", "unsupported-publication")
		host := newExternalTestHost(externalTestCore(map[string]any{"echo": pluginrpc.ServiceHandlerFunc(func(context.Context, pluginrpc.CallRequest) (pluginrpc.CallResponse, error) {
			return pluginrpc.CallResponse{}, nil
		})}), pluginhost.RestartPolicy{})
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := host.Load(ctx)
		require.Error(t, err)
		assert.ErrorContains(t, err, "cannot publish service \"subscription_repository\"")
		assert.ErrorContains(t, err, "not RPC-compatible")
	})

	t.Run("requirement", func(t *testing.T) {
		t.Setenv("XRAYTOOL_EXTERNAL_PLUGIN_HELPER", "1")
		t.Setenv("XRAYTOOL_EXTERNAL_PLUGIN_MODE", "unsupported-requirement")
		host := newExternalTestHost(externalTestCore(map[string]any{
			"echo": pluginrpc.ServiceHandlerFunc(func(context.Context, pluginrpc.CallRequest) (pluginrpc.CallResponse, error) {
				return pluginrpc.CallResponse{}, nil
			}),
			"non_serializable": struct{}{},
		}), pluginhost.RestartPolicy{})
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := host.Load(ctx)
		require.Error(t, err)
		assert.ErrorContains(t, err, "requires service \"non_serializable\"")
		assert.ErrorContains(t, err, "not serializable over the external plugin RPC transport")
	})
}

func newExternalTestHost(core pluginapi.Plugin, policy pluginhost.RestartPolicy) *pluginhost.Host {
	return pluginhost.New(pluginhost.PluginsConfig{
		"core": {Enabled: true, Source: "builtin"},
		"remote_pricing": {
			Enabled:       true,
			Source:        "external",
			Exec:          os.Args[0],
			Args:          []string{"-test.run=^TestExternalPluginHelperProcess$"},
			RestartPolicy: policy,
		},
	}, nil, map[string]func() pluginapi.Plugin{
		"core": func() pluginapi.Plugin { return core },
	}, nil)
}

func externalTestCore(services map[string]any) pluginapi.Plugin {
	publications := make([]pluginapi.ServiceRef, 0, len(services))
	for name := range services {
		publications = append(publications, pluginapi.ServiceRef{Name: name})
	}
	return &externalCoreStub{
		metadata: pluginapi.Metadata{
			Name: "core", Kind: "core", Mandatory: true, APIVersion: "1", Publishes: publications,
		},
		services: services,
	}
}

type externalCoreStub struct {
	metadata pluginapi.Metadata
	services map[string]any
}

func (p *externalCoreStub) Metadata() pluginapi.Metadata { return p.metadata }
func (p *externalCoreStub) Init(context.Context, pluginapi.RawConfig, pluginapi.ServiceResolver) error {
	return nil
}
func (p *externalCoreStub) Start(ctx context.Context) error { <-ctx.Done(); return nil }
func (*externalCoreStub) Stop(context.Context) error        { return nil }
func (*externalCoreStub) Health(context.Context) error      { return nil }
func (p *externalCoreStub) PublishedServices() map[string]any {
	return p.services
}

var _ pluginapi.ServiceProvider = (*externalCoreStub)(nil)
