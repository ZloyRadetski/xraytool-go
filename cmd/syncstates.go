package cmd

import (
	"context"
	"fmt"
	"xraytool/internal/statesync"

	"github.com/spf13/cobra"
)

// ---------------------------------------------------------------------------
// syncstates — reconcile master config with each slave's state
// ---------------------------------------------------------------------------

func syncStatesCmd(deps *AppDeps) *cobra.Command {
	var dryRun bool

	cmd := &cobra.Command{
		Use:   "syncstates",
		Short: "Synchronise user state from master to all slaves",
		Run: func(cmd *cobra.Command, _ []string) {
			requireRoot() //nolint:errcheck

			if !deps.Cfg.IsMaster() {
				fmt.Println("ERROR|syncstates can only run on master node")
				return
			}

			dbReg := deps.Registry
			svc := statesync.NewService(dbReg, deps.Engine, deps.SlaveProvider)

			// Self-heal: Sync UUIDs from Database to Master config before building snapshot
			changed, err := svc.SelfHealMasterUUIDs(context.Background())
			if err != nil {
				fmt.Printf("ERROR|syncing UUIDs from DB: %v\n", err)
				// non-fatal, continue anyway
			} else if changed {
				fmt.Println("INFO|Self-healing complete.")
			}

			results, err := svc.SyncAllSlaves(context.Background(), dryRun)
			if err != nil {
				fmt.Printf("ERROR|%v\n", err)
				return
			}

			if dryRun {
				fmt.Println("INFO|Dry run completed.")
			}

			for _, res := range results {
				if res.Success {
					fmt.Printf("  [OK] %s: Synchronized\n", res.ServerName)
				} else {
					fmt.Printf("  [FAIL] %s: %v\n", res.ServerName, res.Error)
				}
			}

			fmt.Println("All slaves synchronized.")
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print changes without applying them")
	return cmd
}
