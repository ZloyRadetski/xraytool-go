package database

import (
	"context"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"xraytool/internal/pluginapi"
)

// NewPluginDBFactory returns a host-ready factory for per-plugin database
// handles. Every produced handle shares the application's connection pool,
// but keeps an independent migration version table derived from its plugin
// name. A nil database deliberately yields a nil factory so composition roots
// cannot accidentally advertise a usable PluginDBHandle without a pool.
func NewPluginDBFactory(db *gorm.DB) func(string) pluginapi.PluginDBHandle {
	if db == nil {
		return nil
	}
	return func(pluginName string) pluginapi.PluginDBHandle {
		return &pluginDBHandle{pluginName: pluginName, db: db}
	}
}

// MigrationTableName returns the isolated version-table name for a plugin.
// Plugin identifiers are intentionally limited to lowercase letters, digits
// and underscores, matching plugin metadata/config names and preventing an
// identifier from becoming part of SQL syntax.
func MigrationTableName(pluginName string) (string, error) {
	name := strings.TrimSpace(pluginName)
	if !validPluginMigrationName(name) {
		return "", fmt.Errorf("database: invalid plugin migration namespace %q", pluginName)
	}
	return "schema_migrations_" + name, nil
}

func validPluginMigrationName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			continue
		}
		return false
	}
	return true
}

type pluginDBHandle struct {
	pluginName string
	db         *gorm.DB
}

func (h *pluginDBHandle) PluginName() string {
	if h == nil {
		return ""
	}
	return h.pluginName
}

// RunMigrations applies versioned *.up.sql files from a directory. It is the
// public path-based API used by external plugins deployed beside their own
// migration files. Built-in schema ownership hooks are intentionally not run
// here: an external plugin named like a built-in must not acquire that
// built-in's tables.
func (h *pluginDBHandle) RunMigrations(ctx context.Context, migrationsPath string) error {
	if strings.TrimSpace(migrationsPath) == "" {
		return fmt.Errorf("database: plugin %q migration path is empty", h.PluginName())
	}
	return h.runMigrations(ctx, os.DirFS(migrationsPath), ".", false)
}

// RunEmbeddedMigrations applies a compiled-in migration directory. The Host
// calls it for enabled built-in plugins before their Init methods. The
// builtinSchema flag keeps the legacy GORM model migration as a compatibility
// implementation of each built-in plugin's initial migration while the SQL
// migration directories become the durable ownership/version boundary.
func (h *pluginDBHandle) RunEmbeddedMigrations(ctx context.Context, migrations pluginapi.MigrationSet) error {
	if migrations.FS == nil {
		return fmt.Errorf("database: plugin %q supplied nil embedded migrations filesystem", h.PluginName())
	}
	dir := strings.TrimSpace(migrations.Dir)
	if dir == "" {
		return fmt.Errorf("database: plugin %q embedded migrations directory is empty", h.PluginName())
	}
	if clean := path.Clean(dir); clean == ".." || strings.HasPrefix(clean, "../") || path.IsAbs(clean) {
		return fmt.Errorf("database: plugin %q has unsafe embedded migrations directory %q", h.PluginName(), dir)
	}
	return h.runMigrations(ctx, migrations.FS, dir, true)
}

type sqlMigration struct {
	version int64
	name    string
	sql     string
}

var migrationFilename = regexp.MustCompile(`^([0-9]+)_[a-zA-Z0-9][a-zA-Z0-9_-]*\.up\.sql$`)

func (h *pluginDBHandle) runMigrations(ctx context.Context, migrationFS fs.FS, dir string, builtinSchema bool) error {
	if ctx == nil {
		return fmt.Errorf("database: plugin %q migration context must not be nil", h.PluginName())
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("database: plugin %q migrations cancelled: %w", h.PluginName(), err)
	}
	if h == nil || h.db == nil {
		return fmt.Errorf("database: plugin %q has no database connection", h.PluginName())
	}
	table, err := MigrationTableName(h.pluginName)
	if err != nil {
		return err
	}
	migrations, err := readSQLMigrations(migrationFS, dir)
	if err != nil {
		return fmt.Errorf("database: plugin %q read migrations: %w", h.pluginName, err)
	}

	return h.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := createMigrationTable(tx, table); err != nil {
			return fmt.Errorf("create migration version table %q: %w", table, err)
		}
		for _, migration := range migrations {
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("migration %s cancelled: %w", migration.name, err)
			}
			applied, err := migrationApplied(tx, table, migration.version)
			if err != nil {
				return fmt.Errorf("check migration %s: %w", migration.name, err)
			}
			if applied {
				continue
			}
			if builtinSchema {
				if err := applyBuiltinPluginSchema(tx, h.pluginName, migration.version); err != nil {
					return fmt.Errorf("apply builtin schema for migration %s: %w", migration.name, err)
				}
			}
			if strings.TrimSpace(migration.sql) != "" {
				if err := tx.Exec(migration.sql).Error; err != nil {
					return fmt.Errorf("execute migration %s: %w", migration.name, err)
				}
			}
			if err := tx.Exec(
				fmt.Sprintf("INSERT INTO %s (version, applied_at) VALUES (?, ?)", quoteIdentifier(table)),
				migration.version,
				time.Now().UTC(),
			).Error; err != nil {
				return fmt.Errorf("record migration %s: %w", migration.name, err)
			}
		}
		return nil
	})
}

