//go:build !minimal

package pluginhost

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"xraytool/internal/appconfig"
	"xraytool/internal/pluginapi"
)

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
		"subscription_format_legacy", "traffic_file", "server_routing",
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

func TestBuiltinRegistryServerRoutingPluginContract(t *testing.T) {
	t.Parallel()

	registry := BuiltinRegistry(nil)
	factory := registry["server_routing"]
	if factory == nil {
		t.Fatal("BuiltinRegistry does not register server_routing")
	}

	plugin := factory()
	metadata := plugin.Metadata()
	if metadata.Name != "server_routing" || metadata.Kind != "admin_tool" || metadata.Version != "1.0.0" || metadata.APIVersion != pluginapi.CurrentAPIVersion || metadata.Mandatory {
		t.Fatalf("unexpected server_routing metadata: %#v", metadata)
	}
	if _, ok := plugin.(pluginapi.HTTPContributor); !ok {
		t.Fatal("server_routing does not implement pluginapi.HTTPContributor")
	}

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg, err := appconfig.Load(configPath)
	if err != nil {
		t.Fatalf("load default config: %v", err)
	}
	pluginCfg, ok := cfg.Plugins["server_routing"]
	if !ok || !pluginCfg.Enabled || pluginCfg.Source != "builtin" || pluginCfg.Config["routing_dir"] != "/root/xraytool/data/routing" {
		t.Fatalf("unexpected default server_routing config: %#v", pluginCfg)
	}

	routingDir := filepath.Join(t.TempDir(), "routing")
	if err := plugin.Init(context.Background(), pluginapi.RawConfig{"routing_dir": routingDir}, builtinRegistryTestResolver{}); err != nil {
		t.Fatalf("initialize server_routing plugin: %v", err)
	}
	for _, path := range []string{routingDir, filepath.Join(routingDir, "outbounds")} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %q: %v", path, err)
		}
		if !info.IsDir() {
			t.Errorf("%q is not a directory", path)
		}
	}
	if err := plugin.Health(context.Background()); err != nil {
		t.Fatalf("server_routing plugin health: %v", err)
	}
}

type builtinRegistryTestResolver struct{}

func (builtinRegistryTestResolver) Resolve(string) (any, error) {
	return nil, errors.New("unexpected service resolution")
}

func (builtinRegistryTestResolver) Logger() pluginapi.Logger {
	return builtinRegistryTestLogger{}
}

func (builtinRegistryTestResolver) EmitEvent(string, map[string]any, map[string]any) {}

func (builtinRegistryTestResolver) DB() pluginapi.PluginDBHandle { return nil }

type builtinRegistryTestLogger struct{}

func (builtinRegistryTestLogger) Debug(string, ...any) {}
func (builtinRegistryTestLogger) Info(string, ...any)  {}
func (builtinRegistryTestLogger) Warn(string, ...any)  {}
func (builtinRegistryTestLogger) Error(string, ...any) {}
