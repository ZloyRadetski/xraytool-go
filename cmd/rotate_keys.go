package cmd

import (
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

			// 3. The replication plugin reads the new key artifact and sends it over
			// its mTLS stream; no HTTP push of key material is used.
			if deps.Cfg.IsMaster() {
				if deps.ReplicationService == nil {
					fmt.Println("WARNING|cluster_replication is not configured; keys will not be replicated")
				} else if _, err := deps.ReplicationService.PublishArtifacts(cmd.Context(), keysPath); err != nil {
					fmt.Printf("WARNING|Failed to publish key artifact: %v\n", err)
				}
				fmt.Println("INFO|Keys successfully rotated; connected slaves will receive the artifact.")
			} else {
				fmt.Println("INFO|Keys rotated locally. Note: Slaves should receive keys from Master instead of rotating locally.")
			}
		},
	}

	return cmd
}
