package clusterreplication

import (
	"context"
	"fmt"
	json "github.com/goccy/go-json"
	"log/slog"
	"strings"
	"sync"
	"time"

	"xraytool/internal/appconfig"
	"xraytool/internal/database"
	"xraytool/internal/domain"
	"xraytool/internal/pluginapi"
	"xraytool/internal/plugins/cluster_replication/protocol"
	vpn "xraytool/internal/plugins/engine_xray"
)

const ServiceClusterReplicationProvider = pluginapi.ServiceClusterReplicationProvider

type Runtime struct {
	Registry domain.Registry
	Engine   domain.Engine
	// Service is supplied by the composition root on a master before core is
	// loaded, so the engine publisher and the transport use one outbox.
	Service   *Service
	Logger    *slog.Logger
	FraudSink func(context.Context, string, []pluginapi.FraudEvent) error
}

type Status struct {
	Mode            string
	NodeID          string
	LatestRevision  int64
	AppliedRevision int64
}

type Plugin struct {
	runtime Runtime

	mu              sync.RWMutex
	config          Config
	service         *Service
	log             *slog.Logger
	stopGRPC        func() error
	running         bool
	cliConfig       *appconfig.Config
	fraudSink       func(context.Context, string, []pluginapi.FraudEvent) error
	trafficSnapshot pluginapi.TrafficSnapshotProvider
	session         *slaveSession

	// fraudMu serializes queue persistence, sends and ACK processing. It keeps
	// batches ordered while the gRPC stream itself remains usable for normal
	// replication and statistics frames.
	fraudMu sync.Mutex
}

func New() *Plugin { return &Plugin{} }

func NewWithRuntime(runtime Runtime) *Plugin { return &Plugin{runtime: runtime} }

// NewCLI constructs the self-contained command entrypoint used by
// `xraytool plugin run`. Server composition overrides this factory with
// NewWithRuntime, so no second engine or transport is created there.
func NewCLI(config *appconfig.Config) *Plugin { return &Plugin{cliConfig: config} }

func (*Plugin) Metadata() pluginapi.Metadata {
	return pluginapi.Metadata{
		Name:        "cluster_replication",
		Kind:        "cluster_replication",
		Version:     "1.0.0",
		APIVersion:  pluginapi.CurrentAPIVersion,
		Description: "Durable mTLS gRPC master-to-slave replication with drift repair.",
		Mandatory:   false,
		Requires: []pluginapi.ServiceRef{
			{Name: "domain_registry"},
			{Name: "domain_engine"},
			{Name: pluginapi.ServiceTrafficSnapshotProvider, Optional: true},
		},
		Publishes: []pluginapi.ServiceRef{{Name: ServiceClusterReplicationProvider}},
	}
}

func (p *Plugin) Init(_ context.Context, raw pluginapi.RawConfig, resolver pluginapi.ServiceResolver) error {
	config, err := parseConfig(raw)
	if err != nil {
		return fmt.Errorf("cluster_replication: config: %w", err)
	}
	if err := config.Validate(); err != nil {
		return fmt.Errorf("cluster_replication: config: %w", err)
	}

	registry := p.runtime.Registry
	engine := p.runtime.Engine
	if resolver != nil {
		if registry == nil {
			value, resolveErr := resolver.Resolve("domain_registry")
			if resolveErr != nil {
				return fmt.Errorf("resolve domain_registry: %w", resolveErr)
			}
			var ok bool
			registry, ok = value.(domain.Registry)
			if !ok || registry == nil {
				return fmt.Errorf("domain_registry has unexpected type %T", value)
			}
		}
		if engine == nil {
			value, resolveErr := resolver.Resolve("domain_engine")
			if resolveErr != nil {
				return fmt.Errorf("resolve domain_engine: %w", resolveErr)
			}
			var ok bool
			engine, ok = value.(domain.Engine)
			if !ok || engine == nil {
				return fmt.Errorf("domain_engine has unexpected type %T", value)
			}
		}
		if value, resolveErr := resolver.Resolve(pluginapi.ServiceTrafficSnapshotProvider); resolveErr == nil {
			if provider, ok := value.(pluginapi.TrafficSnapshotProvider); ok {
				p.trafficSnapshot = provider
			}
		}
	}
	if registry == nil || engine == nil {
		return fmt.Errorf("runtime Registry and Engine are required")
	}

	log := p.runtime.Logger
	if log == nil {
		log = slog.Default()
	}
	service := p.runtime.Service
	if service == nil {
		db, ok := database.GormDB(registry)
		if !ok || db == nil {
			return fmt.Errorf("cluster replication requires GORM-backed registry")
		}
		service = NewService(registry, engine, NewStore(db), log)
	}

	p.mu.Lock()
	p.config = config
	p.log = log.With("plugin", "cluster_replication", "node", config.NodeID)
	p.service = service
	if p.runtime.FraudSink != nil {
		p.fraudSink = p.runtime.FraudSink
	}
	p.mu.Unlock()
	return nil
}

