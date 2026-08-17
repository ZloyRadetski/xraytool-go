package server_routing

import (
	"context"
	"fmt"
	"net/http"

	"xraytool/internal/appconfig"
	"xraytool/internal/pluginapi"
)

type replicationSyncer interface {
	TriggerSync(ctx context.Context) error
}

// Plugin provides the administrative routing-management API and storage.
type Plugin struct {
	log                 pluginapi.Logger
	appCfg              *appconfig.Config
	cfg                 pluginConfig
	manager             *Manager
	replicationProvider replicationSyncer
}

// New creates a server-routing plugin instance.
func New(appCfg ...*appconfig.Config) *Plugin {
	p := &Plugin{}
	if len(appCfg) > 0 {
		p.appCfg = appCfg[0]
	}
	return p
}

func (p *Plugin) Metadata() pluginapi.Metadata {
	return pluginapi.Metadata{
		Name:        "server_routing",
		Kind:        "admin_tool",
		Version:     "1.0.0",
		APIVersion:  pluginapi.CurrentAPIVersion,
		Description: "Administrative Xray server traffic routing management.",
		Mandatory:   false,
	}
}

func (p *Plugin) Init(_ context.Context, rawCfg pluginapi.RawConfig, reg pluginapi.ServiceResolver) error {
	if reg != nil {
		p.log = reg.Logger()
	}
	if p.log == nil {
		p.log = nopLogger{}
	}

	cfg, err := parseConfig(rawCfg)
	if err != nil {
		return fmt.Errorf("server_routing: config error: %w", err)
	}
	if err := ensureDirectories(cfg); err != nil {
		return fmt.Errorf("server_routing: %w", err)
	}
	p.cfg = cfg

	// Resolve appconfig if available
	appCfg := p.appCfg
	if appCfg == nil && reg != nil {
		if srv, err := reg.Resolve("app_config"); err == nil {
			if ac, ok := srv.(*appconfig.Config); ok {
				appCfg = ac
			}
		}
	}
	if reg != nil {
		if srv, err := reg.Resolve(pluginapi.ServiceClusterReplicationProvider); err == nil {
			if rp, ok := srv.(replicationSyncer); ok {
				p.replicationProvider = rp
			}
		}
	}

	p.manager = NewManager(cfg.RoutingDir, appCfg, p.log)
	p.log.Info("server_routing: initialised", "routing_dir", cfg.RoutingDir)
	return nil
}

func (p *Plugin) Start(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

func (p *Plugin) Stop(_ context.Context) error {
	return nil
}

func (p *Plugin) Health(_ context.Context) error {
	if p == nil || p.cfg.RoutingDir == "" || p.manager == nil {
		return fmt.Errorf("server_routing: not initialised")
	}
	return nil
}

func (p *Plugin) RegisterRoutes(mux *http.ServeMux) {
	if p == nil || mux == nil {
		return
	}
	mux.HandleFunc("GET /api/v1/admin/routing/topology", p.handleGetTopology())
	mux.HandleFunc("POST /api/v1/admin/routing/apply", p.handleApplyRouting())
}

var _ pluginapi.Plugin = (*Plugin)(nil)
var _ pluginapi.HTTPContributor = (*Plugin)(nil)

