//go:build minimal

package pluginhost

import "testing"

func TestBuiltinRegistryMinimalExcludesOptionalBuiltinPlugins(t *testing.T) {
	t.Parallel()

	registry := BuiltinRegistry(nil)
	want := map[string]struct{}{
		"core":                      {},
		"user_management":           {},
		"subscription_runtime":      {},
		"subscription_autobalancer": {},
		"api_server":                {},
		"pricing_default":           {},
		"engine_xray":               {},
	}

	if len(registry) != len(want) {
		t.Fatalf("BuiltinRegistry returned %d plugins, want only %d: %#v", len(registry), len(want), registry)
	}
	for name := range want {
		if registry[name] == nil {
			t.Errorf("minimal BuiltinRegistry does not register %q", name)
		}
	}
	for _, name := range []string{"engine_singbox", "antifraud", "mailer_resend", "eventsink_webhook", "payment_platega", "cluster_sync"} {
		if registry[name] != nil {
			t.Errorf("minimal BuiltinRegistry unexpectedly registers %q", name)
		}
	}
}
