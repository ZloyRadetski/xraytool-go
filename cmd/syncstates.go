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
		Run: func(cmd *cobra.Command, _ []string) {
			requireRoot() //nolint:errcheck

			if !deps.Cfg.IsMaster() || deps.ReplicationService == nil {
				fmt.Println("ERROR|cluster_replication is not configured on this master")
				return
			}
			users, err := deps.ReplicationService.BuildSnapshot(cmd.Context())
			if err != nil {
				fmt.Printf("ERROR|building desired state: %v\n", err)
				return
			}
			if dryRun {
				fmt.Printf("INFO|Would reconcile %d users and append a streamed snapshot marker.\n", len(users))
				return
			}
			// The replication service keeps the actual local engine, before the
			// master publishing wrapper. Reconcile through it so a template-only
			// repair is never skipped by the engine's desired-state hash.
			if err := deps.ReplicationService.ReconcileDesiredState(cmd.Context()); err != nil {
				fmt.Printf("ERROR|reconciling master: %v\n", err)
				return
			}
			// Publish the template-derived static-client artifact before adding a
			// snapshot marker. The marker's streamed snapshot must already contain
			// master-only hardcoded users, otherwise a newly connected slave could
			// rebuild from its own empty template first and receive them later.
			if _, err := deps.ReplicationService.PublishArtifacts(cmd.Context(), deps.Cfg.Reality.KeysFilepath); err != nil {
				fmt.Printf("ERROR|publishing replication artifacts: %v\n", err)
				return
			}
			changed, err := deps.ReplicationService.DetectDesiredState(cmd.Context())
			if err != nil {
				fmt.Printf("ERROR|recording snapshot: %v\n", err)
				return
			}
			if forceFull && !changed {
				// The durable marker is intentionally compact; connected slaves expand
				// it into their own streamed snapshot rather than accepting a JSON blob.
				err = deps.ReplicationService.RequestSnapshot(cmd.Context(), "manual")
			}
			if err != nil {
				fmt.Printf("ERROR|recording forced snapshot: %v\n", err)
				return
			}
			fmt.Println("[OK] Master reconciled; slaves will receive the next streamed snapshot over gRPC.")
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print changes without applying them")
	cmd.Flags().BoolVar(&forceFull, "full", false, "Append a new streamed snapshot marker")
	return cmd
}
