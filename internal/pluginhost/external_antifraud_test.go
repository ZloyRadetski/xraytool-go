package pluginhost_test

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"xraytool/internal/pluginapi"
	"xraytool/internal/pluginhost"
	"xraytool/pluginrpc"
)

// TestExternalAntifraudHelperProcess is a real go-plugin subprocess. It uses
// the brokered ban_update_sink during Init, before Host installs its sink, to
// verify that the adapter replays recovered bans into Host.BanCache.
func TestExternalAntifraudHelperProcess(t *testing.T) {
	if os.Getenv("XRAYTOOL_EXTERNAL_ANTIFRAUD_HELPER") != "1" {
		return
	}
	pluginrpc.Serve(&externalAntifraudHelper{})
}

type externalAntifraudHelper struct {
	mu sync.Mutex

	services      pluginrpc.ServiceCaller
	providerCalls int
	sourceID      string
	eventCount    int
}

func (*externalAntifraudHelper) Describe(context.Context) (pluginrpc.Metadata, error) {
	return pluginrpc.Metadata{
		Name:        "remote_antifraud",
		Kind:        "antifraud",
		Version:     "test",
		APIVersion:  "1",
		Description: "test-only external antifraud provider",
		Publishes:   []pluginrpc.ServiceRef{{Name: "antifraud_provider"}},
	}, nil
}

func (h *externalAntifraudHelper) Init(ctx context.Context, request pluginrpc.InitRequest) error {
	if request.Services == nil {
		return errors.New("external antifraud did not receive the ban update sink")
	}
	h.mu.Lock()
	h.services = request.Services
	h.mu.Unlock()
	_, err := request.Services.Call(ctx, pluginrpc.CallRequest{
		Service: "ban_update_sink",
		Method:  "push_ban_update",
		Payload: map[string]any{
			"email":        "recovered@example.test",
			"banned_until": time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano),
		},
	})
	return err
}

func (*externalAntifraudHelper) Start(ctx context.Context) error { <-ctx.Done(); return nil }
func (*externalAntifraudHelper) Stop(context.Context) error      { return nil }
func (*externalAntifraudHelper) Health(context.Context) error    { return nil }

func (h *externalAntifraudHelper) Call(ctx context.Context, request pluginrpc.CallRequest) (pluginrpc.CallResponse, error) {
	if request.Service != "antifraud_provider" {
		return pluginrpc.CallResponse{}, errors.New("unexpected external antifraud service")
	}
	h.mu.Lock()
	h.providerCalls++
	calls := h.providerCalls
	services := h.services
	h.mu.Unlock()

	switch request.Method {
	case "force_unban":
		email, _ := request.Payload["email"].(string)
		if email == "" {
			return pluginrpc.CallResponse{}, errors.New("force_unban did not include email")
		}
		if services == nil {
			return pluginrpc.CallResponse{}, errors.New("ban update sink is unavailable")
		}
		_, err := services.Call(ctx, pluginrpc.CallRequest{
			Service: "ban_update_sink",
			Method:  "push_unban",
			Payload: map[string]any{"email": email},
		})
		return pluginrpc.CallResponse{}, err
	case "snapshot":
		h.mu.Lock()
		sourceID, eventCount := h.sourceID, h.eventCount
		h.mu.Unlock()
		return pluginrpc.CallResponse{Payload: map[string]any{
			"provider_calls": calls,
			"source_id":      sourceID,
			"event_count":    eventCount,
		}}, nil
	case "ingest_events":
		sourceID, _ := request.Payload["source_id"].(string)
		events, ok := request.Payload["events"].([]any)
		if !ok {
			return pluginrpc.CallResponse{}, errors.New("ingest_events did not receive an events array")
		}
		h.mu.Lock()
		h.sourceID = sourceID
		h.eventCount = len(events)
		h.mu.Unlock()
		return pluginrpc.CallResponse{}, nil
	default:
		return pluginrpc.CallResponse{}, errors.New("unsupported external antifraud method")
	}
}

func TestHostLoadExternalAntifraudPushesCacheAndUsesStructuredAdapters(t *testing.T) {
	t.Setenv("XRAYTOOL_EXTERNAL_ANTIFRAUD_HELPER", "1")
	host := pluginhost.New(pluginhost.PluginsConfig{
		"core": {Enabled: true, Source: "builtin"},
		"remote_antifraud": {
			Enabled: true,
			Source:  "external",
			Exec:    os.Args[0],
			Args:    []string{"-test.run=^TestExternalAntifraudHelperProcess$"},
		},
	}, nil, map[string]func() pluginapi.Plugin{
		"core": func() pluginapi.Plugin { return externalTestCore(nil) },
	}, nil)

	loadCtx, cancelLoad := context.WithCancel(context.Background())
	defer cancelLoad()
	require.NoError(t, host.Load(loadCtx))
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		assert.NoError(t, host.Shutdown(shutdownCtx))
	})

	provider, ok := host.Antifraud()
	require.True(t, ok)
	assert.Empty(t, host.PaymentProviders(), "an external antifraud adapter must not be exposed as a payment provider")
	assert.Empty(t, host.EventSinks(), "an external antifraud adapter must not be exposed as an event sink")
	require.True(t, provider.IsBanned("recovered@example.test"))
	require.True(t, host.BanCache().IsBanned("recovered@example.test"))

	// IsBanned is deliberately exercised repeatedly before any provider Call.
	// Snapshot reports exactly one provider call (the snapshot itself), proving
	// the hot path was served by the adapter's local state rather than RPC.
	for range 50 {
		assert.True(t, provider.IsBanned("recovered@example.test"))
	}
	snapshot, err := provider.Snapshot(context.Background())
	require.NoError(t, err)
	assert.Equal(t, float64(1), snapshot["provider_calls"])

	err = provider.IngestEvents(context.Background(), "slave-a", []pluginapi.FraudEvent{
		{Email: "a@example.test", IP: "203.0.113.10"},
		{Email: "b@example.test", IP: "203.0.113.11"},
	})
	require.NoError(t, err)
	snapshot, err = provider.Snapshot(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "slave-a", snapshot["source_id"])
	assert.Equal(t, float64(2), snapshot["event_count"])

	require.NoError(t, provider.ForceUnban(context.Background(), "recovered@example.test"))
	assert.False(t, provider.IsBanned("recovered@example.test"))
	assert.False(t, host.BanCache().IsBanned("recovered@example.test"))
}
