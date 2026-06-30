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

			updatedCount := 0
			if err := xrayconfig.Modify(cfg.Paths.XrayConfig, func(c xrayconfig.RawConfig) error {
				for _, sub := range subs {
					if sub.Email == "" || sub.XrayUUID == "" {
						continue
					}
					
					exists, err := xrayconfig.UserExists(c, sub.Email)
					if err != nil {
						fmt.Printf("[WARN] Failed to check user %s: %v\n", sub.Email, err)
						continue
					}
					
					if exists {
						err = xrayconfig.UpdateStringField(c, sub.Email, "id", sub.XrayUUID)
						if err != nil {
							fmt.Printf("[WARN] Failed to update user %s: %v\n", sub.Email, err)
						} else {
							updatedCount++
						}
					}
				}
				return nil
			}); err != nil {
				return fmt.Errorf("failed to update xray config: %w", err)
			}

			if updatedCount > 0 {
				fmt.Printf("[INFO] Updated %d clients in config.json.\n", updatedCount)
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
