package cmd

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"
	"xraytool/internal/templates"
	"xraytool/internal/xrayconfig"
)

// updateXrayCmd downloads the latest xray-core binary.
func updateXrayCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "update-xray",
		Short: "Update xray-core to the latest version",
		Run: func(cmd *cobra.Command, _ []string) {
			requireRoot()
			fmt.Println("[INFO] Running xray update script…")
			script := `bash -c "$(curl -fsSL https://github.com/XTLS/Xray-install/raw/main/install-release.sh)" -- install`
			c := exec.Command("bash", "-c", script)
			c.Stdout = os.Stdout
			c.Stderr = os.Stderr
			if err := c.Run(); err != nil {
				fmt.Fprintf(os.Stderr, "[ERROR] Update failed: %v\n", err)
				osExit(1)
			}
			fmt.Println("[OK] Xray updated.")
		},
	}
}

// updateGeoCmd updates the geoip/geosite files.
func updateGeoCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "update-geo",
		Short: "Update geoip.dat and geosite.dat files",
		Run: func(cmd *cobra.Command, _ []string) {
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
			fmt.Println("[INFO] Restarting xray…")
			systemctlRestart("xray")
		},
	}
}

// migrateCmd is used to clean up legacy fields in config.json
// and ensure all users conform to the current template structure.
func migrateCmd() *cobra.Command {
	var legacy bool

	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Clean legacy fields from config.json and sync all users with current templates",
		Run: func(cmd *cobra.Command, _ []string) {
			requireRoot()

			if err := xrayconfig.Modify(cfg.Paths.XrayConfig, func(xrayCfg xrayconfig.RawConfig) error {
				users, err := xrayconfig.ListUsers(xrayCfg)
				if err != nil {
					return err
				}
				fmt.Printf("[INFO] Found %d users. Cleaning config…\n", len(users))

				// Validate templates exist for all inbounds.
				if err := templates.Validate(cfg.Paths.TemplatesDir, xrayCfg); err != nil {
					return err
				}

				// Re-apply all users: remove each user and re-add using current template.
				// This strips any legacy fields.
				for _, u := range users {
					email := u.Email()
					if email == "" {
						continue
					}

					authVal := u.GetString("auth")

					// Build params from the existing config values.
					params := templates.ClientParams{
						Email:   email,
						UUID:    u.GetString("id"),
						Auth:    authVal,
						Subfile: u.GetString("subfile"),
						Expire:  u.GetString("expire"),
					}
					if lv, ok := u.GetNumber("limit"); ok {
						params.Limit = &lv
					}

					// Remove from all inbounds.
					if err := xrayconfig.RemoveUserFromAllInbounds(xrayCfg, email); err != nil {
						fmt.Printf("  [WARN] Failed to remove %s: %v\n", email, err)
						continue
					}

					// Re-add using fresh template.
					payload, err := templates.BuildForAllInbounds(cfg.Paths.TemplatesDir, xrayCfg, params)
					if err != nil {
						fmt.Printf("  [WARN] Template build failed for %s: %v\n", email, err)
						continue
					}
					if err := xrayconfig.AddUserToInbounds(xrayCfg, payload); err != nil {
						fmt.Printf("  [WARN] Re-add failed for %s: %v\n", email, err)
						continue
					}
					fmt.Printf("  [OK] %s\n", email)
				}
				return nil
			}); err != nil {
				fmt.Fprintf(os.Stderr, "ERROR|migrate: %v\n", err)
				osExit(1)
			}

			if legacy {
				systemctlRestart("xray")
			}
			fmt.Println("[OK] Migration complete. Review config.json before restarting.")
		},
	}

	cmd.Flags().BoolVar(&legacy, "legacy", false, "Restart xray after migration")
	return cmd
}
