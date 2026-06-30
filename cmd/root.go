// Package cmd implements the xraytool CLI using cobra.
package cmd

import (
	"fmt"
	"os"
	"runtime"

	"xraytool/internal/appconfig"
	"xraytool/internal/database"
	"xraytool/internal/logger"
	"xraytool/internal/user"

	"github.com/spf13/cobra"
)

var (
	cfgFile string
	cfg     *appconfig.Config
)

// rootCmd is the base command.
var rootCmd = &cobra.Command{
	Use:   "xraytool",
	Short: "Xray user and traffic management tool",
	Long:  banner,
	// Silence default error printing — our printer handles it.
	SilenceErrors: true,
	SilenceUsage:  true,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		return loadConfig()
	},
	Run: func(cmd *cobra.Command, args []string) {
		cmd.Help()
	},
}

// Execute runs the root command.
func Execute() error {
	return rootCmd.Execute()
}

func init() {
	rootCmd.PersistentFlags().StringVar(
		&cfgFile, "config", "/etc/xraytool/config.yaml",
		"Path to xraytool config file",
	)

	// loadConfig is run before every subcommand.
	// Config loading is now handled in PersistentPreRunE
	// cobra.OnInitialize(loadConfig)

	getUserSvc := func() *user.Service {
		return user.NewService(database.DB(), cfg)
	}

	rootCmd.AddCommand(
		newUserCmd(getUserSvc),
		rmUserCmd(getUserSvc),
		limitCmd(getUserSvc),
		unlimitCmd(getUserSvc),
		setExpireCmd(getUserSvc),
		updateLimitCmd(getUserSvc),
		userListCmd(getUserSvc),
		shareLinkCmd(getUserSvc),
		statsCmd(),
		syncStatesCmd(),

		migrateCmd(),
		updateXrayCmd(),
		updateGeoCmd(),
		genBalancerCmd(),
		startServerCmd(),
		convertCmd(),
		migrateLegacyDBCmd(),
		syncXrayCmd(),
		applyBatchCmd(),
	)
}

func loadConfig() error {

	var err error
	cfg, err = appconfig.Load(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config %q: %v", cfgFile, err)
	}

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

	if err := database.Init(database.Config{
		Driver:      cfg.Database.Driver,
		DSN:         cfg.Database.DSN,
		SQLitePath:  cfg.Database.SQLitePath,
		AutoMigrate: isServerOrMigrate,
		Silent:      !isServerOrMigrate,
	}); err != nil {
		return fmt.Errorf("failed to initialize database: %v", err)
	}
	return nil
}

var geteuid = func() int { return os.Geteuid() }
var currentGOOS = runtime.GOOS

// requireRoot checks if the process is running as root.
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
  ██╗  ██╗██████╗  █████╗ ██╗   ██╗████████╗ ██████╗  ██████╗ ██╗
  ╚██╗██╔╝██╔══██╗██╔══██╗╚██╗ ██╔╝╚══██╔══╝██╔═══██╗██╔═══██╗██║
   ╚███╔╝ ██████╔╝███████║ ╚████╔╝    ██║   ██║   ██║██║   ██║██║
   ██╔██╗ ██╔══██╗██╔══██║  ╚██╔╝     ██║   ██║   ██║██║   ██║██║
  ██╔╝ ██╗██║  ██║██║  ██║   ██║      ██║   ╚██████╔╝╚██████╔╝███████╗
  
  Go Edition
`