func (p *Plugin) PublishedServices() map[string]any {
	return map[string]any{ServiceClusterReplicationProvider: p}
}

func (p *Plugin) Start(ctx context.Context) error {
	p.mu.Lock()
	if p.running {
		p.mu.Unlock()
		return fmt.Errorf("cluster_replication already running")
	}
	p.running = true
	config := p.config
	service := p.service
	log := p.log
	fraudSink := p.fraudSink
	p.mu.Unlock()

	if config.Mode == "master" {
		stop, err := startMasterTransport(ctx, service, config, log, fraudSink)
		if err != nil {
			return err
		}
		p.mu.Lock()
		p.stopGRPC = stop
		p.mu.Unlock()
		defer func() { _ = stop() }()
		p.runMasterLoop(ctx, service, config, log)
		return nil
	}
	p.runSlaveLoop(ctx, service, config, log)
	return nil
}

func (p *Plugin) runMasterLoop(ctx context.Context, service *Service, config Config, log *slog.Logger) {
	run := func() {
		if changed, err := service.DetectDesiredState(ctx); err != nil {
			log.Error("replication desired-state scan failed", "err", err)
		} else if changed {
			log.Info("replication snapshot marker appended")
		}
		if count, err := service.PublishArtifacts(ctx, config.RealityKeysPath); err != nil {
			log.Error("replication artifact scan failed", "err", err)
		} else if count > 0 {
			log.Info("replication artifacts appended", "count", count)
		}
		if removed, err := service.store.PruneAcknowledged(ctx, config.AllowedNodes); err != nil {
			log.Error("replication outbox retention failed", "err", err)
		} else if removed > 0 {
			log.Info("replication outbox events pruned", "count", removed)
		}
	}
	run()
	ticker := time.NewTicker(config.scanEvery())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}

func (p *Plugin) runSlaveLoop(ctx context.Context, service *Service, config Config, log *slog.Logger) {
	drift := time.NewTicker(config.driftEvery())
	defer drift.Stop()
	statsTicker := time.NewTicker(config.statsEvery())
	defer statsTicker.Stop()
	connect := time.NewTimer(0)
	defer connect.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-drift.C:
			if err := service.ReconcileSlave(ctx); err != nil {
				log.Error("replication drift repair failed", "err", err)
			}
		case <-statsTicker.C:
			if err := p.reportSlaveStats(ctx); err != nil && ctx.Err() == nil {
				log.Debug("replication statistics report skipped", "err", err)
			}
			if err := p.flushFraudOutbox(ctx); err != nil && ctx.Err() == nil {
				log.Debug("replication anti-fraud retry skipped", "err", err)
			}
		case <-connect.C:
			if err := runSlaveSession(ctx, service, config, log, p.setSlaveSession, p.acknowledgeFraudEvents); err != nil && ctx.Err() == nil {
				log.Warn("replication stream disconnected", "err", err)
			}
			connect.Reset(config.reconnectEvery())
		}
	}
}

