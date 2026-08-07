package payment_platega

import (
	"embed"

	"xraytool/internal/pluginapi"
)

//go:embed migrations/*.up.sql
var builtinMigrationFiles embed.FS

// PluginMigrations reserves provider-local migration ownership. Canonical
// payment records remain owned by the core plugin.
func (*Plugin) PluginMigrations() pluginapi.MigrationSet {
	return pluginapi.MigrationSet{FS: builtinMigrationFiles, Dir: "migrations"}
}

var _ pluginapi.MigrationProvider = (*Plugin)(nil)
