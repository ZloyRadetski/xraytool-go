package cmd

import (
	"fmt"
	"log/slog"
	"xraytool/internal/statesync"

	"github.com/spf13/cobra"
)

func syncXrayCmd(deps *AppDeps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sync-xray",
		Short: "Sync UUIDs from DB to Engine",
		Long: `Updates the UUIDs of existing clients in the engine
based on the XrayUUIDs stored in the SQLite database. Does not delete any clients.
Only modifies existing clients in the engine that match by email.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			requireRoot() //nolint:errcheck

			svc := statesync.NewService(deps.Registry, deps.Engine, nil, slog.Default())
			changed, err := svc.SelfHealMasterUUIDs(cmd.Context())
			if err != nil {
				return fmt.Errorf("failed to sync UUIDs: %w", err)
			}

			if changed {
				fmt.Printf("[OK] Sync complete.\n")
			} else {
				fmt.Printf("[INFO] No clients needed updating.\n")
			}

			return nil
		},
	}
	return cmd
}