func (p *Plugin) setSlaveSession(session *slaveSession) {
	p.mu.Lock()
	p.session = session
	p.mu.Unlock()
	if session != nil {
		go func() { _ = p.reportSlaveStats(context.Background()) }()
		go func() { _ = p.flushFraudOutbox(context.Background()) }()
	}
}

func (p *Plugin) reportSlaveStats(ctx context.Context) error {
	p.mu.RLock()
	provider := p.trafficSnapshot
	p.mu.RUnlock()
	if provider == nil {
		return fmt.Errorf("traffic snapshot provider is unavailable")
	}
	snapshot, err := provider.LocalTrafficSnapshot(ctx)
	if err != nil {
		return fmt.Errorf("generate local traffic snapshot: %w", err)
	}
	points := make([]statsPoint, 0, len(snapshot))
	for _, user := range snapshot {
		points = append(points, statsPoint{Email: user.Email, Total: user.Usage.UploadBytes + user.Usage.DownloadBytes})
	}
	return p.ReportStatistics(ctx, points)
}

func (p *Plugin) ReportStatistics(ctx context.Context, points []statsPoint) error {
	p.mu.RLock()
	mode := p.config.Mode
	session := p.session
	p.mu.RUnlock()
	if mode != "slave" {
		return fmt.Errorf("statistics may be reported only by a slave")
	}
	if session == nil {
		return fmt.Errorf("replication stream is not connected")
	}
	payload, err := json.Marshal(points)
	if err != nil {
		return err
	}
	return session.send(protocol.Frame{Kind: protocol.KindStats, Payload: payload})
}

// ReportFraudEvents commits slave observations to a local durable outbox before
// attempting the authenticated replication stream. A disconnected master is
// therefore a retry condition, not a reason to discard an IP observation.
func (p *Plugin) ReportFraudEvents(ctx context.Context, events []pluginapi.FraudEvent) error {
	if len(events) == 0 {
		return nil
	}
	p.fraudMu.Lock()
	defer p.fraudMu.Unlock()

	p.mu.RLock()
	mode := p.config.Mode
	service := p.service
	p.mu.RUnlock()
	if mode != "slave" {
		return fmt.Errorf("anti-fraud events may be reported only by a slave")
	}
	if service == nil || service.store == nil {
		return fmt.Errorf("replication anti-fraud outbox is unavailable")
	}
	if _, err := service.store.EnqueueFraudEvents(ctx, events); err != nil {
		return err
	}
	return p.flushFraudOutboxLocked(ctx)
}

// flushFraudOutbox retries the oldest unacknowledged batch when a slave stream
// is connected. It is intentionally harmless while disconnected: persistence
// already succeeded and the next connection or periodic tick will retry.
func (p *Plugin) flushFraudOutbox(ctx context.Context) error {
	p.fraudMu.Lock()
	defer p.fraudMu.Unlock()
	return p.flushFraudOutboxLocked(ctx)
}

func (p *Plugin) flushFraudOutboxLocked(ctx context.Context) error {
	p.mu.RLock()
	mode := p.config.Mode
	service := p.service
	session := p.session
	p.mu.RUnlock()
	if mode != "slave" {
		return fmt.Errorf("anti-fraud events may be reported only by a slave")
	}
	if service == nil || service.store == nil {
		return fmt.Errorf("replication anti-fraud outbox is unavailable")
	}
	if session == nil {
		return nil
	}
	events, err := service.store.PendingFraudEvents(ctx, 100)
	if err != nil {
		return err
	}
	if len(events) == 0 {
		return nil
	}
	payload, err := json.Marshal(events)
	if err != nil {
		return err
	}
	return session.send(protocol.Frame{Kind: protocol.KindFraudEvents, Payload: payload})
}

