package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"xraytool/internal/database"
	"xraytool/internal/legacy"
)

// migrateLegacyDBCmd returns the "db-migrate" cobra sub-command.
// It reads the old Telegram-bot SQLite database (bot.db) and inserts
// Users + Subscriptions into the configured target database (Postgres or SQLite).
func migrateLegacyDBCmd(deps *AppDeps) *cobra.Command {
	var sourcePath string

	cmd := &cobra.Command{
		Use:   "db-migrate",
		Short: "Migrate legacy bot.db (SQLite) data into the configured database",
		Long: `Reads the old Telegram-bot SQLite database and migrates all users and
subscriptions into the target database configured in config.yaml.

This is a one-time migration tool. Running it a second time is safe because
it skips rows that already exist (identified by Telegram ID in Metadata).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// ── 1. Validate inputs ─────────────────────────────────────────────
			if deps.Cfg == nil {
				return fmt.Errorf("config not loaded; pass --config <path>")
			}

			dbCfg := database.Config{
				Driver:      deps.Cfg.Database.Driver,
				DSN:         deps.Cfg.Database.DSN,
				SQLitePath:  deps.Cfg.Database.SQLitePath,
				AutoMigrate: false,
				Silent:      false,
			}

			if err := legacy.Migrate(sourcePath, dbCfg); err != nil {
				return err
			}

			return nil
		},
	}

	cmd.Flags().StringVar(
		&sourcePath, "from", "",
		"Path to the legacy bot.db SQLite file (required)",
	)
	_ = cmd.MarkFlagRequired("from")

	return cmd
}
