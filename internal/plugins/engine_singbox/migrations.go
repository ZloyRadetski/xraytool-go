package engine_singbox

import (
	"embed"

	"xraytool/internal/pluginapi"
)

//go:embed migrations/*.up.sql
var builtinMigrationFiles embed.FS

// PluginMigrations reserves a separate schema-version namespace for the
// Sing-box adapter. The adapter is currently file-managed, but every builtin
// plugin has an explicit migration stream so future provider-local state does
// not leak into core's schema ownership.
func (*Plugin) PluginMigrations() pluginapi.MigrationSet {
	return pluginapi.MigrationSet{FS: builtinMigrationFiles, Dir: "migrations"}
}

var _ pluginapi.MigrationProvider = (*Plugin)(nil)
