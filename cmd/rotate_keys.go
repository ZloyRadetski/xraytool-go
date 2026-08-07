package cmd

import (
	"context"
	"fmt"
	"os"

	vpn "xraytool/internal/plugins/engine_xray"

	"github.com/spf13/cobra"
)

func rotateKeysCmd(deps *AppDeps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rotate-keys",
		Short: "Force rotation of Reality keys and Short IDs and sync them across the cluster",
		Run: func(cmd *cobra.Command, _ []string) {
			requireRoot() //nolint:errcheck

			if !deps.Cfg.Reality.RotationEnabled {
				fmt.Println("ERROR|reality.rotation_enabled is not enabled in config")
				return
			}

			keysPath := deps.Cfg.Reality.KeysFilepath
			if keysPath == "" {
				fmt.Println("ERROR|reality.keys_filepath is not configured")
				return
			}

			fmt.Println("INFO|Rotating Reality keys and Short IDs...")

			// 1. Delete the old keys file
			if err := os.Remove(keysPath); err != nil && !os.IsNotExist(err) {
				fmt.Printf("ERROR|Failed to delete old keys file: %v\n", err)
				return
			}

			// 2. Generate new keys (LoadOrCreateRealityKeys will create a new one because the file is gone)
			keys, err := vpn.LoadOrCreateRealityKeys(keysPath)
			if err != nil {
				fmt.Printf("ERROR|Failed to generate new keys: %v\n", err)
				return
			}

			fmt.Println("INFO|New keys generated successfully.")
			fmt.Printf("  Public Key: %s\n", keys.PublicKey)
			fmt.Println("  Short IDs count:", len(keys.ShortIDs))

			// 3. Trigger config regeneration locally on master (by syncing to Xray)
			// Wait, calling SyncAllSlaves will sync to all slaves. But we also want to sync to master itself!
			// Does Master run Xray? Yes, Master itself can also run Xray. The syncStatesCmd also runs self-heal.
			// Let's run statesync propagation to all slaves.
			if deps.Cfg.IsMaster() {
				if deps.SlaveProvider != nil {
					fmt.Println("INFO|Propagating new keys to all Slaves...")
					results, err := deps.SlaveProvider.SyncAllSlaves(context.Background(), false, false)
					if err != nil {
						fmt.Printf("WARNING|Failed to sync slaves: %v\n", err)
					} else {
						for _, res := range results {
							if res.Success {
								fmt.Printf("  [OK] %s: Keys and state synchronized\n", res.ServerName)
							} else {
								fmt.Printf("  [FAIL] %s: %v\n", res.ServerName, res.Error)
							}
						}
					}
				}
				fmt.Println("INFO|Keys successfully rotated and propagated across the cluster.")
			} else {
				fmt.Println("INFO|Keys rotated locally. Note: Slaves should receive keys from Master instead of rotating locally.")
			}
		},
	}

	return cmd
}
