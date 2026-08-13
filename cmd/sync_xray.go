package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

func syncXrayCmd(deps *AppDeps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sync-xray",
		Short: "Sync UUIDs from DB to Engine",
		Long: `Updates the UUIDs of existing clients in the engine
based on the subscription UUIDs stored in the SQLite database. Does not delete any clients.
Only modifies existing clients in the engine that match by email.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireRoot(); err != nil {
				return err
			}

			if deps.ReplicationService == nil {
				return fmt.Errorf("cluster_replication is not configured on this master")
			}
			users, err := deps.ReplicationService.BuildSnapshot(cmd.Context())
			if err != nil {
				return fmt.Errorf("failed to build desired users: %w", err)
			}
			// This command promises an in-place UUID sync. Do not remove engine
			// users simply because a transient DB snapshot omitted them.
			result, err := deps.Engine.SyncUsers(cmd.Context(), users, false)
			if err != nil {
				return fmt.Errorf("failed to sync engine: %w", err)
			}
			fmt.Printf("[OK] Sync complete: %d added, %d removed.\n", result.Added, result.Removed)

			return nil
		},
	}
	return cmd
}
