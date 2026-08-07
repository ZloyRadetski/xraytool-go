package engine_xray

import (
	"embed"

	"xraytool/internal/pluginapi"
)

//go:embed migrations/*.up.sql
var builtinMigrationFiles embed.FS

// PluginMigrations keeps a separate version namespace even though Xray's
// runtime configuration is file-based and currently owns no SQL tables.
func (*Plugin) PluginMigrations() pluginapi.MigrationSet {
	return pluginapi.MigrationSet{FS: builtinMigrationFiles, Dir: "migrations"}
}

var _ pluginapi.MigrationProvider = (*Plugin)(nil)
