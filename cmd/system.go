package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"

	"xraytool/internal/xrayconfig"

	"github.com/spf13/cobra"
)

// updateXrayCmd downloads the latest xray-core binary.
func updateXrayCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "update-xray",
		Short: "Update xray-core to the latest version",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireRoot(); err != nil { return err }
			fmt.Println("[INFO] Downloading xray update script…")
			scriptURL := "https://github.com/XTLS/Xray-install/raw/main/install-release.sh"
			scriptPath := "/tmp/install-release-xray.sh"

			dlCmd := exec.Command("curl", "-fsSL", scriptURL, "-o", scriptPath)
			dlCmd.Stdout = os.Stdout
			dlCmd.Stderr = os.Stderr
			if err := dlCmd.Run(); err != nil {
				return fmt.Errorf("[ERROR] Failed to download script: %v", err)
			}
			defer os.Remove(scriptPath)

			fmt.Println("[INFO] Running xray update script…")
			c := exec.Command("bash", scriptPath, "install")
			if err := c.Run(); err != nil {
				return fmt.Errorf("[ERROR] Update failed: %v", err)
			}
			fmt.Println("[OK] Xray updated.")
			return nil
		},
	}
}

// updateGeoCmd updates the geoip/geosite files.
func updateGeoCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "update-geo",
		Short: "Update geoip.dat and geosite.dat files",
		RunE: func(cmd *cobra.Command, _ []string) error {
			requireRoot()
			fmt.Println("[INFO] Updating geo databases…")
			geoFiles := []struct {
				url  string
				dest string
			}{
				{
					url:  "https://github.com/Loyalsoldier/v2ray-rules-dat/releases/latest/download/geoip.dat",
					dest: cfg.Paths.GeoIPDat,
				},
				{
					url:  "https://github.com/Loyalsoldier/v2ray-rules-dat/releases/latest/download/geosite.dat",
					dest: cfg.Paths.GeositeDat,
				},
			}
			for _, f := range geoFiles {
				fmt.Printf("  Downloading %s…\n", f.dest)
				c := exec.Command("curl", "-fsSL", f.url, "-o", f.dest)
				c.Stdout = os.Stdout
				c.Stderr = os.Stderr
				if err := c.Run(); err != nil {
					fmt.Fprintf(os.Stderr, "  [WARN] Failed to download %s: %v\n", f.url, err)
				}
			}
			fmt.Println("[OK] Geo databases updated.")
			// fmt.Println("[INFO] Restarting xray…")
			// systemctlRestart("xray")
			return nil
		},
	}
}

func diffClients(oldRaw, newRaw xrayconfig.RawClient) []string {
	var diffs []string

	oldMap := make(map[string]interface{})
	newMap := make(map[string]interface{})

	for k, v := range oldRaw {
		var val interface{}
		if err := json.Unmarshal(v, &val); err == nil {
			oldMap[k] = val
		}
	}
	for k, v := range newRaw {
		var val interface{}
		if err := json.Unmarshal(v, &val); err == nil {
			newMap[k] = val
		}
	}

	for k, oldVal := range oldMap {
		newVal, exists := newMap[k]
		if !exists {
			diffs = append(diffs, fmt.Sprintf("- removed %q: %v", k, oldVal))
			continue
		}
		if fmt.Sprintf("%v", oldVal) != fmt.Sprintf("%v", newVal) {
			diffs = append(diffs, fmt.Sprintf("~ changed %q: %v -> %v", k, oldVal, newVal))
		}
	}
	for k, newVal := range newMap {
		if _, exists := oldMap[k]; !exists {
			diffs = append(diffs, fmt.Sprintf("+ added %q: %v", k, newVal))
		}
	}

	return diffs
}

// migrateCmd is used to clean up legacy fields in config.json
// and ensure all users conform to the current template structure.
func migrateCmd() *cobra.Command {
	var legacy bool

	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Clean legacy fields from config.json and sync all users with current templates",
		RunE: func(cmd *cobra.Command, _ []string) error {
			requireRoot()

			if err := xrayconfig.Modify(cfg.Paths.XrayConfig, func(xrayCfg xrayconfig.RawConfig) error {
				users, err := xrayconfig.ListUsers(xrayCfg)
				if err != nil {
					return err
				}
				fmt.Printf("[INFO] Found %d users. Cleaning config…\n", len(users))

				modifiedCount := 0
				skippedCount := 0

				for _, u := range users {
					email := u.Email()
					if email == "" {
						continue
					}

					authVal := u.GetString("auth")
					params := xrayconfig.ClientParams{
						Email:   email,
						UUID:    u.GetString("id"),
						Auth:    authVal,
						Subfile: u.GetString("subfile"),
						Expire:  u.GetString("expire"),
					}
					if lv, ok := u.GetNumber("limit"); ok {
						params.Limit = &lv
					}

					payload, err := xrayconfig.BuildForAllInbounds(xrayCfg, params)
					if err != nil {
						fmt.Printf("  [WARN] Template build failed for %s: %v\n", email, err)
						continue
					}

					// Compute diff using the first payload client as reference.
					// If a user spans multiple protocols (vless/trojan), their merged config
					// might have extraneous fields, which will be safely stripped here.
					var diffs []string
					if len(payload) > 0 {
						diffs = diffClients(u, payload[0].Client)
					}

					if len(diffs) == 0 {
						skippedCount++
						continue
					}

					// Remove from all inbounds.
					if err := xrayconfig.RemoveUserFromAllInbounds(xrayCfg, email); err != nil {
						fmt.Printf("  [WARN] Failed to remove %s: %v\n", email, err)
						continue
					}

					if err := xrayconfig.AddUserToInbounds(xrayCfg, payload); err != nil {
						fmt.Printf("  [WARN] Re-add failed for %s: %v\n", email, err)
						continue
					}

					fmt.Printf("  [MODIFIED] %s\n", email)
					for _, d := range diffs {
						fmt.Printf("      %s\n", d)
					}
					modifiedCount++
				}

				fmt.Printf("\n=== Migration summary: %d modified, %d skipped ===\n", modifiedCount, skippedCount)
				return nil
			}); err != nil {
				return fmt.Errorf("ERROR|migrate: %v", err)
			}

			if legacy {
				systemctlRestart("xray")
			}
			fmt.Println("[OK] Migration complete. Review config.json before restarting.")
			return nil
		},
	}

	cmd.Flags().BoolVar(&legacy, "legacy", false, "Restart xray after migration")
	return cmd
}