func (p *Plugin) acknowledgeFraudEvents(ctx context.Context, eventIDs []string) error {
	p.fraudMu.Lock()
	defer p.fraudMu.Unlock()

	p.mu.RLock()
	mode := p.config.Mode
	service := p.service
	p.mu.RUnlock()
	if mode != "slave" {
		return fmt.Errorf("anti-fraud events may be acknowledged only by a slave")
	}
	if service == nil || service.store == nil {
		return fmt.Errorf("replication anti-fraud outbox is unavailable")
	}
	if err := service.store.AcknowledgeFraudEvents(ctx, eventIDs); err != nil {
		return err
	}
	return p.flushFraudOutboxLocked(ctx)
}

func (p *Plugin) SetFraudEventSink(sink func(context.Context, string, []pluginapi.FraudEvent) error) {
	p.mu.Lock()
	p.fraudSink = sink
	p.mu.Unlock()
}

func (p *Plugin) Stop(_ context.Context) error {
	p.mu.Lock()
	stop := p.stopGRPC
	p.stopGRPC = nil
	p.running = false
	p.mu.Unlock()
	if stop != nil {
		return stop()
	}
	return nil
}

func (p *Plugin) Health(ctx context.Context) error {
	p.mu.RLock()
	service := p.service
	config := p.config
	running := p.running
	p.mu.RUnlock()
	if service == nil || !running {
		return fmt.Errorf("cluster replication is not running")
	}
	if config.Mode == "master" {
		_, err := service.store.LatestRevision(ctx)
		return err
	}
	_, _, err := service.store.GetMeta(ctx, "applied_revision")
	return err
}

func (p *Plugin) Status(ctx context.Context) (Status, error) {
	p.mu.RLock()
	service := p.service
	config := p.config
	p.mu.RUnlock()
	if service == nil {
		return Status{}, fmt.Errorf("cluster replication is not initialized")
	}
	latest, err := service.store.LatestRevision(ctx)
	if err != nil {
		return Status{}, err
	}
	return Status{
		Mode:            config.Mode,
		NodeID:          config.NodeID,
		LatestRevision:  latest,
		AppliedRevision: currentRevision(ctx, service.store),
	}, nil
}

func (p *Plugin) CLICommands() []pluginapi.CLICommand {
	return []pluginapi.CLICommand{
		{Name: "status", Use: "status", Short: "Show replication revisions and role."},
		{Name: "snapshot", Use: "snapshot", Short: "Request a new streamed snapshot from the master."},
	}
}

func (p *Plugin) RunCLI(ctx context.Context, name string, _ []string) (string, error) {
	if err := p.ensureCLI(ctx); err != nil {
		return "", err
	}
	switch name {
	case "status":
		status, err := p.Status(ctx)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("mode=%s node=%s latest_revision=%d applied_revision=%d", status.Mode, status.NodeID, status.LatestRevision, status.AppliedRevision), nil
	case "snapshot":
		p.mu.RLock()
		service := p.service
		mode := p.config.Mode
		p.mu.RUnlock()
		if mode != "master" {
			return "", fmt.Errorf("snapshot command is available only on the master")
		}
		if err := service.RequestSnapshot(ctx, "manual"); err != nil {
			return "", err
		}
		return "replication snapshot marker appended", nil
	default:
		return "", fmt.Errorf("unknown cluster_replication command %q", name)
	}
}

