//go:build !minimal

package cmd

import (
	"context"
	"fmt"
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
	// Both plugins are initialised before either Start goroutine runs. The
	// closure resolves the provider at delivery time so replication remains
	// usable when anti-fraud is intentionally disabled.
	var antifraudProvider *antifraudPlugin.Plugin
	factories["antifraud"] = func() pluginapi.Plugin {
		antifraudProvider = antifraudPlugin.NewWithRuntime(antifraudPlugin.Runtime{
			Registry:   deps.Registry,
			Banner:     engine,
			LoggerCtl:  engine,
			Propagator: deps.Propagator,
			Reporter:   nil,
		})
		return antifraudProvider
	}
	factories["cluster_replication"] = func() pluginapi.Plugin {
		return clusterreplication.NewWithRuntime(clusterreplication.Runtime{
			Registry: deps.Registry,
			Engine:   engine,
			Service:  deps.ReplicationService,
			Logger:   slog.Default(),
			FraudSink: func(ctx context.Context, sourceID string, events []pluginapi.FraudEvent) error {
				if antifraudProvider == nil {
					return fmt.Errorf("anti-fraud plugin is not enabled on the master")
				}
				return antifraudProvider.IngestEvents(ctx, sourceID, events)
			},
		})
	}
}
