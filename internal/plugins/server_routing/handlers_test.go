package server_routing

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	json "github.com/goccy/go-json"
	"xraytool/internal/appconfig"
)

func createTestPlugin(t *testing.T) (*Plugin, string) {
	t.Helper()
	tmpDir, err := os.MkdirTemp("", "server_routing_plugin_test_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmpDir) })

	_ = os.MkdirAll(filepath.Join(tmpDir, "outbounds"), 0o755)

	xrayPath := filepath.Join(tmpDir, "xray.json")
	_ = os.WriteFile(xrayPath, []byte(`{"routing":{}, "outbounds":[]}`), 0o644)

	cfg := &appconfig.Config{
		Mode: "master",
		Server: appconfig.ServerConf{
			Domain: "master.test.com",
			IP:     "1.2.3.4",
		},
		Replication: appconfig.ReplicationConf{
			AllowedNodes: []string{"msk-slave"},
		},
		Paths: appconfig.PathsConf{
			XrayConfig: xrayPath,
		},
	}

	mgr := NewManager(tmpDir, cfg, nil)
	mgr.SetRestartFunc(func(ctx context.Context) error { return nil })
	return &Plugin{
		cfg:     pluginConfig{RoutingDir: tmpDir},
		manager: mgr,
	}, tmpDir
}

func TestHandlers_GetTopology(t *testing.T) {
	p, _ := createTestPlugin(t)

	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/routing/topology", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}

	var topo TopologyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &topo); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if len(topo.Servers) != 2 {
		t.Errorf("expected 2 servers, got %d", len(topo.Servers))
	}
}

func TestHandlers_GetTopology_Uninitialized(t *testing.T) {
	p := &Plugin{}
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/routing/topology", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 Service Unavailable, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandlers_ApplyRouting_Success(t *testing.T) {
	p, _ := createTestPlugin(t)

	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	payload := ApplyRequest{
		Routing: []ServerRoutingConfig{
			{
				Server: "master.test.com",
				Rules: []RoutingRule{
					{
						ID:           "rule-1",
						Name:         "Route to MSK",
						SourceServer: "master.test.com",
						TargetServer: "direct",
						Domain:       []string{"geosite:google"},
						Priority:     1,
						Enabled:      true,
					},
				},
			},
			{
				Server: "msk-slave",
				Rules:  []RoutingRule{},
			},
		},
	}

	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/routing/apply", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp ApplyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if !resp.OK {
		t.Errorf("expected resp.OK to be true")
	}
}

func TestHandlers_ApplyRouting_Uninitialized(t *testing.T) {
	p := &Plugin{}
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/routing/apply", bytes.NewReader([]byte(`{}`)))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 Service Unavailable, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandlers_ApplyRouting_MalformedJSON(t *testing.T) {
	p, _ := createTestPlugin(t)
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	// 1. Invalid syntax
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/routing/apply", bytes.NewReader([]byte(`{bad json`)))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request, got %d: %s", rec.Code, rec.Body.String())
	}

	// 2. Empty body
	reqEmpty := httptest.NewRequest(http.MethodPost, "/api/v1/admin/routing/apply", bytes.NewReader([]byte(``)))
	recEmpty := httptest.NewRecorder()
	mux.ServeHTTP(recEmpty, reqEmpty)

	if recEmpty.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request on empty body, got %d: %s", recEmpty.Code, recEmpty.Body.String())
	}
}

func TestHandlers_ApplyRouting_BodyTooLarge(t *testing.T) {
	p, _ := createTestPlugin(t)
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	// Create payload > 2MB
	hugeJSON := `{"routing": [{"server": "master.test.com", "rules": [{"name": "` + strings.Repeat("x", 2*1024*1024+500) + `"}]}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/routing/apply", strings.NewReader(hugeJSON))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest && rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 400 or 413 for oversized body, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandlers_ApplyRouting_ValidationError(t *testing.T) {
	p, _ := createTestPlugin(t)
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	// Dangling rule (missing target_server)
	payload := ApplyRequest{
		Routing: []ServerRoutingConfig{
			{
				Server: "master.test.com",
				Rules: []RoutingRule{
					{
						ID:           "rule-1",
						Name:         "Dangling rule",
						SourceServer: "master.test.com",
						TargetServer: "",
						Domain:       []string{"geosite:google"},
						Priority:     1,
						Enabled:      true,
					},
				},
			},
		},
	}

	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/routing/apply", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 Unprocessable Entity, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandlers_ApplyRouting_MissingOutboundTemplate(t *testing.T) {
	p, tmpDir := createTestPlugin(t)
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	// Set XrayConfig path to a temporary valid xray config file
	xrayPath := filepath.Join(tmpDir, "xray.json")
	_ = os.WriteFile(xrayPath, []byte(`{"routing": {}, "outbounds": []}`), 0o644)
	p.manager.cfg.Paths.XrayConfig = xrayPath

	// Rule targets msk-slave, but msk-slave outbound template does NOT exist
	payload := ApplyRequest{
		Routing: []ServerRoutingConfig{
			{
				Server: "master.test.com",
				Rules: []RoutingRule{
					{
						ID:           "rule-relay",
						Name:         "Route to MSK without template",
						SourceServer: "master.test.com",
						TargetServer: "msk-slave",
						Domain:       []string{"geosite:google"},
						Priority:     1,
						Enabled:      true,
					},
				},
			},
		},
	}

	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/routing/apply", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 Unprocessable Entity for missing outbound template, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandlers_ApplyRouting_CorruptLocalXrayConfig(t *testing.T) {
	p, tmpDir := createTestPlugin(t)
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	// Set XrayConfig to a corrupt non-JSON file
	xrayPath := filepath.Join(tmpDir, "xray.json")
	_ = os.WriteFile(xrayPath, []byte(`CORRUPTED_JSON`), 0o644)
	p.manager.cfg.Paths.XrayConfig = xrayPath

	payload := ApplyRequest{
		Routing: []ServerRoutingConfig{
			{
				Server: "master.test.com",
				Rules: []RoutingRule{
					{
						ID:           "rule-1",
						Name:         "Direct",
						SourceServer: "master.test.com",
						TargetServer: "direct",
						Domain:       []string{"geosite:google"},
						Priority:     1,
						Enabled:      true,
					},
				},
			},
		},
	}

	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/routing/apply", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 Internal Server Error for corrupt xray config, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestPlugin_Lifecycle_And_Health(t *testing.T) {
	p := New()

	meta := p.Metadata()
	if meta.Name != "server_routing" {
		t.Errorf("unexpected metadata name: %s", meta.Name)
	}

	// Health before Init
	if err := p.Health(context.Background()); err == nil {
		t.Errorf("expected error on Health before Init")
	}

	tmpDir, _ := os.MkdirTemp("", "sr_plugin_*")
	defer func() { _ = os.RemoveAll(tmpDir) }()

	// Init with nil resolver
	err := p.Init(context.Background(), map[string]any{"routing_dir": tmpDir}, nil)
	if err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	if err := p.Health(context.Background()); err != nil {
		t.Errorf("expected healthy after Init, got %v", err)
	}

	// Register routes with nil mux should not panic
	p.RegisterRoutes(nil)

	if err := p.Stop(context.Background()); err != nil {
		t.Errorf("Stop failed: %v", err)
	}
}
