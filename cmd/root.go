// Package cmd implements the xraytool CLI using cobra.
package cmd

import (
	"fmt"
	"os"
	"runtime"

	"xraytool/internal/appconfig"
	"xraytool/internal/database"
	"xraytool/internal/logger"

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
	Run: func(cmd *cobra.Command, args []string) {
		cmd.Help()
	},
}

// Execute runs the root command.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		osExit(1)
	}
}

func init() {
	rootCmd.PersistentFlags().StringVar(
		&cfgFile, "config", "/etc/xraytool/config.yaml",
		"Path to xraytool config file",
	)

	// loadConfig is run before every subcommand.
	cobra.OnInitialize(loadConfig)

	rootCmd.AddCommand(
		newUserCmd(),
		rmUserCmd(),
		limitCmd(),
		unlimitCmd(),
		setExpireCmd(),
		updateLimitCmd(),
		userListCmd(),
		shareLinkCmd(),
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

func loadConfig() {
	// Skip if no subcommand was given (just --help, etc.)
	if len(os.Args) < 2 {
		return
	}
	// Skip config load for help flags
	if len(os.Args) == 2 && (os.Args[1] == "--help" || os.Args[1] == "-h" || os.Args[1] == "help") {
		return
	}

	var err error
	cfg, err = appconfig.Load(cfgFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR|failed to load config %q: %v\n", cfgFile, err)
		osExit(1)
	}

	if err := logger.Init(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "WARN|failed to initialize logger: %v\n", err)
	}

	isServerOrMigrate := false
	if len(os.Args) > 1 && (os.Args[1] == "start-server" || os.Args[1] == "migrate" || os.Args[1] == "db-migrate") {
		isServerOrMigrate = true
	}

	if err := database.Init(database.Config{
		Driver:     cfg.Database.Driver,
		DSN:        cfg.Database.DSN,
		SQLitePath: cfg.Database.SQLitePath,
		Silent:     !isServerOrMigrate,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "WARN|failed to initialize database: %v\n", err)
	}
}

var geteuid = func() int { return os.Geteuid() }
var currentGOOS = runtime.GOOS

// requireRoot exits if the process is not running as root.
func requireRoot() {
	if currentGOOS == "windows" {
		return
	}
	if geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "ERROR|Script must be run as root")
		osExit(1)
	}
}

const banner = `
  ██╗  ██╗██████╗  █████╗ ██╗   ██╗████████╗ ██████╗  ██████╗ ██╗
  ╚██╗██╔╝██╔══██╗██╔══██╗╚██╗ ██╔╝╚══██╔══╝██╔═══██╗██╔═══██╗██║
   ╚███╔╝ ██████╔╝███████║ ╚████╔╝    ██║   ██║   ██║██║   ██║██║
   ██╔██╗ ██╔══██╗██╔══██║  ╚██╔╝     ██║   ██║   ██║██║   ██║██║
  ██╔╝ ██╗██║  ██║██║  ██║   ██║      ██║   ╚██████╔╝╚██████╔╝███████╗
  
  Go Edition
`
