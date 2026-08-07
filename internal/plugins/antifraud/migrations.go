package antifraud_plugin

import (
	"embed"

	"xraytool/internal/pluginapi"
)

//go:embed migrations/*.up.sql
var builtinMigrationFiles embed.FS

// PluginMigrations declares the antifraud-owned schema version stream.
func (*Plugin) PluginMigrations() pluginapi.MigrationSet {
	return pluginapi.MigrationSet{FS: builtinMigrationFiles, Dir: "migrations"}
}

var _ pluginapi.MigrationProvider = (*Plugin)(nil)
