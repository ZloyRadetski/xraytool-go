package database

import (
	"context"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"

	"xraytool/internal/pluginapi"
)

func TestPluginDBHandleUsesIndependentVersionNamespaces(t *testing.T) {
	db, err := NewConnection(Config{
		Driver:     "sqlite",
		SQLitePath: "file:plugin-db-namespaces?mode=memory&cache=shared",
		Silent:     true,
	})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	factory := NewPluginDBFactory(db)
	alpha := factory("alpha")
	beta := factory("beta")
	alphaRunner, ok := alpha.(pluginapi.EmbeddedMigrationRunner)
	require.True(t, ok)
	betaRunner, ok := beta.(pluginapi.EmbeddedMigrationRunner)
	require.True(t, ok)

	alphaSet := testMigrationSet("CREATE TABLE alpha_owned (id INTEGER PRIMARY KEY);")
	betaSet := testMigrationSet("CREATE TABLE beta_owned (id INTEGER PRIMARY KEY);")
	require.NoError(t, alphaRunner.RunEmbeddedMigrations(context.Background(), alphaSet))
	require.NoError(t, betaRunner.RunEmbeddedMigrations(context.Background(), betaSet))
	// A repeat is intentionally idempotent: the version record, rather than a
	// shared global table, decides whether this plugin's script runs again.
	require.NoError(t, alphaRunner.RunEmbeddedMigrations(context.Background(), alphaSet))

	alphaTable, err := MigrationTableName("alpha")
	require.NoError(t, err)
	betaTable, err := MigrationTableName("beta")
	require.NoError(t, err)
	require.True(t, db.Migrator().HasTable(alphaTable))
	require.True(t, db.Migrator().HasTable(betaTable))
	require.True(t, db.Migrator().HasTable("alpha_owned"))
	require.True(t, db.Migrator().HasTable("beta_owned"))

	var alphaVersions, betaVersions int64
	require.NoError(t, db.Table(alphaTable).Count(&alphaVersions).Error)
	require.NoError(t, db.Table(betaTable).Count(&betaVersions).Error)
	require.EqualValues(t, 1, alphaVersions)
	require.EqualValues(t, 1, betaVersions)
}

func TestBuiltinPluginSchemaOwnershipIsScoped(t *testing.T) {
	db, err := NewConnection(Config{
		Driver:     "sqlite",
		SQLitePath: "file:plugin-db-ownership?mode=memory&cache=shared",
		Silent:     true,
	})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	factory := NewPluginDBFactory(db)
	coreRunner, ok := factory("core").(pluginapi.EmbeddedMigrationRunner)
	require.True(t, ok)
	require.NoError(t, coreRunner.RunEmbeddedMigrations(context.Background(), testMigrationSet("SELECT 1;")))

	// The core migration owns user/subscription/payment tables but must not
	// bootstrap data owned by disabled optional plugins.
	require.True(t, db.Migrator().HasTable(&User{}))
	require.True(t, db.Migrator().HasTable(&Subscription{}))
	require.True(t, db.Migrator().HasTable(&Payment{}))
	require.False(t, db.Migrator().HasTable(&AntifraudBan{}))
	require.False(t, db.Migrator().HasTable(&SyncEvent{}))

	antifraudRunner, ok := factory("antifraud").(pluginapi.EmbeddedMigrationRunner)
	require.True(t, ok)
	require.NoError(t, antifraudRunner.RunEmbeddedMigrations(context.Background(), testMigrationSet("SELECT 1;")))
	require.True(t, db.Migrator().HasTable(&AntifraudBan{}))
	require.False(t, db.Migrator().HasTable(&SyncEvent{}))
}

func TestMigrationTableNameRejectsUnsafePluginNames(t *testing.T) {
	for _, name := range []string{"", "../core", "core-plugin", "core;DROP TABLE users"} {
		_, err := MigrationTableName(name)
		require.Error(t, err, name)
	}

	table, err := MigrationTableName("payment_platega")
	require.NoError(t, err)
	require.Equal(t, "schema_migrations_payment_platega", table)
}

func testMigrationSet(sql string) pluginapi.MigrationSet {
	return pluginapi.MigrationSet{
		FS: fstest.MapFS{
			"migrations/000001_initial.up.sql": &fstest.MapFile{Data: []byte(strings.TrimSpace(sql) + "\n")},
		},
		Dir: "migrations",
	}
}
