// Package config_storage owns the API for operator-managed configuration
// assets. It preserves the legacy endpoints while isolating filesystem policy
// from the mandatory core domain plugin.
package config_storage

import (
	"context"
	"fmt"
	json "github.com/goccy/go-json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"xraytool/internal/appconfig"
	"xraytool/internal/events"
	"xraytool/internal/pluginapi"
	"xraytool/internal/plugins/core"
	"xraytool/internal/safeio"
)

const maxUploadBytes = 10 * 1024 * 1024

type Plugin struct {
	cfg        *appconfig.Config
	dispatcher *events.Dispatcher
	auth       func(http.Handler) http.Handler
}

func New(cfg *appconfig.Config) *Plugin { return &Plugin{cfg: cfg} }

func (p *Plugin) Metadata() pluginapi.Metadata {
	return pluginapi.Metadata{
		Name:        "config_storage",
		Kind:        "storage",
		Version:     "1.0.0",
		APIVersion:  pluginapi.CurrentAPIVersion,
		Description: "Authorized storage for subscription templates and other configuration assets.",
		Requires: []pluginapi.ServiceRef{
			{Name: "protected_middleware"},
			{Name: core.ServiceEventDispatcher},
		},
	}
}

func (p *Plugin) Init(_ context.Context, _ pluginapi.RawConfig, reg pluginapi.ServiceResolver) error {
	if p.cfg == nil {
		return fmt.Errorf("config_storage: app config must not be nil")
	}
	auth, err := reg.Resolve("protected_middleware")
	if err != nil {
		return err
	}
	dispatcher, err := reg.Resolve(core.ServiceEventDispatcher)
	if err != nil {
		return err
	}
	p.auth = auth.(func(http.Handler) http.Handler)
	p.dispatcher = dispatcher.(*events.Dispatcher)
	return nil
}

func (p *Plugin) Start(ctx context.Context) error { <-ctx.Done(); return nil }
func (p *Plugin) Stop(_ context.Context) error    { return nil }
func (p *Plugin) Health(_ context.Context) error {
	if p.auth == nil {
		return fmt.Errorf("config_storage: not initialized")
	}
	return nil
}

func (p *Plugin) RegisterRoutes(mux *http.ServeMux) {
	protected := func(handler http.HandlerFunc) http.Handler { return p.auth(handler) }
	mux.Handle("POST /api/rest/update-links", protected(p.handleUpdateLinks))
	mux.Handle("POST /api/rest/upload", protected(p.handleUpload))
	mux.Handle("GET /api/rest/download", protected(p.handleDownload))
}

func (p *Plugin) handleUpdateLinks(w http.ResponseWriter, req *http.Request) {
	p.handleWrite(w, req, true)
}

func (p *Plugin) handleUpload(w http.ResponseWriter, req *http.Request) {
	p.handleWrite(w, req, false)
}

func (p *Plugin) handleWrite(w http.ResponseWriter, req *http.Request, allowConfigFiles bool) {
	req.Body = http.MaxBytesReader(w, req.Body, maxUploadBytes)
	if err := req.ParseMultipartForm(maxUploadBytes); err != nil {
		writeError(w, http.StatusBadRequest, "form parse error")
		return
	}
	path := req.FormValue("path")
	if !p.allowed(path) {
		writeError(w, http.StatusForbidden, "path not allowed")
		return
	}
	if !allowConfigFiles && forbiddenUploadExtension(path) {
		writeError(w, http.StatusForbidden, "forbidden file extension")
		return
	}
	file, _, err := req.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "missing file")
		return
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "read error")
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, "disk error")
		return
	}
	if err := safeio.WriteToFile(path, data, 0o644); err != nil {
		writeError(w, http.StatusInternalServerError, "disk error")
		return
	}
	if allowConfigFiles && p.dispatcher != nil {
		p.dispatcher.Dispatch("file.uploaded", map[string]any{"path": path}, nil)
	}
	message := "file saved"
	if allowConfigFiles {
		message = "file saved & webhook dispatched"
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "success", "message": message})
}

func (p *Plugin) handleDownload(w http.ResponseWriter, req *http.Request) {
	path := req.URL.Query().Get("path")
	if !p.allowed(path) {
		writeError(w, http.StatusForbidden, "path not allowed")
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			writeError(w, http.StatusNotFound, "file not found")
		} else {
			writeError(w, http.StatusInternalServerError, "disk error")
		}
		return
	}
	if info.IsDir() {
		writeError(w, http.StatusNotFound, "file not found")
		return
	}
	http.ServeFile(w, req, path)
}

func (p *Plugin) allowed(path string) bool {
	cleanPath := filepath.Clean(path)
	if path == "" || !filepath.IsAbs(cleanPath) {
		return false
	}
	for _, root := range p.cfg.Server.AllowedDirs {
		realRoot, err := filepath.EvalSymlinks(root)
		if err != nil {
			realRoot, err = filepath.Abs(root)
			if err != nil {
				continue
			}
		}
		rel, err := filepath.Rel(realRoot, cleanPath)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
			return true
		}
	}
	return false
}

func forbiddenUploadExtension(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".exe" || ext == ".yaml" || ext == ".yml"
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

var _ pluginapi.Plugin = (*Plugin)(nil)
var _ pluginapi.HTTPContributor = (*Plugin)(nil)
