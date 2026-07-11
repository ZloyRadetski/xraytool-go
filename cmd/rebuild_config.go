package cmd

import (
	"fmt"
	"log/slog"
	"xraytool/internal/domain"
	"xraytool/internal/statesync"
	"xraytool/internal/vpn"

	"github.com/spf13/cobra"
)

func rebuildConfigCmd(deps *AppDeps) *cobra.Command {
	var syncAll bool

	cmd := &cobra.Command{
		Use:   "rebuild-config",
		Short: "Rebuild Xray configuration file from database and template",
		Long: `Rebuilds the local xray config.json file by merging active database users into the template.
Also injects the latest Reality keys and short IDs.
If --sync is specified, it will also trigger statesync to rebuild configurations on all slave servers.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			requireRoot() //nolint:errcheck

			ctx := cmd.Context()

			// 1. Fetch active subscriptions from DB
			subs, err := deps.Registry.Subscriptions().FindAll(ctx)
			if err != nil {
				return fmt.Errorf("failed to load subscriptions from database: %w", err)
			}

			dbUsers := make([]domain.VPNUserConfig, 0, len(subs))
			for _, sub := range subs {
				if sub.Status != "active" || sub.Email == "" || sub.XrayUUID == "" {
					continue
				}
				dbUsers = append(dbUsers, vpn.SubscriptionToVPNUserConfig(sub))
			}

			// 2. Call SyncUsers (removeOrphans = true) to rewrite config.json and hot-sync Xray
			fmt.Println("INFO|Rebuilding local Xray configuration...")
			result, err := deps.Engine.SyncUsers(ctx, dbUsers, true)
			if err != nil {
				return fmt.Errorf("failed to rebuild local config: %w", err)
			}

			fmt.Printf("[OK] Local config rebuilt. %d users hot-added, %d users hot-removed.\n", result.Added, result.Removed)

			// 3. Propagate to Slaves if --sync flag is specified
			if syncAll {
				if !deps.Cfg.IsMaster() {
					fmt.Println("WARNING|--sync ignored: current node is not configured as Master")
					return nil
				}
				if deps.SlaveProvider == nil {
					fmt.Println("WARNING|--sync ignored: slave provider is not initialized")
					return nil
				}

				fmt.Println("INFO|Propagating config updates and synchronizing all Slaves...")
				svc := statesync.NewService(deps.Registry, deps.Engine, deps.SlaveProvider, slog.Default())
				results, err := svc.SyncAllSlaves(ctx, false, false)
				if err != nil {
					return fmt.Errorf("failed to sync slaves: %w", err)
				}

				for _, res := range results {
					if res.Success {
						fmt.Printf("  [OK] %s: Synchronized\n", res.ServerName)
					} else {
						fmt.Printf("  [FAIL] %s: %v\n", res.ServerName, res.Error)
					}
				}
				fmt.Println("[OK] All slaves synchronized successfully.")
			}

			return nil
		},
	}

	cmd.Flags().BoolVar(&syncAll, "sync", false, "Synchronize state to all slave servers after rebuilding local config")
	return cmd
}
