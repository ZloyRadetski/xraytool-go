package subscription_autobalancer

import (
	"context"
	stdjson "encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"xraytool/internal/plugins/subscription_format_legacy/convert"
)

func TestPluginProcessesV2AndSeparatesPortableEndpoints(t *testing.T) {
	input := `{
  "version": 2,
  "servers": {
    "nl": {"name":"Netherlands","outbound":{"protocol":"vless","settings":{"vnext":[{"address":"nl.example.com","port":443,"users":[{"id":"12345678-1234-1234-1234-123456789012","encryption":"none"}]}]}}},
    "de": {"name":"Germany","outbound":{"protocol":"vless","settings":{"vnext":[{"address":"de.example.com","port":443,"users":[{"id":"12345678-1234-1234-1234-123456789012","encryption":"none"}]}]}}},
    "fi": {"name":"Finland","outbound":{"protocol":"vless","settings":{"vnext":[{"address":"fi.example.com","port":443,"users":[{"id":"12345678-1234-1234-1234-123456789012","encryption":"none"}]}]}}}
  },
  "subscription": [
    {"type":"server","ref":"nl"},
    {"type":"server","ref":"de"},
    {"type":"auto_balancer","id":"eu","name":"Europe auto","members":[{"ref":"nl"},{"ref":"de"}]},
    {"type":"auto_balancer","id":"north","name":"North auto","members":[{"ref":"nl"},{"ref":"fi"}]}
  ]
}`

	result, err := New().ProcessSubscriptionTemplate(context.Background(), input)
	if err != nil {
		t.Fatalf("ProcessSubscriptionTemplate: %v", err)
	}
	if !result.Handled {
		t.Fatal("v2 source was not handled")
	}
	var delivery []map[string]any
	if err := stdjson.Unmarshal([]byte(result.JSONConfig), &delivery); err != nil {
		t.Fatal(err)
	}
	if len(delivery) != 4 {
		t.Fatalf("delivery profiles = %d, want 4", len(delivery))
	}
	var portable []map[string]any
	if err := stdjson.Unmarshal([]byte(result.ExportJSONConfig), &portable); err != nil {
		t.Fatal(err)
	}
	if len(portable) != 3 {
		t.Fatalf("portable profiles = %d, want normal NL/DE plus external FI", len(portable))
	}
	links, err := convert.XrayJSONToShareText(result.ExportJSONConfig)
	if err != nil {
		t.Fatalf("VLESS export: %v", err)
	}
	if got := len(nonEmptyLines(links)); got != 3 {
		t.Fatalf("VLESS nodes = %d, want 3: %q", got, links)
	}
	clash, err := convert.XrayJSONToClashYAML(result.ExportJSONConfig)
	if err != nil {
		t.Fatalf("Clash export: %v", err)
	}
	var clashDocument map[string]any
	if err := yaml.Unmarshal([]byte(clash), &clashDocument); err != nil {
		t.Fatalf("parse Clash YAML: %v", err)
	}
	if proxies, ok := clashDocument["proxies"].([]any); !ok || len(proxies) != 3 {
		t.Fatalf("Clash proxies = %#v, want 3", clashDocument["proxies"])
	}
}

func nonEmptyLines(value string) []string {
	var result []string
	for _, line := range strings.Split(value, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			result = append(result, line)
		}
	}
	return result
}

func TestPluginLeavesLegacyTemplateUntouched(t *testing.T) {
	result, err := New().ProcessSubscriptionTemplate(context.Background(), `[{"outbounds":[]}]`)
	if err != nil {
		t.Fatal(err)
	}
	if result.Handled {
		t.Fatal("legacy JSON must stay outside the auto-balancer plugin")
	}
}
