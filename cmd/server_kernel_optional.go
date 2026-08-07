//go:build !minimal

package cmd

import (
	"context"
	"log/slog"

	"xraytool/internal/domain"
	"xraytool/internal/pluginapi"
	antifraudPlugin "xraytool/internal/plugins/antifraud"
	clustersyncPlugin "xraytool/internal/plugins/clustersync"
	"xraytool/internal/plugins/clustersync/slave"
	"xraytool/internal/plugins/clustersync/statesync"
)

// configureOptionalPluginFactories contains the only constructors that need
// optional built-in packages. Keeping it behind !minimal makes a minimal
// binary exclude their implementation rather than merely hiding them from the
// registry.
func configureOptionalPluginFactories(
	factories map[string]func() pluginapi.Plugin,
	deps *AppDeps,
	engine domain.Engine,
	_ domain.FraudEventReporter, // ignored — constructed here based on cfg
) {
	cfg := deps.Cfg

	// Build slave fraud reporter for antifraud plugin on slave nodes.
	// Lives here (not in server_kernel.go) to keep slave imports out of the
	// main kernel file.
	var fraudReporter domain.FraudEventReporter
	if cfg.AntiFraud.Enabled && !cfg.IsMaster() && cfg.MasterAPI.URL != "" && cfg.AntiFraud.ReportToMaster {
		client := slave.NewClient(cfg.SlaveAPI.ConnectTimeout, cfg.SlaveAPI.RequestTimeout, cfg.SlaveAPI.RemotePath)
		entry := slave.Entry{
			URL: cfg.MasterAPI.URL, APIKey: cfg.MasterAPI.APIKey, Insecure: cfg.MasterAPI.Insecure,
		}
		reporter := slave.NewFraudReporterAdapter(client, entry, slog.Default())
		go reporter.Run(context.Background())
		fraudReporter = reporter
	}

	factories["antifraud"] = func() pluginapi.Plugin {
		return antifraudPlugin.NewWithRuntime(antifraudPlugin.Runtime{
			Registry:   deps.Registry,
			Banner:     engine,
			LoggerCtl:  engine,
			Propagator: deps.Propagator,
			Reporter:   fraudReporter,
		})
	}
	factories["cluster_sync"] = func() pluginapi.Plugin {
		syncService, _ := deps.SyncSvc.(*statesync.Service)
		return clustersyncPlugin.NewWithRuntime(clustersyncPlugin.Runtime{
			Registry:      deps.Registry,
			Engine:        engine,
			SyncService:   syncService,
			SlaveProvider: deps.SlaveProvider,
			StatsProvider: deps.ClusterProvider,
			Propagator:    deps.Propagator,
			Logger:        slog.Default(),
		})
	}
}
