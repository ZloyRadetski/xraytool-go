package pluginhost_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"xraytool/internal/pluginapi"
	"xraytool/internal/pluginhost"
)

// TestExternalSDKPaymentStubInteroperatesWithHost builds the public standalone
// SDK example as its own Go module, then loads its binary using the host's
// production go-plugin descriptor. This catches wire incompatibilities between
// plugins-external/sdk and the in-tree host transport.
func TestExternalSDKPaymentStubInteroperatesWithHost(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a standalone external plugin binary")
	}

	binary := buildExternalSDKPaymentStub(t)
	host := pluginhost.New(pluginhost.PluginsConfig{
		"core": {Enabled: true, Source: "builtin"},
		"payment_stub": {
			Enabled: true,
			Source:  "external",
			Exec:    binary,
		},
	}, nil, map[string]func() pluginapi.Plugin{
		"core": func() pluginapi.Plugin { return &externalSDKInteropCore{} },
	}, nil)

	loadCtx, cancelLoad := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancelLoad()
	require.NoError(t, host.Load(loadCtx))
	t.Cleanup(func() {
		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancelShutdown()
		assert.NoError(t, host.Shutdown(shutdownCtx))
	})

	provider := host.PaymentProviders()["stub"]
	require.NotNil(t, provider)
	intent, err := provider.CreateIntent(context.Background(), pluginapi.PaymentIntentRequest{
		Amount: 123,
		Currency: "RUB",
	})
	require.NoError(t, err)
	assert.Equal(t, "stub-payment-id", intent.ExternalID)
	assert.Equal(t, "https://example.invalid/pay/stub-payment-id", intent.PaymentURL)

	callback := httptest.NewRequest(http.MethodPost, "https://host.example/callback", nil)
	result, err := provider.VerifyCallback(context.Background(), callback)
	require.NoError(t, err)
	assert.Equal(t, "stub-payment-id", result.ExternalID)
	assert.Equal(t, "completed", result.Status)
}

func buildExternalSDKPaymentStub(t *testing.T) string {
	t.Helper()
	_, sourceFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	repositoryRoot := filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "..", ".."))
	externalModule := filepath.Join(repositoryRoot, "plugins-external")
	extension := ""
	if runtime.GOOS == "windows" {
		extension = ".exe"
	}
	binary := filepath.Join(t.TempDir(), "xraytool-plugin-payment-stub"+extension)
	command := exec.Command("go", "build", "-o", binary, "./examples/payment_stub")
	command.Dir = externalModule
	command.Env = os.Environ()
	output, err := command.CombinedOutput()
	require.NoErrorf(t, err, "build standalone SDK payment stub: %s", output)
	return binary
}

type externalSDKInteropCore struct{}

func (*externalSDKInteropCore) Metadata() pluginapi.Metadata {
	return pluginapi.Metadata{
		Name:       "core",
		Kind:       "core",
		Mandatory:  true,
		APIVersion: pluginapi.CurrentAPIVersion,
	}
}

func (*externalSDKInteropCore) Init(context.Context, pluginapi.RawConfig, pluginapi.ServiceResolver) error {
	return nil
}

func (*externalSDKInteropCore) Start(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

func (*externalSDKInteropCore) Stop(context.Context) error   { return nil }
func (*externalSDKInteropCore) Health(context.Context) error { return nil }
