package pluginhost

import (
	"context"
	"fmt"

	"xraytool/internal/pluginapi"
)

// PluginDBFactory creates a database handle scoped to exactly one plugin.
// The Host owns the factory and invokes it only for enabled plugins while
// loading them; disabled entries never receive a handle or run migrations.
type PluginDBFactory func(pluginName string) pluginapi.PluginDBHandle

// HostOption configures a Host at construction time. Options are applied
// before Load and preserve the original New signature for lightweight tests
// and hosts that intentionally do not have a database.
type HostOption func(*Host)

// WithPluginDBFactory wires the kernel-owned connection pool into the Host
// through scoped handles. The factory should be created from the composition
// root (database.NewPluginDBFactory), never by an individual plugin.
func WithPluginDBFactory(factory PluginDBFactory) HostOption {
	return func(host *Host) {
		host.pluginDBFactory = factory
	}
}

func (h *Host) databaseHandle(pluginName string) pluginapi.PluginDBHandle {
	if h == nil || h.pluginDBFactory == nil {
		return nil
	}
	return h.pluginDBFactory(pluginName)
}

func runPluginMigrations(ctx context.Context, pluginName string, plugin pluginapi.Plugin, db pluginapi.PluginDBHandle) error {
	if db == nil {
		return nil
	}
	provider, ok := plugin.(pluginapi.MigrationProvider)
	if !ok {
		return nil
	}
	migrations := provider.PluginMigrations()
	if runner, ok := db.(pluginapi.EmbeddedMigrationRunner); ok {
		if err := runner.RunEmbeddedMigrations(ctx, migrations); err != nil {
			return fmt.Errorf("plugin %q migrations: %w", pluginName, err)
		}
		return nil
	}
	return fmt.Errorf(
		"plugin %q declares embedded migrations but its database handle %T cannot run them",
		pluginName,
		db,
	)
}
