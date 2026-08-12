package cmd

import "testing"

func TestBuildBalancerConfig2IncludesFullConfigCompatibility(t *testing.T) {
	config := buildBalancerConfig2([]string{"AT_001", "AT_002"}, []any{
		map[string]any{"protocol": "vless", "tag": "AT_001"},
		map[string]any{"protocol": "vless", "tag": "AT_002"},
	}, "Auto")
	if config.Stats == nil {
		t.Fatal("full config must include stats")
	}
	if config.BurstObservatory.PingConfig.Connectivity != config.BurstObservatory.PingConfig.Destination {
		t.Fatalf("connectivity probe = %q, want %q", config.BurstObservatory.PingConfig.Connectivity, config.BurstObservatory.PingConfig.Destination)
	}
	rule, ok := config.Routing.Rules[0].(map[string]any)
	if !ok || rule["outboundTag"] != "direct" {
		t.Fatalf("first routing rule must bypass DNS directly: %#v", config.Routing.Rules[0])
	}
	if ips, ok := rule["ip"].([]string); !ok || len(ips) != 4 {
		t.Fatalf("DNS bypass IPs = %#v", rule["ip"])
	}
}
