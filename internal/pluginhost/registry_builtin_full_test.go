//go:build !minimal

package pluginhost

import "testing"

func TestBuiltinRegistryIncludesDefaultBuiltinPlugins(t *testing.T) {
	t.Parallel()

	registry := BuiltinRegistry(nil)
	want := []string{
		"core",
		"pricing_default",
		"engine_xray",
		
		"antifraud",
		"mailer_resend",
		"eventsink_webhook",
		"payment_platega",
		
	}

	if len(registry) != len(want) {
		t.Fatalf("BuiltinRegistry returned %d plugins, want %d: %#v", len(registry), len(want), registry)
	}
	for _, name := range want {
		if registry[name] == nil {
			t.Errorf("BuiltinRegistry does not register %q", name)
		}
	}
}
