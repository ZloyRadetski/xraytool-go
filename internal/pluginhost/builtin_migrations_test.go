package pluginhost

import (
	"io/fs"
	"strings"
	"testing"

	"xraytool/internal/pluginapi"
)

func TestBuiltinPluginsExposeEmbeddedMigrationDirectories(t *testing.T) {
	for name, factory := range BuiltinRegistry(nil) {
		plugin := factory()
		provider, ok := plugin.(pluginapi.MigrationProvider)
		if !ok {
			t.Errorf("builtin plugin %q does not expose a migration set", name)
			continue
		}
		migrations := provider.PluginMigrations()
		if migrations.FS == nil || migrations.Dir == "" {
			t.Errorf("builtin plugin %q returned an empty migration set", name)
			continue
		}
		entries, err := fs.ReadDir(migrations.FS, migrations.Dir)
		if err != nil {
			t.Errorf("read migrations for builtin plugin %q: %v", name, err)
			continue
		}
		foundUpMigration := false
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".up.sql") {
				foundUpMigration = true
				break
			}
		}
		if !foundUpMigration {
			t.Errorf("builtin plugin %q has no versioned .up.sql migration", name)
		}
	}
}
