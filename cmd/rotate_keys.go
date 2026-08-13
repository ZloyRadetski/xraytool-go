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
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireRoot(); err != nil {
				return err
			}

			if !deps.Cfg.Reality.RotationEnabled {
				return fmt.Errorf("reality.rotation_enabled is not enabled in config")
			}

			keysPath := deps.Cfg.Reality.KeysFilepath
			if keysPath == "" {
				return fmt.Errorf("reality.keys_filepath is not configured")
			}

			fmt.Println("INFO|Rotating Reality keys and Short IDs...")

			// 1. Delete the old keys file
			if err := os.Remove(keysPath); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("delete old keys file: %w", err)
			}

			// 2. Generate new keys (LoadOrCreateRealityKeys will create a new one because the file is gone)
			keys, err := vpn.LoadOrCreateRealityKeys(keysPath)
			if err != nil {
				return fmt.Errorf("generate new keys: %w", err)
			}

			fmt.Println("INFO|New keys generated successfully.")
			fmt.Printf("  Public Key: %s\n", keys.PublicKey)
			fmt.Println("  Short IDs count:", len(keys.ShortIDs))

			// 3. The replication plugin reads the new key artifact and sends it over
			// its mTLS stream; no HTTP push of key material is used.
			if deps.Cfg.IsMaster() {
				if deps.ReplicationService == nil {
					fmt.Println("INFO|cluster_replication is not configured; key sync is skipped")
				} else if _, err := deps.ReplicationService.PublishArtifacts(cmd.Context(), keysPath); err != nil {
					return fmt.Errorf("publish key artifact: %w", err)
				}
				fmt.Println("INFO|Keys successfully rotated; connected slaves will receive the artifact.")
			} else {
				fmt.Println("INFO|Keys rotated locally. Note: Slaves should receive keys from Master instead of rotating locally.")
			}
			return nil
		},
	}

	return cmd
}
