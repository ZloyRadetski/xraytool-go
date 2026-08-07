// Package commandruntime owns the transitional composition used by the CLI.
//
// The public CLI is deliberately kept free from imports of the legacy
// antifraud/payment/slave/statesync implementations.  Server commands now use
// PluginHost; the remaining command-line utilities still need the existing
// domain adapters while they are migrated to plugin services.  Keeping that
// compatibility wiring here makes the boundary explicit and gives those
// commands one place to retire from later.
package commandruntime

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"xraytool/internal/appconfig"
	"xraytool/internal/database"
	"xraytool/internal/domain"
	"xraytool/internal/events"
	"xraytool/internal/logger"
	"xraytool/internal/plugins/billing"
	"xraytool/internal/plugins/core/user"
	vpn "xraytool/internal/plugins/engine_xray"
)

// LoadOptions controls the small set of command-specific resource choices.
// PluginHostServer skips legacy core services because the core plugin owns
// them.  AutoMigrate mirrors the historic server/migration behaviour.
type LoadOptions struct {
	AutoMigrate      bool
	PluginHostServer bool
}

// Dependencies are the resources exposed to CLI command handlers during the
// migration.  Concrete legacy types are intentionally contained in this
// bridge, not constructed by package cmd.
//
// SyncSvc remains exposed temporarily because older administrative commands
// use its self-healing operation.  New server code must use the cluster_sync
// plugin instead.
type Dependencies struct {
	Cfg             *appconfig.Config
	Registry        domain.Registry
	Engine          domain.Engine
	Dispatcher      *events.Dispatcher
	UserSvc         *user.Service
	PaymentSvc      *billing.Service
	Propagator      domain.EventPropagator
	ClusterProvider domain.ClusterStatsProvider
	SlaveProvider   domain.StateSyncSlaveProvider
	SyncSvc         SyncService

	cleanup []func()
}

// SyncService is the small compatibility surface used by administrative CLI
// commands and the legacy sync worker. Keeping it as a domain-facing
// interface lets a `-tags minimal` binary omit internal/statesync entirely.
type SyncService interface {
	SelfHealMasterUUIDs(context.Context) (bool, error)
	SyncAllSlaves(context.Context, bool, bool) ([]domain.SyncResult, error)
	PurgeOldEvents(context.Context)
}

// RunCleanup executes all registered cleanup functions exactly once.
func (deps *Dependencies) RunCleanup() {
	if deps == nil {
		return
	}
	for _, cleanup := range deps.cleanup {
		cleanup()
	}
	deps.cleanup = nil
}

// Load builds the command runtime.  It is a compatibility bridge for CLI
// utilities; start-server receives PluginHostServer=true and therefore does
// not construct legacy user/payment/event services.
func Load(configPath string, options LoadOptions) (*Dependencies, error) {
	cfg, err := appconfig.Load(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load config %q: %w", configPath, err)
	}

	if err := logger.Init(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "WARN|failed to initialize logger: %v\n", err)
	}

	targetDB, err := database.NewConnection(database.Config{
		Driver:     cfg.Database.Driver,
		DSN:        cfg.Database.DSN,
		SQLitePath: cfg.Database.SQLitePath,
		// PluginHostServer delegates schema ownership to the Host. Running the
		// historic monolithic AutoMigrate here would create tables for disabled
		// plugins before their enabled state is even inspected.
		AutoMigrate: options.AutoMigrate && !options.PluginHostServer,
		Silent:      !options.AutoMigrate,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to initialize database: %w", err)
	}

	deps := &Dependencies{
		Cfg:      cfg,
		Registry: database.NewRegistry(targetDB),
	}
	deps.cleanup = append(deps.cleanup, func() {
		if deps.UserSvc != nil {
			deps.UserSvc.Wait()
		}
		if sqlDB, err := targetDB.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})

	// Engine construction stays in this migration bridge for the non-server
	// CLI.  The PluginHost server wraps this same instance in engine_xray,
	// preventing a second adapter from diverging from state synchronisation.
	deps.Engine = vpn.NewAdapter(
		cfg.Xray.APIAddr,
		cfg.Paths.XrayConfig,
		cfg.Paths.XrayTemplate,
		cfg.Reality.RotationEnabled,
		cfg.Reality.KeysFilepath,
		cfg.BlacklistedAdmins,
		slog.Default(),
	)

	configureClusterCompatibility(deps)

	if !options.PluginHostServer {
		deps.Dispatcher = events.NewDispatcher(&events.Config{})
		deps.UserSvc = user.NewService(
			deps.Registry,
			user.Config{IsMaster: cfg.IsMaster(), Domain: cfg.Server.Domain},
			deps.Engine,
			deps.Propagator,
			slog.Default(),
		)
		deps.PaymentSvc = billing.NewService(deps.Registry, deps.Dispatcher, slog.Default())
	}

	return deps, nil
}
