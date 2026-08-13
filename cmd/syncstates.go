package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

// ---------------------------------------------------------------------------
// syncstates — reconcile master config with each slave's state
// ---------------------------------------------------------------------------

func syncStatesCmd(deps *AppDeps) *cobra.Command {
	var dryRun bool
	var forceFull bool

	cmd := &cobra.Command{
		Use:   "syncstates",
		Short: "Synchronise user state from master to all slaves",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireRoot(); err != nil {
				return err
			}

			if !deps.Cfg.IsMaster() {
				return fmt.Errorf("cluster_replication is not configured on this master")
			}
			// Dry-run is a safe local snapshot inspection. It remains useful on
			// single-node masters where replication is intentionally disabled and
			// must not require a transport/outbox service that it will not use.
			if dryRun && deps.ReplicationService == nil {
				subs, err := deps.Registry.Subscriptions().FindAll(cmd.Context())
				if err != nil {
					return fmt.Errorf("build local desired state: %w", err)
				}
				users := 0
				for _, sub := range subs {
					if sub.Status == "active" && sub.Email != "" && sub.UUID != "" {
						users++
					}
				}
				fmt.Fprintf(cmd.OutOrStdout(), "INFO|Would reconcile %d users; cluster replication is disabled.\n", users)
				return nil
			}
			if deps.ReplicationService == nil {
				return fmt.Errorf("cluster_replication is not configured on this master")
			}
			users, err := deps.ReplicationService.BuildSnapshot(cmd.Context())
			if err != nil {
				return fmt.Errorf("build desired state: %w", err)
			}
			if dryRun {
				fmt.Fprintf(cmd.OutOrStdout(), "INFO|Would reconcile %d users and append a streamed snapshot marker.\n", len(users))
				return nil
			}
			// The replication service keeps the actual local engine, before the
			// master publishing wrapper. Reconcile through it so a template-only
			// repair is never skipped by the engine's desired-state hash.
			if err := deps.ReplicationService.ReconcileDesiredState(cmd.Context()); err != nil {
				return fmt.Errorf("reconcile master: %w", err)
			}
			// Publish remaining configuration artifacts (Reality keys). Hardcoded
			// template users are already regular entries in the snapshot above.
			if _, err := deps.ReplicationService.PublishArtifacts(cmd.Context(), deps.Cfg.Reality.KeysFilepath); err != nil {
				return fmt.Errorf("publish replication artifacts: %w", err)
			}
			changed, err := deps.ReplicationService.DetectDesiredState(cmd.Context())
			if err != nil {
				return fmt.Errorf("record snapshot: %w", err)
			}
			if forceFull && !changed {
				// The durable marker is intentionally compact; connected slaves expand
				// it into their own streamed snapshot rather than accepting a JSON blob.
				err = deps.ReplicationService.RequestSnapshot(cmd.Context(), "manual")
			}
			if err != nil {
				return fmt.Errorf("record forced snapshot: %w", err)
			}
			fmt.Fprintln(cmd.OutOrStdout(), "[OK] Master reconciled; slaves will receive the next streamed snapshot over gRPC.")
			return nil
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print changes without applying them")
	cmd.Flags().BoolVar(&forceFull, "full", false, "Append a new streamed snapshot marker")
	return cmd
}
