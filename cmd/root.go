// Package cmd implements the xraytool CLI using cobra.
package cmd

import (
	"fmt"
	"os"
	"runtime"

	"github.com/spf13/cobra"

	"xraytool/internal/commandruntime"
)

// AppDeps is the transitional runtime exposed to legacy administrative CLI
// commands. Its construction lives outside cmd so this package remains a thin
// command dispatcher while those commands are migrated to plugin services.
type AppDeps = commandruntime.Dependencies

// Global flag variables
var (
	cfgFile string
)

func NewRootCmd() *cobra.Command {
	deps := &AppDeps{}
	// Keep the Phase-1.1 spelling as a compatibility alias, but make the
	// PluginHost server reachable through the stable `start-server` command.
	kernelServerAlias := startServerKernelCmd(deps)
	kernelServerAlias.Hidden = true
	kernelServerAlias.Deprecated = "use start-server"

	rootCmd := &cobra.Command{
		Use:           "xraytool",
		Short:         "Xray user and traffic management tool",
		Long:          banner,
		SilenceErrors: true,
		SilenceUsage:  true,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			return loadDependencies(deps, cfgFile, cmd.Name())
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
		kernelServerAlias,
		convertCmd(deps),
		migrateLegacyDBCmd(deps),
		syncXrayCmd(deps),
		applyBatchCmd(deps),
		rotateKeysCmd(deps),
		rebuildConfigCmd(deps),
		newPluginCmd(),
	)

	return rootCmd
}

// Execute runs the root command.
func Execute() error {
	return NewRootCmd().Execute()
}

func loadDependencies(deps *AppDeps, configPath, commandName string) error {
	loaded, err := commandruntime.Load(configPath, commandruntime.LoadOptions{
		AutoMigrate:      commandName == "start-server" || commandName == "start-server-v2" || commandName == "migrate" || commandName == "db-migrate",
		PluginHostServer: commandName == "start-server" || commandName == "start-server-v2",
	})
	if err != nil {
		return err
	}
	*deps = *loaded
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
