package xrayconfig

import (
	"math"
	"strings"
	"testing"
)

func TestRawConfig_GetInbounds(t *testing.T) {
	// Missing inbounds
	cfg := RawConfig{}
	ib, err := cfg.GetInbounds()
	if err != nil || ib != nil {
		t.Errorf("expected nil, nil for missing inbounds, got %v, %v", ib, err)
	}

	// Unmarshal error
	cfg = RawConfig{"inbounds": []byte(`[{"tag": "test"}`)} // malformed JSON
	_, err = cfg.GetInbounds()
	if err == nil {
		t.Errorf("expected error for malformed inbounds")
	}

	// Success
	cfg = RawConfig{"inbounds": []byte(`[{"tag": "test"}]`)}
	ib, err = cfg.GetInbounds()
	if err != nil || len(ib) != 1 || ib[0].Tag() != "test" {
		t.Errorf("expected 1 inbound with tag 'test', got %v, err: %v", ib, err)
	}
}

func TestRawConfig_SetInbounds(t *testing.T) {
	cfg := RawConfig{}
	err := cfg.SetInbounds([]RawInbound{{"tag": []byte(`"test"`)}})
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if string(cfg["inbounds"]) != `[{"tag":"test"}]` {
		t.Errorf("unexpected inbounds JSON: %s", string(cfg["inbounds"]))
	}
}

func TestRawInbound_TagAndProtocol(t *testing.T) {
	ib := RawInbound{
		"tag":      []byte(`"my-tag"`),
		"protocol": []byte(`"VLESS"`),
	}
	if ib.Tag() != "my-tag" {
		t.Errorf("expected 'my-tag', got %s", ib.Tag())
	}
	if ib.Protocol() != "vless" {
		t.Errorf("expected 'vless', got %s", ib.Protocol())
	}

	// Missing
	ib2 := RawInbound{}
	if ib2.Tag() != "" {
		t.Errorf("expected empty tag, got %s", ib2.Tag())
	}

	// Invalid type
	ib3 := RawInbound{"tag": []byte(`123`)}
	if ib3.Tag() != "" {
		t.Errorf("expected empty tag for non-string, got %s", ib3.Tag())
	}
}

func TestRawInbound_IsHysteria(t *testing.T) {
	tests := []struct {
		protocol string
		want     bool
	}{
		{"hysteria", true},
		{"HYSTERIA2", true},
		{"hy2", true},
		{"vless", false},
	}
	for _, tt := range tests {
		ib := RawInbound{"protocol": []byte(`"` + tt.protocol + `"`)}
		if ib.IsHysteria() != tt.want {
			t.Errorf("protocol %s IsHysteria() = %v, want %v", tt.protocol, ib.IsHysteria(), tt.want)
		}
	}
}

func TestRawInbound_HasClientListAndClientsKey(t *testing.T) {
	// Parse error in settings
	ibErr := RawInbound{"settings": []byte(`{`)}
	if ibErr.HasClientList() {
		t.Errorf("expected HasClientList=false on parse error")
	}
	if ibErr.ClientsKey() != "" {
		t.Errorf("expected empty ClientsKey on parse error")
	}

	// Has clients
	ibClients := RawInbound{"settings": []byte(`{"clients":[]}`)}
	if !ibClients.HasClientList() || ibClients.ClientsKey() != "clients" {
		t.Errorf("expected clients")
	}

	// Has users
	ibUsers := RawInbound{"settings": []byte(`{"users":[]}`)}
	if !ibUsers.HasClientList() || ibUsers.ClientsKey() != "users" {
		t.Errorf("expected users")
	}

	// Neither
	ibNone := RawInbound{"settings": []byte(`{}`)}
	if ibNone.HasClientList() || ibNone.ClientsKey() != "" {
		t.Errorf("expected neither")
	}
}

func TestRawInbound_GetClients(t *testing.T) {
	// Settings parse error
	ibErr := RawInbound{"settings": []byte(`{`)}
	_, err := ibErr.GetClients()
	if err == nil {
		t.Errorf("expected error on invalid settings")
	}

	// Error parsing clients
	ibBadClients := RawInbound{"settings": []byte(`{"clients": {}}`)}
	_, err = ibBadClients.GetClients()
	if err == nil || !strings.Contains(err.Error(), "parsing clients") {
		t.Errorf("expected clients parse error, got %v", err)
	}

	// Error parsing users
	ibBadUsers := RawInbound{"settings": []byte(`{"users": {}}`)}
	_, err = ibBadUsers.GetClients()
	if err == nil || !strings.Contains(err.Error(), "parsing users") {
		t.Errorf("expected users parse error, got %v", err)
	}

	// Success clients
	ibC := RawInbound{"settings": []byte(`{"clients": [{"email": "c@c"}]}`)}
	clients, err := ibC.GetClients()
	if err != nil || len(clients) != 1 || clients[0].Email() != "c@c" {
		t.Errorf("unexpected clients: %v, %v", clients, err)
	}

	// Success users
	ibU := RawInbound{"settings": []byte(`{"users": [{"email": "u@u"}]}`)}
	users, err := ibU.GetClients()
	if err != nil || len(users) != 1 || users[0].Email() != "u@u" {
		t.Errorf("unexpected users: %v, %v", users, err)
	}

	// Neither
	ibNone := RawInbound{"settings": []byte(`{"other": 123}`)}
	n, err := ibNone.GetClients()
	if err != nil || n != nil {
		t.Errorf("expected nil, nil, got %v, %v", n, err)
	}
	
	// null settings
	ibNull := RawInbound{"settings": []byte(`null`)}
	n2, err := ibNull.GetClients()
	if err != nil || n2 != nil {
		t.Errorf("expected nil, nil for null settings, got %v, %v", n2, err)
	}
}

