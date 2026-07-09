package legacy

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"xraytool/internal/database"
)

func Migrate(sourcePath string, targetCfg database.Config) error {
	srcGorm, err := gorm.Open(sqlite.Open(sourcePath), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Warn),
	})
	if err != nil {
		return fmt.Errorf("open source db %q: %w", sourcePath, err)
	}

	targetDB, err := database.NewConnection(targetCfg)
	if err != nil {
		return fmt.Errorf("open target db: %w", err)
	}

	if err := migrateData(srcGorm, targetDB); err != nil {
		return err
	}

	if err := migratePayments(srcGorm, targetDB); err != nil {
		fmt.Printf("[WARN] Error migrating payments: %v\n", err)
	}

	if sqlDB, err := srcGorm.DB(); err == nil {
		sqlDB.Close()
	}
	if sqlDB, err := targetDB.DB(); err == nil {
		sqlDB.Close()
	}
	return nil
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

		createdAt, err := parseFlexibleTime(lu.CreatedAt)
		if err != nil {
			fmt.Printf("[FAIL] tg_id=%d failed to parse user CreatedAt %q: %v\n", lu.TgID, lu.CreatedAt, err)
			failed++
			continue
		}

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

			startsAt, err1 := parseFlexibleTimePtr(ls.StartsAt)
			endsAt, err2 := parseFlexibleTimePtr(ls.EndsAt)
			if err1 != nil || err2 != nil {
				fmt.Printf("[FAIL] tg_id=%d failed to parse sub dates (starts: %v, ends: %v)\n", lu.TgID, err1, err2)
				failed++
				continue
			}

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
func parseFlexibleTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, fmt.Errorf("empty time string")
	}
	for _, fmtStr := range timeFormats {
		if t, err := time.Parse(fmtStr, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized time format: %q", s)
}

// parseFlexibleTimePtr is like parseFlexibleTime but returns nil for empty strings,
// suitable for nullable Subscription.StartsAt / EndsAt fields.
func parseFlexibleTimePtr(s string) (*time.Time, error) {
	if s == "" || s == "None" || s == "null" {
		return nil, nil
	}
	t, err := parseFlexibleTime(s)
	if err != nil {
		return nil, err
	}
	return &t, nil
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
				fmt.Sscanf(strVal, "%d", &id) //nolint:errcheck
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

		createdAt, err := parseFlexibleTime(lp.CreatedAt)
		if err != nil {
			fmt.Printf("[FAIL] Payment ID=%d failed to parse CreatedAt %q: %v\n", lp.ID, lp.CreatedAt, err)
			failed++
			continue
		}

		newPayment := database.Payment{
			UserID:      userID,
			Amount:      lp.Amount,
			Status:      status,
			PaymentType: lp.PaymentType,
			Method:      lp.Method,
			ExternalID:  externalID,
			CustomData:  customData,
			CreatedAt:   createdAt,
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
