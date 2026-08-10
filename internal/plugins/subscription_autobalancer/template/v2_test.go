package template

import (
	stdjson "encoding/json"
	"strings"
	"testing"
)

const vlessOutbound = `{
  "protocol": "vless",
  "tag": "source-tag",
  "settings": {"vnext": [{"address": "vpn.example.com", "port": 443, "users": [{"id": "12345678-1234-1234-1234-123456789012", "encryption": "none"}]}]},
  "streamSettings": {"network": "tcp", "security": "reality", "realitySettings": {"serverName": "www.example.com", "publicKey": "key"}}
}`

func TestCompileV2BuildsNativeProfiles(t *testing.T) {
	input := `{
  "version": 2,
  "servers": {
    "nl-1": {"name": "Netherlands", "outbound": ` + vlessOutbound + `},
    "de-1": {"name": "Germany", "outbound": ` + vlessOutbound + `}
  },
  "subscription": [
    {"type": "server", "ref": "nl-1"},
    {"type": "server", "ref": "de-1"},
    {"type": "auto_balancer", "id": "eu-auto", "name": "Europe Auto", "members": [{"ref": "nl-1"}, {"ref": "de-1"}]}
  ]
}`

	result, err := Compile(input)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if !result.IsV2 || result.ProfileCount != 3 || result.BalancerCount != 1 {
		t.Fatalf("unexpected result: %#v", result)
	}

	var profiles []map[string]any
	if err := stdjson.Unmarshal([]byte(result.JSON), &profiles); err != nil {
		t.Fatalf("compiled JSON: %v", err)
	}
	if len(profiles) != 3 {
		t.Fatalf("profiles = %d, want 3", len(profiles))
	}
	balancer := profiles[2]
	if _, exists := balancer["version"]; exists {
		t.Fatalf("v2 source fields must not reach the client JSON: %#v", balancer)
	}
	if balancer["remarks"] != "Europe Auto" {
		t.Fatalf("unexpected balancer remarks: %#v", balancer["remarks"])
	}
	routing, ok := balancer["routing"].(map[string]any)
	if !ok || len(routing["balancers"].([]any)) != 1 {
		t.Fatalf("native routing balancer was not generated: %#v", balancer)
	}
	outbounds := balancer["outbounds"].([]any)
	if len(outbounds) != 3 {
		t.Fatalf("balancer outbounds = %d, want 3 including direct", len(outbounds))
	}
	first := outbounds[0].(map[string]any)
	if first["tag"] == "source-tag" || !strings.HasPrefix(first["tag"].(string), "ab_eu_auto_nl_1_") {
		t.Fatalf("member tag was not isolated: %#v", first["tag"])
	}
}

func TestCompileV2IncludesInlineBalancerMembers(t *testing.T) {
	input := `{
  "version": 2,
  "servers": {"nl-1": {"name": "Netherlands", "outbound": ` + vlessOutbound + `}},
  "subscription": [
    {"type": "auto_balancer", "id": "backup", "name": "Backup", "members": [
      {"ref": "nl-1"},
      {"server": {"id": "fi-1", "name": "Finland", "outbound": ` + vlessOutbound + `}}
    ]}
  ]
}`
	result, err := Compile(input)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if result.ProfileCount != 1 || result.BalancerCount != 1 {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestCompileV2AllowsBalancerWithOnlyInlineServers(t *testing.T) {
	input := `{
  "version": 2,
  "subscription": [
    {"type": "auto_balancer", "id": "inline", "name": "Inline", "members": [
      {"server": {"id": "fi-1", "name": "Finland", "outbound": ` + vlessOutbound + `}},
      {"server": {"id": "se-1", "name": "Sweden", "outbound": ` + vlessOutbound + `}}
    ]}
  ]
}`
	result, err := Compile(input)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if !result.IsV2 || result.ProfileCount != 1 {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestCompileV2RejectsInvalidReferencesAndAmbiguousConfig(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "missing ref",
			input: `{"version":2,"servers":{"one":{"name":"One","outbound":` + vlessOutbound + `}},"subscription":[{"type":"auto_balancer","id":"auto","name":"Auto","members":[{"ref":"one"},{"ref":"missing"}]}]}`,
			want:  "unknown server",
		},
		{
			name:  "ambiguous config endpoint",
			input: `{"version":2,"servers":{"one":{"name":"One","config":{"outbounds":[` + vlessOutbound + `,` + vlessOutbound + `]}},"two":{"name":"Two","outbound":` + vlessOutbound + `}},"subscription":[{"type":"auto_balancer","id":"auto","name":"Auto","members":[{"ref":"one"},{"ref":"two"}]}]}`,
			want:  "multiple proxy outbounds",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Compile(test.input)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Compile error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestCompileLeavesLegacyInputUntouched(t *testing.T) {
	result, err := Compile(`[{"outbounds":[]}]`)
	if err != nil {
		t.Fatalf("Compile legacy: %v", err)
	}
	if result.IsV2 {
		t.Fatal("legacy input must not be treated as v2")
	}

	result, err = Compile(`{"version":2,"outbounds":[]}`)
	if err != nil {
		t.Fatalf("Compile ordinary config: %v", err)
	}
	if result.IsV2 {
		t.Fatal("ordinary JSON config must not be treated as v2")
	}
}
