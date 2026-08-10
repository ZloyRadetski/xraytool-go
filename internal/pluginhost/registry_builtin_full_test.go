//go:build !minimal

package pluginhost

import "testing"

func TestBuiltinRegistryIncludesDefaultBuiltinPlugins(t *testing.T) {
	t.Parallel()

	registry := BuiltinRegistry(nil)
	want := []string{
		"core",
		"user_management", "subscription_runtime", "subscription_autobalancer", "api_server",
		"pricing_default",
		"engine_xray",

		"antifraud",
		"mailer_resend",
		"eventsink_webhook",
		"payment_platega", "billing", "promo", "referral", "support_chat",
		"config_storage", "identity_memory", "subscription_lifecycle",
		"subscription_format_legacy", "traffic_file",
	}

	if len(registry) < len(want) {
		t.Fatalf("BuiltinRegistry returned %d plugins, expected at least %d: %#v", len(registry), len(want), registry)
	}
	for _, name := range want {
		if registry[name] == nil {
			t.Errorf("BuiltinRegistry does not register %q", name)
		}
	}
}
