package engine_xray

import (
	json "github.com/goccy/go-json"
	"strings"
	"testing"
)

func TestBuildClient(t *testing.T) {
	tests := []struct {
		name      string
		protocol  string
		streamSec string
		params    ClientParams
		want      map[string]string
		wantErr   bool
	}{
		{
			name:      "vless with reality",
			protocol:  "vless",
			streamSec: "reality",
			params:    ClientParams{Email: "test@test", UUID: "123", Expire: "2025"},
			want:      map[string]string{"email": "test@test", "id": "123", "flow": "xtls-rprx-vision", "expire": "2025"},
			wantErr:   false,
		},
		{
			name:      "vless without streamSettings",
			protocol:  "vless",
			streamSec: "",
			params:    ClientParams{Email: "test@test", UUID: "123"},
			want:      map[string]string{"email": "test@test", "id": "123"}, // no flow
			wantErr:   false,
		},
		{
			name:      "vmess",
			protocol:  "vmess",
			streamSec: "",
			params:    ClientParams{Email: "test@test", UUID: "123"},
			want:      map[string]string{"email": "test@test", "id": "123"},
			wantErr:   false,
		},
		{
			name:      "trojan",
			protocol:  "trojan",
			streamSec: "tls",
			params:    ClientParams{Email: "test@test", Auth: "secret"},
			want:      map[string]string{"email": "test@test", "password": "secret"},
			wantErr:   false,
		},
		{
			name:      "hysteria2 fallback auth",
			protocol:  "hysteria2",
			streamSec: "",
			params:    ClientParams{Email: "test@test", UUID: "123"},                                                                       // no auth provided
			want:      map[string]string{"email": "test@test", "auth": "aecfb6182c8734da6f1218ca7ff28607d2f721be2340ba120133a109a88b871f"}, // SHA256 of 123
			wantErr:   false,
		},
		{
			name:      "hysteria2 legacy uuid auth replacement",
			protocol:  "hysteria2",
			streamSec: "",
			params:    ClientParams{Email: "test@test", UUID: "123", Auth: "12345678-1234-1234-1234-1234567890ab"},
			want:      map[string]string{"email": "test@test", "auth": "aecfb6182c8734da6f1218ca7ff28607d2f721be2340ba120133a109a88b871f"}, // should be replaced with SHA256 of 123
			wantErr:   false,
		},
		{
			name:      "unsupported protocol",
			protocol:  "wireguard",
			streamSec: "",
			params:    ClientParams{Email: "test@test", UUID: "123"},
			want:      nil,
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ibMap := map[string]interface{}{
				"protocol": tt.protocol,
			}
			if tt.streamSec != "" {
				ibMap["streamSettings"] = map[string]interface{}{
					"security": tt.streamSec,
				}
			}
			data, _ := json.Marshal(ibMap)
			var ib RawInbound
			json.Unmarshal(data, &ib) //nolint:errcheck

			client, err := BuildClient(ib, tt.params)
			if (err != nil) != tt.wantErr {
				t.Errorf("BuildClient() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err == nil {
				for k, v := range tt.want {
					if !client.Has(k) {
						t.Errorf("BuildClient() missing key %q", k)
					} else {
						// For string keys
						val := client.GetString(k)
						if k == "auth" && tt.protocol == "hysteria2" {
							// we just check prefix for simplicity since generation expands it
							if !strings.HasPrefix(val, v) {
								t.Errorf("BuildClient() key %q = %q, want prefix %q", k, val, v)
							}
						} else {
							if val != v {
								t.Errorf("BuildClient() key %q = %q, want %q", k, val, v)
							}
						}
					}
				}
			}
		})
	}
}

func TestBuildClientXHTTPNeverGeneratesVisionFlow(t *testing.T) {
	for _, protocol := range []string{"xhttp", "splithttp"} {
		t.Run(protocol, func(t *testing.T) {
			data, err := json.Marshal(map[string]interface{}{
				"protocol": protocol,
				// Deliberately omit network: the old implementation treated this as
				// TCP and incorrectly generated Vision for xHTTP.
				"streamSettings": map[string]interface{}{"security": "reality"},
			})
			if err != nil {
				t.Fatal(err)
			}
			var inbound RawInbound
			if err := json.Unmarshal(data, &inbound); err != nil {
				t.Fatal(err)
			}
			client, err := BuildClient(inbound, ClientParams{
				Email: "user@example.test", UUID: "11111111-1111-1111-1111-111111111111", Flow: "xtls-rprx-vision",
			})
			if err != nil {
				t.Fatal(err)
			}
			if client.Has("flow") {
				t.Fatalf("%s client must not contain Vision flow: %+v", protocol, client)
			}
		})
	}
}
