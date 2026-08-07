package pluginhost_test

import (
	"context"
	"io/fs"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"

	"xraytool/internal/pluginapi"
	"xraytool/internal/pluginhost"
)

type migrationStubPlugin struct {
	*stubPlugin
	migrations pluginapi.MigrationSet
}

func (p *migrationStubPlugin) PluginMigrations() pluginapi.MigrationSet {
	return p.migrations
}

type recordingPluginDBHandle struct {
	name         string
	embeddedRuns int
}

func (h *recordingPluginDBHandle) PluginName() string { return h.name }

func (h *recordingPluginDBHandle) RunMigrations(context.Context, string) error {
	return nil
}

func (h *recordingPluginDBHandle) RunEmbeddedMigrations(_ context.Context, migrations pluginapi.MigrationSet) error {
	if migrations.FS == nil || migrations.Dir != "migrations" {
		return fs.ErrInvalid
	}
	h.embeddedRuns++
	return nil
}

var _ pluginapi.PluginDBHandle = (*recordingPluginDBHandle)(nil)
var _ pluginapi.EmbeddedMigrationRunner = (*recordingPluginDBHandle)(nil)

func TestHostRunsScopedMigrationsOnlyForEnabledPlugins(t *testing.T) {
	coreHandle := &recordingPluginDBHandle{name: "core"}
	disabledHandle := &recordingPluginDBHandle{name: "disabled"}
	seenHandles := make([]string, 0, 2)

	core := &migrationStubPlugin{
		stubPlugin: makeCorePlugin(),
		migrations: pluginapi.MigrationSet{
			FS:  fstest.MapFS{"migrations/000001_initial.up.sql": &fstest.MapFile{Data: []byte("SELECT 1;")}},
			Dir: "migrations",
		},
	}
	core.initFn = func(_ context.Context, _ pluginapi.RawConfig, resolver pluginapi.ServiceResolver) error {
		if resolver.DB() == nil || resolver.DB().PluginName() != "core" {
			return fs.ErrInvalid
		}
		if coreHandle.embeddedRuns != 1 {
			return fs.ErrInvalid
		}
		return nil
	}

	disabled := &migrationStubPlugin{
		stubPlugin: &stubPlugin{
			meta: pluginapi.Metadata{
				Name:       "disabled",
				Kind:       "test",
				Version:    "1.0.0",
				APIVersion: pluginapi.CurrentAPIVersion,
			},
		},
		migrations: pluginapi.MigrationSet{
			FS:  fstest.MapFS{"migrations/000001_initial.up.sql": &fstest.MapFile{Data: []byte("SELECT 1;")}},
			Dir: "migrations",
		},
	}

	host := pluginhost.New(
		pluginhost.PluginsConfig{
			"core":     {Enabled: true, Source: "builtin"},
			"disabled": {Enabled: false, Source: "builtin"},
		},
		nil,
		map[string]func() pluginapi.Plugin{
			"core":     func() pluginapi.Plugin { return core },
			"disabled": func() pluginapi.Plugin { return disabled },
		},
		nil,
		pluginhost.WithPluginDBFactory(func(name string) pluginapi.PluginDBHandle {
			seenHandles = append(seenHandles, name)
			switch name {
			case "core":
				return coreHandle
			case "disabled":
				return disabledHandle
			default:
				return nil
			}
		}),
	)

	require.NoError(t, host.Load(context.Background()))
	require.Equal(t, 1, coreHandle.embeddedRuns)
	require.Zero(t, disabledHandle.embeddedRuns)
	require.Equal(t, []string{"core"}, seenHandles)
	require.NoError(t, host.Shutdown(context.Background()))
}

var _ pluginapi.MigrationProvider = (*migrationStubPlugin)(nil)
