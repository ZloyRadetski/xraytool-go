//go:build !minimal

package cmd

import (
	"log/slog"

	"xraytool/internal/domain"
	"xraytool/internal/pluginapi"
	antifraudPlugin "xraytool/internal/plugins/antifraud"
	clusterreplication "xraytool/internal/plugins/cluster_replication"
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
	factories["antifraud"] = func() pluginapi.Plugin {
		return antifraudPlugin.NewWithRuntime(antifraudPlugin.Runtime{
			Registry:   deps.Registry,
			Banner:     engine,
			LoggerCtl:  engine,
			Propagator: deps.Propagator,
			Reporter:   nil,
		})
	}
	factories["cluster_replication"] = func() pluginapi.Plugin {
		return clusterreplication.NewWithRuntime(clusterreplication.Runtime{
			Registry:  deps.Registry,
			Engine:    engine,
			AppConfig: deps.Cfg,
			Service:   deps.ReplicationService,
			Logger:    slog.Default(),
		})
	}
}
