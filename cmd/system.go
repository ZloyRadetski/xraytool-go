package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"
)

// updateXrayCmd downloads the latest xray-core binary.
func updateXrayCmd(deps *AppDeps) *cobra.Command {
	return &cobra.Command{
		Use:   "update-xray",
		Short: "Update xray-core to the latest version",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireRoot(); err != nil {
				return err
			}
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
func updateGeoCmd(deps *AppDeps) *cobra.Command {
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
					dest: deps.Cfg.Paths.GeoIPDat,
				},
				{
					url:  "https://github.com/Loyalsoldier/v2ray-rules-dat/releases/latest/download/geosite.dat",
					dest: deps.Cfg.Paths.GeositeDat,
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

func migrateCmd(deps *AppDeps) *cobra.Command {
	var legacy bool

	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Clean legacy fields from config and sync all users with current templates",
		RunE: func(cmd *cobra.Command, _ []string) error {
			requireRoot()

			engine := deps.Engine
			users, err := engine.ListUsers(context.Background())
			if err != nil {
				return err
			}
			fmt.Printf("[INFO] Found %d users. Cleaning config…\n", len(users))

			for _, u := range users {
				if u.Email == "" {
					continue
				}
				// By banning and adding back, the engine uses the latest templates
				_ = engine.BanUser(context.Background(), u.Email)
				_ = engine.AddUser(context.Background(), u)
				fmt.Printf("  [SYNCED] %s\n", u.Email)
			}

			if legacy {
				systemctlRestart("xray")
			}
			fmt.Println("[OK] Migration complete. Review config before restarting if needed.")
			return nil
		},
	}

	cmd.Flags().BoolVar(&legacy, "legacy", false, "Restart engine after migration")
	return cmd
}