func TestRawInbound_SetClients(t *testing.T) {
	// Parse error
	ibErr := RawInbound{"settings": []byte(`{`)}
	err := ibErr.SetClients(nil)
	if err == nil {
		t.Errorf("expected error")
	}

	clients := []RawClient{
		{"email": []byte(`"a@a"`), "hy2_auth": []byte(`"123"`), "hy2_obfs": []byte(`"456"`)},
	}

	// Has clients
	ibC := RawInbound{"settings": []byte(`{"clients":[]}`)}
	_ = ibC.SetClients(clients)
	if !strings.Contains(string(ibC["settings"]), `"clients":`) || strings.Contains(string(ibC["settings"]), "hy2_auth") {
		t.Errorf("SetClients with 'clients' failed: %s", string(ibC["settings"]))
	}

	// Has users
	ibU := RawInbound{"settings": []byte(`{"users":[]}`)}
	_ = ibU.SetClients(clients)
	if !strings.Contains(string(ibU["settings"]), `"users":`) {
		t.Errorf("SetClients with 'users' failed: %s", string(ibU["settings"]))
	}

	// Fallback hysteria
	ibH := RawInbound{"protocol": []byte(`"hysteria"`)}
	_ = ibH.SetClients(clients)
	if !strings.Contains(string(ibH["settings"]), `"users":`) {
		t.Errorf("SetClients hysteria fallback failed: %s", string(ibH["settings"]))
	}

	// Fallback other
	ibO := RawInbound{"protocol": []byte(`"vless"`)}
	_ = ibO.SetClients(clients)
	if !strings.Contains(string(ibO["settings"]), `"clients":`) {
		t.Errorf("SetClients other fallback failed: %s", string(ibO["settings"]))
	}
}

func TestRawClient_Methods(t *testing.T) {
	c := RawClient{
		"email":   []byte(`"test@test"`),
		"name":    []byte(`"test-name"`),
		"limit":   []byte(`100`),
		"subfile": []byte(`"sub"`),
		"expire":  []byte(`"exp"`),
		"other":   []byte(`"o"`),
		"bool":    []byte(`true`),
		"hy2_auth": []byte(`"auth"`),
	}

	// ForXrayAPI
	apiC := c.ForXrayAPI()
	if apiC.Has("limit") || apiC.Has("subfile") || apiC.Has("expire") {
		t.Errorf("ForXrayAPI failed to strip meta")
	}
	if !apiC.Has("other") {
		t.Errorf("ForXrayAPI stripped non-meta")
	}

	// Email
	if c.Email() != "test@test" {
		t.Errorf("Email() failed")
	}
	c2 := RawClient{"name": []byte(`"test-name"`)}
	if c2.Email() != "test-name" {
		t.Errorf("Email() fallback failed")
	}

	// GetString
	if c.GetString("email") != "test@test" {
		t.Errorf("GetString string failed")
	}
	if c.GetString("limit") != "100" {
		t.Errorf("GetString number coercion failed")
	}
	if c.GetString("bool") != "" {
		t.Errorf("GetString invalid type failed")
	}
	if c.GetString("missing") != "" {
		t.Errorf("GetString missing failed")
	}

	// GetNumber
	if n, ok := c.GetNumber("limit"); !ok || n != 100 {
		t.Errorf("GetNumber normal failed: %v, %v", n, ok)
	}
	if _, ok := c.GetNumber("missing"); ok {
		t.Errorf("GetNumber missing failed")
	}
	if _, ok := c.GetNumber("email"); ok {
		t.Errorf("GetNumber invalid type failed")
	}

	// Set
	c.Set("newStr", "abc")
	if c.GetString("newStr") != "abc" {
		t.Errorf("Set failed")
	}

	// SetNumber
	c.SetNumber("newNum", 42)
	if n, _ := c.GetNumber("newNum"); n != 42 {
		t.Errorf("SetNumber failed")
	}
	// NaN / Inf skip
	c.SetNumber("nan", math.NaN())
	if c.Has("nan") {
		t.Errorf("SetNumber NaN should be skipped")
	}

	// Delete
	c.Delete("other")
	if c.Has("other") {
		t.Errorf("Delete failed")
	}
}

func TestTaggedClient(t *testing.T) {
	tc := TaggedClient{
		Tag: "t1",
		Client: RawClient{"id": []byte(`"123"`)},
	}
	if tc.Tag != "t1" {
		t.Errorf("TaggedClient failed")
	}
}

func TestInboundInfo(t *testing.T) {
	ii := InboundInfo{Tag: "t1", Protocol: "p1"}
	if ii.Tag != "t1" {
		t.Errorf("InboundInfo failed")
	}
}