func (p *Plugin) ensureCLI(ctx context.Context) error {
	p.mu.RLock()
	initialized := p.service != nil
	cliConfig := p.cliConfig
	p.mu.RUnlock()
	if initialized {
		return nil
	}
	if cliConfig == nil {
		return fmt.Errorf("cluster replication is not initialized")
	}
	entry, exists := cliConfig.Plugins["cluster_replication"]
	if !exists || !entry.Enabled {
		return fmt.Errorf("cluster_replication is disabled in configuration")
	}
	config, err := parseConfig(entry.Config)
	if err != nil {
		return err
	}
	if err := config.Validate(); err != nil {
		return err
	}
	db, err := database.NewConnection(database.Config{
		Driver: cliConfig.Database.Driver, DSN: cliConfig.Database.DSN, SQLitePath: cliConfig.Database.SQLitePath, Silent: true,
	})
	if err != nil {
		return fmt.Errorf("open replication database: %w", err)
	}
	runner, ok := database.NewPluginDBFactory(db)("cluster_replication").(pluginapi.EmbeddedMigrationRunner)
	if !ok {
		return fmt.Errorf("cluster replication migrations are unavailable")
	}
	if err := runner.RunEmbeddedMigrations(ctx, p.PluginMigrations()); err != nil {
		return err
	}
	registry := database.NewRegistry(db)
	service := NewService(registry, &vpn.NoopEngine{}, NewStore(db), slog.Default())
	p.mu.Lock()
	if p.service == nil {
		p.config = config
		p.service = service
		p.log = slog.Default().With("plugin", "cluster_replication", "node", config.NodeID)
	}
	p.mu.Unlock()
	return nil
}

func parseConfig(raw pluginapi.RawConfig) (Config, error) {
	config := Config{
		ReconnectInterval:  "5s",
		DriftInterval:      "1m",
		MasterScanInterval: "30s",
		StatsInterval:      "30s",
	}
	if raw == nil {
		return config, nil
	}
	readString := func(key string, target *string) error {
		value, exists := raw[key]
		if !exists {
			return nil
		}
		text, ok := value.(string)
		if !ok {
			return fmt.Errorf("%s must be a string, got %T", key, value)
		}
		*target = text
		return nil
	}
	for _, field := range []struct {
		key    string
		target *string
	}{
		{"mode", &config.Mode}, {"node_id", &config.NodeID}, {"listen_address", &config.ListenAddress},
		{"master_address", &config.MasterAddress}, {"ca_file", &config.CAFile}, {"cert_file", &config.CertFile},
		{"key_file", &config.KeyFile}, {"server_name", &config.ServerName}, {"reconnect_interval", &config.ReconnectInterval},
		{"drift_interval", &config.DriftInterval}, {"master_scan_interval", &config.MasterScanInterval}, {"stats_interval", &config.StatsInterval},
		{"reality_keys_path", &config.RealityKeysPath},
	} {
		if err := readString(field.key, field.target); err != nil {
			return Config{}, err
		}
	}
	if value, exists := raw["allowed_nodes"]; exists {
		items, ok := value.([]string)
		if !ok {
			untyped, untypedOK := value.([]any)
			if !untypedOK {
				return Config{}, fmt.Errorf("allowed_nodes must be an array of strings, got %T", value)
			}
			items = make([]string, 0, len(untyped))
			for _, item := range untyped {
				text, textOK := item.(string)
				if !textOK {
					return Config{}, fmt.Errorf("allowed_nodes must contain strings, got %T", item)
				}
				items = append(items, text)
			}
		}
		config.AllowedNodes = append([]string(nil), items...)
	}
	for _, pair := range []struct{ name, value string }{
		{"reconnect_interval", config.ReconnectInterval}, {"drift_interval", config.DriftInterval}, {"master_scan_interval", config.MasterScanInterval}, {"stats_interval", config.StatsInterval},
	} {
		if value, err := time.ParseDuration(pair.value); err != nil || value <= 0 {
			return Config{}, fmt.Errorf("%s must be a positive duration", pair.name)
		}
	}
	config.Mode = strings.ToLower(strings.TrimSpace(config.Mode))
	return config, nil
}

var _ pluginapi.Plugin = (*Plugin)(nil)
var _ pluginapi.ServiceProvider = (*Plugin)(nil)
var _ pluginapi.CLIContributor = (*Plugin)(nil)
var _ pluginapi.ReplicationFraudRelay = (*Plugin)(nil)
