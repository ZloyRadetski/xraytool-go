// Package cmd implements the xraytool CLI using cobra.
package cmd

import (
	"fmt"
	"log/slog"
	"os"
	"runtime"

	"github.com/spf13/cobra"

	"xraytool/internal/appconfig"
	"xraytool/internal/database"
	"xraytool/internal/domain"
	"xraytool/internal/events"
	"xraytool/internal/logger"
	"xraytool/internal/payment"
	"xraytool/internal/slave"
	"xraytool/internal/statesync"
	"xraytool/internal/user"
	"xraytool/internal/vpn"
)

type AppDeps struct {
	Cfg             *appconfig.Config
	Registry        domain.Registry
	Engine          domain.Engine
	Dispatcher      *events.Dispatcher
	UserSvc         *user.Service
	PaymentSvc      *payment.Service
	Propagator      domain.EventPropagator
	ClusterProvider domain.ClusterStatsProvider
	SlaveProvider   domain.StateSyncSlaveProvider
	// SyncSvc is the state-sync service created on master nodes.
	// Passed to both the slave provider and the HTTP router.
	SyncSvc         *statesync.Service
	Cleanup         []func()
}

// RunCleanup executes all registered cleanup functions and clears the slice to prevent double execution.
func (deps *AppDeps) RunCleanup() {
	for _, cleanup := range deps.Cleanup {
		cleanup()
	}
	deps.Cleanup = nil
}

// Global flag variables
var (
	cfgFile string
)

func NewRootCmd() *cobra.Command {
	deps := &AppDeps{}

	rootCmd := &cobra.Command{
		Use:           "xraytool",
		Short:         "Xray user and traffic management tool",
		Long:          banner,
		SilenceErrors: true,
		SilenceUsage:  true,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			return loadDependencies(deps, cfgFile)
		},
		PersistentPostRun: func(cmd *cobra.Command, args []string) {
			deps.RunCleanup()
		},
		Run: func(cmd *cobra.Command, args []string) {
			cmd.Help() //nolint:errcheck
		},
	}

	rootCmd.PersistentFlags().StringVar(
		&cfgFile, "config", "/etc/xraytool/config.yaml",
		"Path to xraytool config file",
	)

	rootCmd.AddCommand(
		newUserCmd(deps),
		rmUserCmd(deps),
		limitCmd(deps),
		unlimitCmd(deps),
		setExpireCmd(deps),
		updateLimitCmd(deps),
		userListCmd(deps),
		shareLinkCmd(deps),
		statsCmd(deps),
		syncStatesCmd(deps),
		migrateCmd(deps),
		updateXrayCmd(deps),
		updateGeoCmd(deps),
		genBalancerCmd(deps),
		antiFraudStateCmd(deps),
		startServerCmd(deps),
		convertCmd(deps),
		migrateLegacyDBCmd(deps),
		syncXrayCmd(deps),
		applyBatchCmd(deps),
		rotateKeysCmd(deps),
		rebuildConfigCmd(deps),
	)

	return rootCmd
}

// Execute runs the root command.
func Execute() error {
	return NewRootCmd().Execute()
}

func loadDependencies(deps *AppDeps, configPath string) error {
	cfg, err := appconfig.Load(configPath)
	if err != nil {
		return fmt.Errorf("failed to load config %q: %v", configPath, err)
	}
	deps.Cfg = cfg

	if err := logger.Init(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "WARN|failed to initialize logger: %v\n", err)
	}

	isServerOrMigrate := false
	for _, arg := range os.Args {
		if arg == "start-server" || arg == "migrate" || arg == "db-migrate" {
			isServerOrMigrate = true
			break
		}
	}

	targetDB, err := database.NewConnection(database.Config{
		Driver:      cfg.Database.Driver,
		DSN:         cfg.Database.DSN,
		SQLitePath:  cfg.Database.SQLitePath,
		AutoMigrate: isServerOrMigrate,
		Silent:      !isServerOrMigrate,
	})
	if err != nil {
		return fmt.Errorf("failed to initialize database: %v", err)
	}
	deps.Registry = database.NewRegistry(targetDB)
	deps.Cleanup = append(deps.Cleanup, func() {
		if deps.UserSvc != nil {
			deps.UserSvc.Wait()
		}
		if sqlDB, err := targetDB.DB(); err == nil {
			sqlDB.Close()
		}
	})

	// Engine
	deps.Engine = vpn.NewAdapter(cfg.Xray.APIAddr, cfg.Paths.XrayConfig, cfg.Paths.XrayTemplate, cfg.Reality.RotationEnabled, cfg.Reality.KeysFilepath, cfg.BlacklistedAdmins, slog.Default())

	// User service
	var propagator domain.EventPropagator
	if len(cfg.SlaveServers) > 0 {
		client := slave.NewClient(cfg.SlaveAPI.ConnectTimeout, cfg.SlaveAPI.RequestTimeout, cfg.SlaveAPI.RemotePath)
		slaveReg := slave.NewRegistry(cfg.SlaveServers, client)
		propagator = slave.NewEventPropagatorAdapter(slaveReg)
	}
	// Dispatcher
	deps.Dispatcher = events.NewDispatcher(&events.Config{
		Webhooks:      cfg.Webhooks,
		WebhookSecret: cfg.WebhookSecret,
	})
	deps.Propagator = propagator

	// On master, we create syncSvc early and wrap the engine so that all services
	// (UserSvc, PaymentSvc, etc) automatically log sync events when mutating the engine.
	if cfg.IsMaster() {
		deps.SyncSvc = statesync.NewService(deps.Registry, deps.Engine, nil, slog.Default())
		deps.Engine = statesync.NewEventAwareEngine(deps.Engine, deps.SyncSvc)
	}
	deps.UserSvc = user.NewService(deps.Registry, user.Config{IsMaster: cfg.IsMaster(), Domain: cfg.Server.Domain}, deps.Engine, propagator, slog.Default())
	deps.PaymentSvc = payment.NewService(deps.Registry, deps.Dispatcher, slog.Default())

	// Providers
	if cfg.IsMaster() {
		client := slave.NewClient(cfg.SlaveAPI.ConnectTimeout, cfg.SlaveAPI.RequestTimeout, cfg.SlaveAPI.RemotePath)
		slaveReg := slave.NewRegistry(cfg.SlaveServers, client)
		deps.ClusterProvider = slave.NewClusterStatsProvider(slaveReg)
		
		deps.SlaveProvider = slave.NewStateSyncProvider(
			slaveReg,
			deps.SyncSvc,
			cfg.Reality.RotationEnabled,
			cfg.Reality.KeysFilepath,
			slog.Default(),
		)
		// Wire the provider back into syncSvc so SyncAllSlaves delegates correctly.
		deps.SyncSvc.SetSlaveProvider(deps.SlaveProvider)
	}

	return nil
}

var geteuid = func() int { return os.Geteuid() }
var currentGOOS = runtime.GOOS

func requireRoot() error {
	if currentGOOS == "windows" {
		return nil
	}
	if geteuid() != 0 {
		return fmt.Errorf("Script must be run as root")
	}
	return nil
}

const banner = `
Xraytool CLI
`
