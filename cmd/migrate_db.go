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

			if err := migratePayments(srcGorm, database.DB()); err != nil {
				fmt.Printf("[WARN] Error migrating payments: %v\n", err)
			}

			if err := migrateDevices(srcGorm, database.DB(), devicesPath); err != nil {
				fmt.Printf("[WARN] Error migrating devices: %v\n", err)
			}

			// Cleanly close both databases to trigger SQLite WAL checkpoint!
			if sqlDB, err := srcGorm.DB(); err == nil {
				sqlDB.Close()
			}
			if sqlDB, err := database.DB().DB(); err == nil {
				sqlDB.Close()
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
	TgID           int64  `gorm:"column:tg_id"`
	ID             int64  `gorm:"column:id"`       // bot-internal auto-int
	Name           string `gorm:"column:name"`     // display name / real name
	Username       string `gorm:"column:username"` // Telegram @handle
	Status         string `gorm:"column:status"`
	CreatedAt      string `gorm:"column:created_at"` // stored as text in SQLite
	RefCode        string `gorm:"column:ref_code"`
	RefCodeUsed    string `gorm:"column:ref_code_used"`
	RefCodeUsedIDs string `gorm:"column:ref_code_used_ids"`
	IsAdmin        int    `gorm:"column:is_admin"` // 0 / 1
	Balance        int    `gorm:"column:balance"`
	MaxDevices     int    `gorm:"column:max_devices"`
	AutoRenew      int    `gorm:"column:auto_renew"`     // 0 / 1
	CashAvailable  int    `gorm:"column:cash_available"` // 0 / 1
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

// legacyPayment mirrors the old payments table.
type legacyPayment struct {
	ID            int64  `gorm:"column:id"`
	TgID          int64  `gorm:"column:tg_id"`
	Status        string `gorm:"column:status"`
	Amount        int    `gorm:"column:amount"`
	ProofFileID   string `gorm:"column:proof_file_id"`
	ProofFileType string `gorm:"column:proof_file_type"`
	CreatedAt     string `gorm:"column:created_at"`
	TargetLimit   int    `gorm:"column:target_limit"`
	FullAmount    int    `gorm:"column:full_amount"`
	ExternalID    string `gorm:"column:external_id"`
	Method        string `gorm:"column:method"`
	PaymentType   string `gorm:"column:payment_type"`
	CustomData    string `gorm:"column:custom_data"`
}

func (legacyPayment) TableName() string { return "payments" }

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

	var legacyServers []legacyServer
	if err := srcDB.Find(&legacyServers).Error; err != nil {
		return fmt.Errorf("reading legacy servers: %w", err)
	}
	serverByTgID := make(map[int64]legacyServer, len(legacyServers))
	for _, s := range legacyServers {
		serverByTgID[s.TgID] = s
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

			metadata := database.Metadata{"migrated_from": "telegram_bot"}
			
			// Parse legacy subfile ID from the server table Link if it exists
			if srv, ok := serverByTgID[lu.TgID]; ok && srv.Link != "" {
				parts := strings.Split(srv.Link, "id=")
				if len(parts) > 1 {
					key := strings.Split(parts[1], "&")[0]
					if key != "" {
						metadata["subfile"] = key
					}
				}
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
				Metadata:   metadata,
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
// Payments migration
// ─────────────────────────────────────────────────────────────────────────────

func migratePayments(srcDB *gorm.DB, dstDB *gorm.DB) error {
	var legacyPayments []legacyPayment
	if err := srcDB.Find(&legacyPayments).Error; err != nil {
		return fmt.Errorf("reading legacy payments: %w", err)
	}

	// Read all target users to map TgID -> UserID
	var allUsers []database.User
	if err := dstDB.Find(&allUsers).Error; err != nil {
		return fmt.Errorf("reading target users: %w", err)
	}
	tgIDToUserID := make(map[int64]string)
	for _, u := range allUsers {
		if tgIDVal, ok := u.Metadata["telegram_id"]; ok {
			if strVal, ok := tgIDVal.(string); ok {
				var id int64
				fmt.Sscanf(strVal, "%d", &id)
				tgIDToUserID[id] = u.ID
			}
		}
	}

	// Read all target payments to ensure idempotency
	var allPayments []database.Payment
	if err := dstDB.Find(&allPayments).Error; err != nil {
		return fmt.Errorf("reading target payments: %w", err)
	}
	migratedLegacyIDs := make(map[int64]bool)
	existingExternalIDs := make(map[string]bool)
	for _, p := range allPayments {
		if p.ExternalID != nil {
			existingExternalIDs[*p.ExternalID] = true
		}
		if legacyIDVal, ok := p.CustomData["legacy_id"]; ok {
			switch v := legacyIDVal.(type) {
			case float64:
				migratedLegacyIDs[int64(v)] = true
			case int64:
				migratedLegacyIDs[v] = true
			}
		}
	}

	migrated, skipped, failed := 0, 0, 0

	for _, lp := range legacyPayments {
		if migratedLegacyIDs[lp.ID] {
			skipped++
			continue
		}
		if lp.ExternalID != "" && existingExternalIDs[lp.ExternalID] {
			skipped++
			continue
		}

		userID, ok := tgIDToUserID[lp.TgID]
		if !ok {
			fmt.Printf("[SKIP] Payment ID=%d tg_id=%d: User not found\n", lp.ID, lp.TgID)
			skipped++
			continue
		}

		customData := database.Metadata{
			"legacy_id": lp.ID,
		}
		if lp.ProofFileID != "" {
			customData["proof_file_id"] = lp.ProofFileID
		}
		if lp.ProofFileType != "" {
			customData["proof_file_type"] = lp.ProofFileType
		}
		if lp.TargetLimit != 0 {
			customData["target_limit"] = lp.TargetLimit
		}
		if lp.FullAmount != 0 {
			customData["full_amount"] = lp.FullAmount
		}
		if lp.CustomData != "" {
			var cd map[string]interface{}
			if err := json.Unmarshal([]byte(lp.CustomData), &cd); err == nil {
				for k, v := range cd {
					customData[k] = v
				}
			} else {
				customData["legacy_custom_data"] = lp.CustomData
			}
		}

		var externalID *string
		if lp.ExternalID != "" {
			extID := lp.ExternalID
			externalID = &extID
		}

		status := lp.Status
		if status == "success" {
			status = "completed"
		}

		newPayment := database.Payment{
			UserID:      userID,
			Amount:      lp.Amount,
			Status:      status,
			PaymentType: lp.PaymentType,
			Method:      lp.Method,
			ExternalID:  externalID,
			CustomData:  customData,
			CreatedAt:   parseFlexibleTime(lp.CreatedAt),
		}

		if err := dstDB.Create(&newPayment).Error; err != nil {
			fmt.Printf("[FAIL] Payment ID=%d insert: %v\n", lp.ID, err)
			failed++
			continue
		}
		migrated++
	}

	fmt.Printf("\n=== Payments Migration complete: %d migrated, %d skipped, %d failed ===\n", migrated, skipped, failed)
	return nil
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
			UserAgent    string `json:"last_user_agent"`
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
		
		var sub database.Subscription
		found := false

		if ok {
			email := fmt.Sprintf("bot_client_%d", tgID)
			if err := dstDB.Where("email = ?", email).First(&sub).Error; err == nil {
				found = true
			}
		}

		if !found {
			// Fallback: search by subfile directly in metadata
			if err := dstDB.Where("json_extract(metadata, '$.subfile') = ? OR json_extract(metadata, '$.subfile') = ?", clientKey, normalizedKey).First(&sub).Error; err == nil {
				found = true
			}
		}

		if !found {
			fmt.Printf("[SKIP] Sub not found for clientKey=%q\n", clientKey)
			continue
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
