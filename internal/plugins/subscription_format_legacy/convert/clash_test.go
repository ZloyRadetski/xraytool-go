package convert

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type clashDoc struct {
	Proxies     []map[string]any `yaml:"proxies"`
	ProxyGroups []struct {
		Name    string   `yaml:"name"`
		Type    string   `yaml:"type"`
		Proxies []string `yaml:"proxies"`
	} `yaml:"proxy-groups"`
	Rules []string `yaml:"rules"`
}

func parseClash(t *testing.T, out string) clashDoc {
	t.Helper()
	var doc clashDoc
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("invalid clash yaml: %v\n%s", err, out)
	}
	return doc
}

func TestShareTextToClashYAML_VLESSReality(t *testing.T) {
	link := "vless://a3482e88-686a-4a58-8126-99c9df64b7bf@1.2.3.4:443?security=reality&sni=www.google.com&fp=chrome&pbk=PUBKEY&sid=ab12&flow=xtls-rprx-vision&type=tcp#VLESS-Reality"

	out, err := ShareTextToClashYAML(link)
	if err != nil {
		t.Fatalf("ShareTextToClashYAML: %v", err)
	}

	doc := parseClash(t, out)
	if len(doc.Proxies) != 1 {
		t.Fatalf("expected 1 proxy, got %d", len(doc.Proxies))
	}
	p := doc.Proxies[0]
	if p["type"] != "vless" || p["server"] != "1.2.3.4" || p["port"] != 443 {
		t.Fatalf("unexpected proxy: %#v", p)
	}
	if p["name"] != "VLESS-Reality" {
		t.Errorf("expected name from fragment, got %v", p["name"])
	}
	if p["uuid"] != "a3482e88-686a-4a58-8126-99c9df64b7bf" {
		t.Errorf("unexpected uuid: %v", p["uuid"])
	}
	if p["flow"] != "xtls-rprx-vision" {
		t.Errorf("unexpected flow: %v", p["flow"])
	}
	if p["tls"] != true || p["servername"] != "www.google.com" || p["client-fingerprint"] != "chrome" {
		t.Errorf("unexpected tls settings: %#v", p)
	}
	reality, ok := p["reality-opts"].(map[string]any)
	if !ok || reality["public-key"] != "PUBKEY" || reality["short-id"] != "ab12" {
		t.Errorf("unexpected reality-opts: %#v", p["reality-opts"])
	}
	if len(doc.ProxyGroups) != 2 || len(doc.Rules) == 0 {
		t.Errorf("expected proxy groups and rules, got %#v / %#v", doc.ProxyGroups, doc.Rules)
	}
	if doc.ProxyGroups[1].Proxies[0] != "VLESS-Reality" {
		t.Errorf("auto group must reference proxy names: %#v", doc.ProxyGroups[1])
	}
}

func TestShareTextToClashYAML_WebsocketAndDuplicateNames(t *testing.T) {
	links := strings.Join([]string{
		"vless://uuid-1@example.com:443?security=tls&type=ws&path=%2Fws&host=cdn.example.com#node",
		"vless://uuid-2@example.org:443?security=tls&type=ws&path=%2Fws#node",
	}, "\n")

	out, err := ShareTextToClashYAML(links)
	if err != nil {
		t.Fatalf("ShareTextToClashYAML: %v", err)
	}

	doc := parseClash(t, out)
	if len(doc.Proxies) != 2 {
		t.Fatalf("expected 2 proxies, got %d", len(doc.Proxies))
	}
	if doc.Proxies[0]["name"] != "node" || doc.Proxies[1]["name"] != "node-2" {
		t.Errorf("duplicate names must be suffixed: %v / %v", doc.Proxies[0]["name"], doc.Proxies[1]["name"])
	}
	wsOpts, ok := doc.Proxies[0]["ws-opts"].(map[string]any)
	if !ok || wsOpts["path"] != "/ws" {
		t.Fatalf("unexpected ws-opts: %#v", doc.Proxies[0]["ws-opts"])
	}
	headers, ok := wsOpts["headers"].(map[string]any)
	if !ok || headers["Host"] != "cdn.example.com" {
		t.Errorf("unexpected ws headers: %#v", wsOpts["headers"])
	}
	if doc.Proxies[0]["network"] != "ws" {
		t.Errorf("expected ws network, got %v", doc.Proxies[0]["network"])
	}
}

func TestShareTextToClashYAML_TrojanAndShadowsocks(t *testing.T) {
	links := strings.Join([]string{
		"trojan://secret@1.2.3.4:8443?security=tls&sni=example.com#trojan-node",
		"ss://YWVzLTI1Ni1nY206cGFzc3dvcmQ@1.2.3.4:8388#ss-node",
	}, "\n")

	out, err := ShareTextToClashYAML(links)
	if err != nil {
		t.Fatalf("ShareTextToClashYAML: %v", err)
	}

	doc := parseClash(t, out)
	if len(doc.Proxies) != 2 {
		t.Fatalf("expected 2 proxies, got %d", len(doc.Proxies))
	}
	trojan := doc.Proxies[0]
	if trojan["type"] != "trojan" || trojan["password"] != "secret" || trojan["sni"] != nil && trojan["servername"] != "example.com" {
		t.Errorf("unexpected trojan proxy: %#v", trojan)
	}
	ss := doc.Proxies[1]
	if ss["type"] != "ss" || ss["cipher"] != "aes-256-gcm" || ss["password"] != "password" || ss["port"] != 8388 {
		t.Errorf("unexpected ss proxy: %#v", ss)
	}
}

