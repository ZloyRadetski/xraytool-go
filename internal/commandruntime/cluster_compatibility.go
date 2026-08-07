//go:build !minimal

package commandruntime

import (
	"context"
	"log/slog"

	"xraytool/internal/appconfig"
	"xraytool/internal/domain"
	"xraytool/internal/plugins/clustersync/slave"
	"xraytool/internal/plugins/clustersync/statesync"
)

func configureClusterCompatibility(deps *Dependencies) {
	cfg := deps.Cfg
	if cfg == nil {
		return
	}

	var slaveRegistry *slave.Registry
	if len(cfg.SlaveServers) > 0 {
		client := slave.NewClient(cfg.SlaveAPI.ConnectTimeout, cfg.SlaveAPI.RequestTimeout, cfg.SlaveAPI.RemotePath)
		slaveRegistry = slave.NewRegistry(toSlaveEntries(cfg.SlaveServers), client)
		deps.Propagator = slave.NewEventPropagatorAdapter(slaveRegistry)
	}
	if !cfg.IsMaster() {
		return
	}
	if slaveRegistry == nil {
		client := slave.NewClient(cfg.SlaveAPI.ConnectTimeout, cfg.SlaveAPI.RequestTimeout, cfg.SlaveAPI.RemotePath)
		slaveRegistry = slave.NewRegistry(toSlaveEntries(cfg.SlaveServers), client)
	}
	syncService := statesync.NewService(deps.Registry, deps.Engine, nil, slog.Default())
	deps.SyncSvc = syncService
	deps.Engine = statesync.NewEventAwareEngine(deps.Engine, syncService)
	deps.ClusterProvider = slave.NewClusterStatsProvider(slaveRegistry)
	deps.SlaveProvider = slave.NewStateSyncProvider(
		slaveRegistry, syncService, cfg.Reality.RotationEnabled, cfg.Reality.KeysFilepath, slog.Default(),
	)
	syncService.SetSlaveProvider(deps.SlaveProvider)
}

func toSlaveEntries(entries map[string]appconfig.SlaveEntry) map[string]slave.Entry {
	if len(entries) == 0 {
		return nil
	}
	result := make(map[string]slave.Entry, len(entries))
	for name, entry := range entries {
		result[name] = slave.Entry{
			URL: entry.URL, Domain: entry.Domain, Host: entry.Host, IP: entry.IP,
			Scheme: entry.Scheme, Port: entry.Port, Path: entry.Path,
			APIKey: entry.APIKey, APIKeyCamel: entry.APIKeyCamel, XAPIKey: entry.XAPIKey,
			XAPIKeyCamel: entry.XAPIKeyCamel, Token: entry.Token, APIToken: entry.APIToken,
			Bearer: entry.Bearer, BearerToken: entry.BearerToken, BearerTokenCamel: entry.BearerTokenCamel,
			AuthHeader: entry.AuthHeader, Authorization: entry.Authorization,
			Insecure: entry.Insecure, AllowInsecure: entry.AllowInsecure,
		}
	}
	return result
}

// StartFraudReporter creates and starts the slave-to-master antifraud reporter
// when the configured node needs one.
func (deps *Dependencies) StartFraudReporter(ctx context.Context) domain.FraudEventReporter {
	if deps == nil || deps.Cfg == nil {
		return nil
	}
	cfg := deps.Cfg
	if !cfg.AntiFraud.Enabled || cfg.IsMaster() || cfg.MasterAPI.URL == "" || !cfg.AntiFraud.ReportToMaster {
		return nil
	}
	client := slave.NewClient(cfg.SlaveAPI.ConnectTimeout, cfg.SlaveAPI.RequestTimeout, cfg.SlaveAPI.RemotePath)
	reporter := slave.NewFraudReporterAdapter(client, slave.Entry{
		URL: cfg.MasterAPI.URL, APIKey: cfg.MasterAPI.APIKey, Insecure: cfg.MasterAPI.Insecure,
	}, slog.Default())
	go reporter.Run(ctx)
	return reporter
}

func (deps *Dependencies) SyncServiceFor(engine domain.Engine) SyncService {
	if deps == nil {
		return nil
	}
	if deps.SyncSvc != nil {
		return deps.SyncSvc
	}
	return statesync.NewService(deps.Registry, engine, deps.SlaveProvider, slog.Default())
}
