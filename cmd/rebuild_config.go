package cmd

import (
	"fmt"
	"xraytool/internal/domain"
	vpn "xraytool/internal/plugins/engine_xray"

	"github.com/spf13/cobra"
)

func rebuildConfigCmd(deps *AppDeps) *cobra.Command {
	var syncAll bool

	cmd := &cobra.Command{
		Use:   "rebuild-config",
		Short: "Rebuild Xray configuration file from database and template",
		Long: `Rebuilds the local xray config.json file by merging active database users into the template.
Also injects the latest Reality keys and short IDs.
If --sync is specified, it appends a streamed snapshot marker for connected slave nodes.`,
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
				if sub.Status != "active" || sub.Email == "" || sub.UUID == "" {
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

			// 3. Request a streamed snapshot if --sync is specified. Slaves pull it
			// through cluster_replication; no direct HTTP fan-out exists anymore.
			if syncAll {
				if !deps.Cfg.IsMaster() || deps.ReplicationService == nil {
					return fmt.Errorf("cluster_replication is not configured on this master")
				}
				if err := deps.ReplicationService.RequestSnapshot(ctx, "rebuild-config"); err != nil {
					return fmt.Errorf("request replication snapshot: %w", err)
				}
				fmt.Println("[OK] Streamed snapshot requested for connected slaves.")
			}

			return nil
		},
	}

	cmd.Flags().BoolVar(&syncAll, "sync", false, "Synchronize state to all slave servers after rebuilding local config")
	return cmd
}