func readSQLMigrations(migrationFS fs.FS, dir string) ([]sqlMigration, error) {
	entries, err := fs.ReadDir(migrationFS, dir)
	if err != nil {
		return nil, err
	}
	migrations := make([]sqlMigration, 0, len(entries))
	versions := make(map[int64]string, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		matches := migrationFilename.FindStringSubmatch(entry.Name())
		if matches == nil {
			continue
		}
		version, parseErr := strconv.ParseInt(matches[1], 10, 64)
		if parseErr != nil || version <= 0 || version == math.MinInt64 {
			return nil, fmt.Errorf("invalid migration version in %q", entry.Name())
		}
		if duplicate, exists := versions[version]; exists {
			return nil, fmt.Errorf("migration version %d is declared by both %q and %q", version, duplicate, entry.Name())
		}
		contents, readErr := fs.ReadFile(migrationFS, path.Join(dir, entry.Name()))
		if readErr != nil {
			return nil, fmt.Errorf("read %q: %w", entry.Name(), readErr)
		}
		versions[version] = entry.Name()
		migrations = append(migrations, sqlMigration{
			version: version,
			name:    entry.Name(),
			sql:     string(contents),
		})
	}
	if len(migrations) == 0 {
		return nil, fmt.Errorf("no versioned *.up.sql files found in %q", dir)
	}
	sort.Slice(migrations, func(i, j int) bool {
		return migrations[i].version < migrations[j].version
	})
	return migrations, nil
}

func createMigrationTable(db *gorm.DB, table string) error {
	return db.Exec(fmt.Sprintf(
		"CREATE TABLE IF NOT EXISTS %s (version BIGINT PRIMARY KEY NOT NULL, applied_at TIMESTAMP NOT NULL)",
		quoteIdentifier(table),
	)).Error
}

func migrationApplied(db *gorm.DB, table string, version int64) (bool, error) {
	var count int64
	err := db.Raw(
		fmt.Sprintf("SELECT COUNT(1) FROM %s WHERE version = ?", quoteIdentifier(table)),
		version,
	).Scan(&count).Error
	return count > 0, err
}

func quoteIdentifier(identifier string) string {
	return `"` + strings.ReplaceAll(identifier, `"`, `""`) + `"`
}

// applyBuiltinPluginSchema is the compatibility bridge from the historic
// single AutoMigrate call to per-plugin ownership. It intentionally knows
// only the table-owning built-ins; stateless plugins still receive and record
// their independent marker migration but do not create application tables.
func applyBuiltinPluginSchema(db *gorm.DB, pluginName string, version int64) error {
	switch pluginName {
	case "core":
		switch version {
		case 1:
			return autoMigrateCore(db)
		case 2:
			// Version 2 owns Plan.EngineIDs. Run a focused AutoMigrate so
			// installations whose original core v1 marker was already applied
			// receive the new JSON column as well.
			if err := db.AutoMigrate(&Plan{}); err != nil {
				return fmt.Errorf("auto-migrate core plan routing column: %w", err)
			}
		}
	case "antifraud":
		if version == 1 {
			if err := db.AutoMigrate(&AntifraudBan{}); err != nil {
				return fmt.Errorf("auto-migrate antifraud tables: %w", err)
			}
		}
	case "cluster_sync":
		if version == 1 {
			if err := db.AutoMigrate(&SyncEvent{}, &SyncState{}); err != nil {
				return fmt.Errorf("auto-migrate cluster_sync tables: %w", err)
			}
		}
	}
	return nil
}

func autoMigrateCore(db *gorm.DB) error {
	if err := migrateSubscriptionUUIDColumn(db); err != nil {
		return err
	}
	if err := db.AutoMigrate(
		&User{},
		&Subscription{},
		&Device{},
		&Payment{},
		&ReferralReward{},
		&SubscriptionNotification{},
		&Plan{},
		&PromoCode{},
	); err != nil {
		return fmt.Errorf("auto-migrate core tables: %w", err)
	}
	if db.Dialector.Name() == "postgres" {
		if err := db.Exec("CREATE INDEX IF NOT EXISTS idx_users_telegram_id ON users ((metadata->>'telegram_id'));").Error; err != nil {
			return fmt.Errorf("create core users telegram index: %w", err)
		}
	}
	return nil
}

var _ pluginapi.PluginDBHandle = (*pluginDBHandle)(nil)
var _ pluginapi.EmbeddedMigrationRunner = (*pluginDBHandle)(nil)
