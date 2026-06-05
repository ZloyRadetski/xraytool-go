package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"xraytool/internal/database"
)

// migrateLegacyDBCmd returns the "db-migrate" cobra sub-command.
// It reads the old Telegram-bot SQLite database (bot.db) and inserts
// Users + Subscriptions into the configured target database (Postgres or SQLite).
func migrateLegacyDBCmd() *cobra.Command {
	var sourcePath string
	var devicesPath string

	cmd := &cobra.Command{
		Use:   "db-migrate",
		Short: "Migrate legacy bot.db (SQLite) data into the configured database",
		Long: `Reads the old Telegram-bot SQLite database and migrates all users and
subscriptions into the target database configured in config.yaml.

This is a one-time migration tool. Running it a second time is safe because
it skips rows that already exist (identified by Telegram ID in Metadata).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// ── 1. Validate inputs ─────────────────────────────────────────────
			if cfg == nil {
				return fmt.Errorf("config not loaded; pass --config <path>")
			}

			// ── 2. Initialise target database ──────────────────────────────────
			if err := database.Init(database.Config{
				Driver:     cfg.Database.Driver,
				DSN:        cfg.Database.DSN,
				SQLitePath: cfg.Database.SQLitePath,
			}); err != nil {
				return fmt.Errorf("target db init: %w", err)
			}

			// ── 3. Open source (legacy) SQLite database ────────────────────────
			// glebarez/sqlite registers the CGo-free "sqlite" driver automatically
			// via the side-effect import in driver_sqlite.go inside the database package.
			// Here we open the source file directly with GORM for a typed read.
			srcGorm, err := gorm.Open(sqlite.Open(sourcePath), &gorm.Config{
				Logger: logger.Default.LogMode(logger.Warn),
			})
			if err != nil {
				return fmt.Errorf("open source db %q: %w", sourcePath, err)
			}

			if err := migrateData(srcGorm, database.DB()); err != nil {
				return err
			}
			
			if err := migrateDevices(srcGorm, database.DB(), devicesPath); err != nil {
				fmt.Printf("[WARN] Error migrating devices: %v\n", err)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(
		&sourcePath, "from", "",
		"Path to the legacy bot.db SQLite file (required)",
	)
	cmd.Flags().StringVar(
		&devicesPath, "devices", "",
		"Path to the legacy devices_state.json file (optional)",
	)
	_ = cmd.MarkFlagRequired("from")

	return cmd
}

// ─────────────────────────────────────────────────────────────────────────────
// Legacy source schema (read-only structs — no GORM tags that modify target DB)
// ─────────────────────────────────────────────────────────────────────────────

// legacyUser mirrors the old Telegram-bot users table.
type legacyUser struct {
	TgID            int64   `gorm:"column:tg_id"`
	ID              int64   `gorm:"column:id"`         // bot-internal auto-int
	Name            string  `gorm:"column:name"`       // display name / real name
	Username        string  `gorm:"column:username"`   // Telegram @handle
	Status          string  `gorm:"column:status"`
	CreatedAt       string  `gorm:"column:created_at"` // stored as text in SQLite
	RefCode         string  `gorm:"column:ref_code"`
	RefCodeUsed     string  `gorm:"column:ref_code_used"`
	RefCodeUsedIDs  string  `gorm:"column:ref_code_used_ids"`
	IsAdmin         int     `gorm:"column:is_admin"`   // 0 / 1
	Balance         int     `gorm:"column:balance"`
	MaxDevices      int     `gorm:"column:max_devices"`
	AutoRenew       int     `gorm:"column:auto_renew"` // 0 / 1
	CashAvailable   int     `gorm:"column:cash_available"` // 0 / 1
}

// TableName makes GORM read from "users" in the legacy DB.
func (legacyUser) TableName() string { return "users" }

// legacySubscription mirrors the old Telegram-bot subscriptions table.
type legacySubscription struct {
	TgID      int64  `gorm:"column:tg_id"`
	Status    string `gorm:"column:status"`
	StartsAt  string `gorm:"column:starts_at"`
	EndsAt    string `gorm:"column:ends_at"`
	UpdatedAt string `gorm:"column:updated_at"`
}

// TableName makes GORM read from "subscriptions" in the legacy DB.
func (legacySubscription) TableName() string { return "subscriptions" }

// legacyServer mirrors the old server table containing subscription links.
type legacyServer struct {
	TgID int64  `gorm:"column:tg_id"`
	Name string `gorm:"column:name"`
	Link string `gorm:"column:link"`
}

func (legacyServer) TableName() string { return "server" }

// ─────────────────────────────────────────────────────────────────────────────
// migrateData is the core migration logic.
// ─────────────────────────────────────────────────────────────────────────────

// migrateData reads all users from srcDB and upserts them (plus their
// subscriptions) into dstDB.  It is idempotent: rows already migrated
// (detected by telegram_id in Metadata) are skipped.
func migrateData(srcDB *gorm.DB, dstDB *gorm.DB) error {
	// Fetch all legacy users.
	var legacyUsers []legacyUser
	if err := srcDB.Find(&legacyUsers).Error; err != nil {
		return fmt.Errorf("reading legacy users: %w", err)
	}

	// Build a map of tg_id → subscription for O(1) lookup.
	var legacySubs []legacySubscription
	if err := srcDB.Find(&legacySubs).Error; err != nil {
		return fmt.Errorf("reading legacy subscriptions: %w", err)
	}
	subByTgID := make(map[int64]legacySubscription, len(legacySubs))
	for _, s := range legacySubs {
		subByTgID[s.TgID] = s
	}

	migrated, skipped, failed := 0, 0, 0

	for _, lu := range legacyUsers {
		// ── Check if already migrated ───────────────────────────────────────
		// We store telegram_id in Metadata so a JSON query is the canonical check.
		// Use a raw count to avoid pulling the full row.
		tgIDStr := fmt.Sprintf("%d", lu.TgID)
		var count int64
		dstDB.Model(&database.User{}).
			Where("metadata LIKE ? OR metadata LIKE ?", `%"telegram_id":"`+tgIDStr+`"%`, `%"telegram_id":`+tgIDStr+`%`).
			Count(&count)
		if count > 0 {
			fmt.Printf("[SKIP] tg_id=%d already migrated\n", lu.TgID)
			skipped++
			continue
		}

		// ── Build new User ──────────────────────────────────────────────────
		newUserID := uuid.New().String()

		createdAt := parseFlexibleTime(lu.CreatedAt)

		metadata := database.Metadata{
			"telegram_id":       tgIDStr,
			"telegram_username": lu.Username,
			"source":            "telegram_bot",
		}
		if lu.CashAvailable != 0 {
			metadata["cash_available"] = true
		}
		if lu.RefCodeUsed != "" {
			metadata["ref_code_used"] = lu.RefCodeUsed
		}

		newUser := database.User{
			ID:        newUserID,
			Username:  lu.Name,
			Balance:   lu.Balance,
			IsAdmin:   lu.IsAdmin != 0,
			RefCode:   lu.RefCode,
			Metadata:  metadata,
			CreatedAt: createdAt,
		}

		// ── Insert User ─────────────────────────────────────────────────────
		if err := dstDB.Create(&newUser).Error; err != nil {
			fmt.Printf("[FAIL] tg_id=%d user insert: %v\n", lu.TgID, err)
			failed++
			continue
		}

		// ── Build + insert Subscription (if exists) ─────────────────────────
		if ls, ok := subByTgID[lu.TgID]; ok {
			newSubID := uuid.New().String()
			newXrayUUID := uuid.New().String() // no old UUID stored in bot DB

			startsAt := parseFlexibleTimePtr(ls.StartsAt)
			endsAt := parseFlexibleTimePtr(ls.EndsAt)

			maxDevices := lu.MaxDevices
			if maxDevices <= 0 {
				maxDevices = 3 // sane default
			}

			newSub := database.Subscription{
				ID:         newSubID,
				UserID:     newUserID,
				Email:      fmt.Sprintf("bot_client_%d", lu.TgID),
				XrayUUID:   newXrayUUID,
				Status:     ls.Status,
				MaxDevices: maxDevices,
				AutoRenew:  lu.AutoRenew != 0,
				StartsAt:   startsAt,
				EndsAt:     endsAt,
				Metadata:   database.Metadata{"migrated_from": "telegram_bot"},
			}

			if err := dstDB.Create(&newSub).Error; err != nil {
				fmt.Printf("[WARN] tg_id=%d sub insert: %v\n", lu.TgID, err)
				// User was created successfully; don't count this as a hard failure.
			}
		}

		fmt.Printf("[OK]   tg_id=%d name=%q migrated → user_id=%s\n", lu.TgID, lu.Name, newUserID)
		migrated++
	}

	fmt.Printf("\n=== Migration complete: %d migrated, %d skipped, %d failed ===\n",
		migrated, skipped, failed)

	if failed > 0 {
		return fmt.Errorf("%d row(s) failed to migrate", failed)
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Time-parsing helpers
// ─────────────────────────────────────────────────────────────────────────────

// timeFormats is an ordered list of formats the bot may have used to store timestamps.
var timeFormats = []string{
	"2006-01-02 15:04:05.999999",
	"2006-01-02 15:04:05",
	"2006-01-02T15:04:05.999999",
	"2006-01-02T15:04:05Z",
	"2006-01-02",
}

// parseFlexibleTime attempts to parse s with several common SQLite formats.
// Returns time.Now() if none match (keeps the row insertable).
func parseFlexibleTime(s string) time.Time {
	if s == "" {
		return time.Now()
	}
	for _, fmt := range timeFormats {
		if t, err := time.Parse(fmt, s); err == nil {
			return t
		}
	}
	return time.Now()
}

// parseFlexibleTimePtr is like parseFlexibleTime but returns nil for empty strings,
// suitable for nullable Subscription.StartsAt / EndsAt fields.
func parseFlexibleTimePtr(s string) *time.Time {
	if s == "" || s == "None" || s == "null" {
		return nil
	}
	t := parseFlexibleTime(s)
	return &t
}

// ─────────────────────────────────────────────────────────────────────────────
// Devices migration
// ─────────────────────────────────────────────────────────────────────────────

type legacyDeviceState struct {
	Clients map[string]struct {
		Devices []struct {
			HWID         string `json:"hwid"`
			DeviceModel  string `json:"device_model"`
			DeviceOS     string `json:"device_os"`
			VerOS        string `json:"ver_os"`
			UserAgent    string `json:"user_agent"`
			RequestCount int    `json:"request_count"`
			FirstSeen    string `json:"first_seen"`
			LastSeen     string `json:"last_seen"`
		} `json:"devices"`
	} `json:"clients"`
}

func migrateDevices(srcDB *gorm.DB, dstDB *gorm.DB, devicesPath string) error {
	if devicesPath == "" {
		return nil
	}
	data, err := os.ReadFile(devicesPath)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("[WARN] devices state file not found, skipping devices migration")
			return nil
		}
		return err
	}
	var state legacyDeviceState
	importJson := true
	_ = importJson // JSON imported in standard imports

	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("parsing devices state: %w", err)
	}

	// Build clientKey -> tg_id map from server table
	var servers []legacyServer
	if err := srcDB.Find(&servers).Error; err != nil {
		return fmt.Errorf("reading legacy server table: %w", err)
	}
	
	keyToTgID := make(map[string]int64)
	for _, s := range servers {
		if s.Link == "" {
			continue
		}
		
		parts := strings.Split(s.Link, "id=")
		if len(parts) > 1 {
			key := strings.Split(parts[1], "&")[0]
			key = strings.ToLower(key)
			keyToTgID[key] = s.TgID
			keyToTgID[key+".txt"] = s.TgID
		}
	}

	migrated := 0
	for clientKey, clientData := range state.Clients {
		normalizedKey := strings.ToLower(clientKey)
		tgID, ok := keyToTgID[normalizedKey]
		if !ok {
			fmt.Printf("[SKIP] Could not find tg_id for clientKey=%q\n", clientKey)
			continue
		}

		email := fmt.Sprintf("bot_client_%d", tgID)

		var sub database.Subscription
		if err := dstDB.Where("email = ?", email).First(&sub).Error; err != nil {
			fmt.Printf("[SKIP] Sub not found for email=%q\n", email)
			continue // Subscription not found
		}

		for _, d := range clientData.Devices {
			var count int64
			dstDB.Model(&database.Device{}).Where("subscription_id = ? AND hw_id = ?", sub.ID, d.HWID).Count(&count)
			if count > 0 {
				continue
			}

			dstDB.Create(&database.Device{
				SubscriptionID: sub.ID,
				HWID:           d.HWID,
				DeviceModel:    d.DeviceModel,
				DeviceOS:       d.DeviceOS,
				VerOS:          d.VerOS,
				UserAgent:      d.UserAgent,
				RequestCount:   d.RequestCount,
				FirstSeen:      parseFlexibleTime(d.FirstSeen),
				LastSeen:       parseFlexibleTime(d.LastSeen),
			})
			migrated++
		}
	}
	fmt.Printf("\n=== Devices Migration complete: %d devices migrated ===\n", migrated)
	return nil
}
