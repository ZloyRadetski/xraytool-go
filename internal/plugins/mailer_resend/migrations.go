package mailer_resend

import (
	"embed"

	"xraytool/internal/pluginapi"
)

//go:embed migrations/*.up.sql
var builtinMigrationFiles embed.FS

// PluginMigrations keeps notification-provider persistence, if introduced,
// in a namespace independent from core. Resend itself is stateless today.
func (*Plugin) PluginMigrations() pluginapi.MigrationSet {
	return pluginapi.MigrationSet{FS: builtinMigrationFiles, Dir: "migrations"}
}

var _ pluginapi.MigrationProvider = (*Plugin)(nil)