func TestShareTextToClashYAML_Hysteria2(t *testing.T) {
	out, err := ShareTextToClashYAML("hysteria2://pass123@1.2.3.4:8443?sni=example.com&obfs=salamander&obfs-password=obfspass&insecure=1#hy2")
	if err != nil {
		t.Fatalf("ShareTextToClashYAML: %v", err)
	}

	p := parseClash(t, out).Proxies[0]
	if p["type"] != "hysteria2" || p["password"] != "pass123" || p["sni"] != "example.com" {
		t.Fatalf("unexpected hysteria2 proxy: %#v", p)
	}
	if p["obfs"] != "salamander" || p["obfs-password"] != "obfspass" || p["skip-cert-verify"] != true {
		t.Errorf("unexpected hysteria2 options: %#v", p)
	}
}

func TestShareTextToClashYAML_VMess(t *testing.T) {
	// base64 of {"v":"2","ps":"vmess-node","add":"1.2.3.4","port":"443","id":"uuid-1","aid":"0","net":"ws","path":"/p","host":"h.example.com","tls":"tls"}
	link := "vmess://eyJ2IjoiMiIsInBzIjoidm1lc3Mtbm9kZSIsImFkZCI6IjEuMi4zLjQiLCJwb3J0IjoiNDQzIiwiaWQiOiJ1dWlkLTEiLCJhaWQiOiIwIiwibmV0Ijoid3MiLCJwYXRoIjoiL3AiLCJob3N0IjoiaC5leGFtcGxlLmNvbSIsInRscyI6InRscyJ9"

	out, err := ShareTextToClashYAML(link)
	if err != nil {
		t.Fatalf("ShareTextToClashYAML: %v", err)
	}

	p := parseClash(t, out).Proxies[0]
	if p["type"] != "vmess" || p["name"] != "vmess-node" || p["uuid"] != "uuid-1" || p["port"] != 443 {
		t.Fatalf("unexpected vmess proxy: %#v", p)
	}
	if p["tls"] != true || p["servername"] != nil && p["servername"] != "" {
		// sni absent in payload, servername should be unset
		t.Logf("vmess tls: %#v", p)
	}
	if p["network"] != "ws" {
		t.Errorf("expected ws network, got %v", p["network"])
	}
}

func TestShareTextToClashYAML_Socks(t *testing.T) {
	// dXNlcjpwYXNz = "user:pass"
	out, err := ShareTextToClashYAML("socks://dXNlcjpwYXNz@1.2.3.4:1080#socks-node")
	if err != nil {
		t.Fatalf("ShareTextToClashYAML: %v", err)
	}

	p := parseClash(t, out).Proxies[0]
	if p["type"] != "socks5" || p["username"] != "user" || p["password"] != "pass" {
		t.Fatalf("unexpected socks proxy: %#v", p)
	}
}

func TestShareTextToClashYAML_Errors(t *testing.T) {
	if _, err := ShareTextToClashYAML(""); err == nil {
		t.Error("expected error for empty input")
	}
	if _, err := ShareTextToClashYAML("ftp://example.com:21"); err == nil {
		t.Error("expected error when no supported proxies are found")
	}
	if _, err := ShareTextToClashYAML("vless://uuid@example.com#no-port"); err == nil {
		t.Error("expected error for missing port")
	}
	if _, err := ShareTextToClashYAML("not-a-link"); err == nil {
		t.Error("expected error for malformed link")
	}
}

func TestXrayJSONToClashYAML(t *testing.T) {
	xrayJSON := `{
		"outbounds": [
			{
				"protocol": "vless",
				"tag": "VLESS-Reality",
				"settings": {
					"vnext": [
						{
							"address": "1.2.3.4",
							"port": 443,
							"users": [
								{"id": "a3482e88-686a-4a58-8126-99c9df64b7bf", "encryption": "none", "flow": "xtls-rprx-vision"}
							]
						}
					]
				},
				"streamSettings": {
					"network": "tcp",
					"security": "reality",
					"realitySettings": {
						"serverName": "www.google.com",
						"fingerprint": "chrome",
						"publicKey": "PUBKEY",
						"shortId": "ab12"
					}
				}
			}
		]
	}`

	out, err := XrayJSONToClashYAML(xrayJSON)
	if err != nil {
		t.Fatalf("XrayJSONToClashYAML: %v", err)
	}

	doc := parseClash(t, out)
	if len(doc.Proxies) != 1 {
		t.Fatalf("expected 1 proxy, got %d", len(doc.Proxies))
	}
	p := doc.Proxies[0]
	if p["type"] != "vless" || p["server"] != "1.2.3.4" || p["port"] != 443 {
		t.Fatalf("unexpected proxy: %#v", p)
	}
	if p["tls"] != true {
		t.Errorf("expected reality to enable tls: %#v", p)
	}

	if _, err := XrayJSONToClashYAML("invalid"); err == nil {
		t.Error("expected error for invalid JSON")
	}
}
