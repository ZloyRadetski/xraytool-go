package core

import (
	"embed"

	"xraytool/internal/pluginapi"
)

// builtinMigrationFiles keeps the core schema migration available in an
// installed binary, where the repository's internal/plugins tree is absent.
//
//go:embed migrations/*.up.sql
var builtinMigrationFiles embed.FS

// PluginMigrations declares the core-owned schema version stream.
func (*Plugin) PluginMigrations() pluginapi.MigrationSet {
	return pluginapi.MigrationSet{FS: builtinMigrationFiles, Dir: "migrations"}
}

var _ pluginapi.MigrationProvider = (*Plugin)(nil)
