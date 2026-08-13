package engine_xray

import (
	json "github.com/goccy/go-json"
	"testing"
)

func TestParseStats(t *testing.T) {
	respJSON := []byte(`{
		"stat": [
			{
				"name": "user>>>test@example.com>>>traffic>>>uplink",
				"value": 1000
			},
			{
				"name": "user>>>test@example.com>>>traffic>>>downlink",
				"value": 2000
			},
			{
				"name": "user>>>another@example.com>>>traffic>>>uplink",
				"value": 500
			},
			{
				"name": "inbound>>>vless-tcp>>>traffic>>>uplink",
				"value": 9999
			},
			{
				"name": "user>>>>>>traffic>>>uplink",
				"value": 111
			},
			{
				"name": "short",
				"value": 222
			}
		]
	}`)

	stats, err := parseStats(respJSON)
	if err != nil {
		t.Fatalf("parseStats error: %v", err)
	}

	if len(stats) != 2 {
		t.Fatalf("Expected 2 user stats, got %d", len(stats))
	}

	for _, s := range stats {
		switch s.Email {
		case "test@example.com":
			if s.Up != 1000 || s.Down != 2000 {
				t.Errorf("test@example.com unexpected values: up=%d down=%d", s.Up, s.Down)
			}
		case "another@example.com":
			if s.Up != 500 || s.Down != 0 {
				t.Errorf("another@example.com unexpected values: up=%d down=%d", s.Up, s.Down)
			}
		default:
			t.Errorf("Unexpected email: %s", s.Email)
		}
	}
}

func TestParseStatsSkipsUnknownTrafficDirection(t *testing.T) {
	stats, err := parseStats([]byte(`{
  "stat": [
    {"name": "user>>>ignored@example.test>>>traffic>>>sideways", "value": 99},
    {"name": "user>>>valid@example.test>>>traffic>>>downlink", "value": 42}
  ]
}`))
	if err != nil {
		t.Fatalf("parseStats: %v", err)
	}
	if len(stats) != 1 {
		t.Fatalf("stats count = %d, want 1: %#v", len(stats), stats)
	}
	if stats[0] != (UserStat{Email: "valid@example.test", Down: 42}) {
		t.Fatalf("stats = %#v, want valid downlink only", stats)
	}
}

func TestBuildAddPayload(t *testing.T) {
	clientJSON := []byte(`{"id": "uuid-1", "email": "test@example.com"}`)
	var rc RawClient
	if err := json.Unmarshal(clientJSON, &rc); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}

	payload := []TaggedClient{
		{Tag: "tag1", Client: rc},
	}

	// With a non-existent configPath, it should default to using "clients"
	apiPld := buildAddPayload(payload, "nonexistent.json")

	if len(apiPld.Inbounds) != 1 {
		t.Fatalf("Expected 1 inbound, got %d", len(apiPld.Inbounds))
	}

	aib := apiPld.Inbounds[0]
	if aib.Tag != "tag1" {
		t.Errorf("Expected tag tag1, got %s", aib.Tag)
	}

	if aib.Protocol != "" {
		t.Errorf("Expected empty protocol, got %s", aib.Protocol)
	}

	if len(aib.Settings) != 1 {
		t.Errorf("Expected 1 setting, got %v", aib.Settings)
	}

	clientsData, ok := aib.Settings["clients"]
	if !ok {
		t.Fatalf("Expected clients in settings, got %v", aib.Settings)
	}

	var parsedClients []RawClient
	if err := json.Unmarshal(clientsData, &parsedClients); err != nil {
		t.Fatalf("Failed to parse clients: %v", err)
	}

	if len(parsedClients) != 1 {
		t.Fatalf("Expected 1 client in array, got %d", len(parsedClients))
	}

	if parsedClients[0].Email() != "test@example.com" {
		t.Errorf("Expected test@example.com, got %s", parsedClients[0].Email())
	}
}
