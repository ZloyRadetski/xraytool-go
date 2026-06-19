package cmd

import (
	"fmt"

	"xraytool/internal/database"
	"xraytool/internal/xrayconfig"

	"github.com/spf13/cobra"
)

func syncXrayCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sync-xray",
		Short: "Sync XrayUUIDs from DB to Xray config.json",
		Long: `Updates the UUIDs (id fields) of existing clients in xray config.json 
based on the XrayUUIDs stored in the SQLite database. Does not delete any clients.
Only modifies existing clients in config.json that match by email.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			requireRoot()

			if cfg == nil {
				return fmt.Errorf("config not loaded; pass --config <path>")
			}

			// Initialize the target DB
			if err := database.Init(database.Config{
				Driver:      cfg.Database.Driver,
				DSN:         cfg.Database.DSN,
				SQLitePath:  cfg.Database.SQLitePath,
				AutoMigrate: false,
			}); err != nil {
				return fmt.Errorf("target db init: %w", err)
			}
			
			db := database.DB()
			if db == nil {
				return fmt.Errorf("database not initialized")
			}

			var subs []database.Subscription
			if err := db.Find(&subs).Error; err != nil {
				return fmt.Errorf("failed to load subscriptions: %w", err)
			}

			xrayCfg, err := xrayconfig.Read(cfg.Paths.XrayConfig)
			if err != nil {
				return fmt.Errorf("failed to read xray config: %w", err)
			}

			updatedCount := 0
			for _, sub := range subs {
				if sub.Email == "" || sub.XrayUUID == "" {
					continue
				}
				
				// Check if user exists in config
				exists, err := xrayconfig.UserExists(xrayCfg, sub.Email)
				if err != nil {
					fmt.Printf("[WARN] Failed to check user %s: %v\n", sub.Email, err)
					continue
				}
				
				if exists {
					// Update the 'id' field to match the DB's XrayUUID
					err = xrayconfig.UpdateStringField(xrayCfg, sub.Email, "id", sub.XrayUUID)
					if err != nil {
						fmt.Printf("[WARN] Failed to update user %s: %v\n", sub.Email, err)
					} else {
						updatedCount++
					}
				}
			}

			if updatedCount > 0 {
				fmt.Printf("[INFO] Updated %d clients in config.json. Saving...\n", updatedCount)
				if err := xrayconfig.Write(cfg.Paths.XrayConfig, xrayCfg); err != nil {
					return fmt.Errorf("failed to save xray config: %w", err)
				}
				fmt.Printf("[INFO] Restarting Xray service...\n")
				systemctlRestart("xray")
				fmt.Printf("[OK] Done!\n")
			} else {
				fmt.Printf("[INFO] No clients needed updating.\n")
			}

			return nil
		},
	}
	return cmd
}
