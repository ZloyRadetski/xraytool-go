package server

// handlers_user.go — implements all user-related REST handlers.
//
// Response shapes are designed to exactly match what the Python Telegram bot
// (clients-bot/sqltools.py) expects. Any deviation will break the bot.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"golang.org/x/time/rate"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"xraytool/internal/database"
	"xraytool/internal/generate"
	"xraytool/internal/templates"
	"xraytool/internal/userdb"
	"xraytool/internal/xrayapi"
	"xraytool/internal/xrayconfig"
)

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

// findUserByTelegramID locates a User whose Metadata contains
// "telegram_id":<tgID>. Works for both SQLite (TEXT JSON) and Postgres (JSONB
// serialised as text by GORM).
func findUserByTelegramID(db *gorm.DB, tgID int64) (*database.User, error) {
	var user database.User
	tgIDStr := strconv.FormatInt(tgID, 10)

	var query *gorm.DB
	switch db.Dialector.Name() {
	case "sqlite":
		query = db.Where(
			"json_extract(metadata, '$.telegram_id') = ? OR json_extract(metadata, '$.telegram_id') = ?",
			tgID,
			tgIDStr,
		)
	case "postgres":
		query = db.Where(
			"metadata::jsonb ->> 'telegram_id' = ?",
			tgIDStr,
		)
	default:
		// Fallback safe query using SQLite/json_extract syntax
		query = db.Where(
			"json_extract(metadata, '$.telegram_id') = ? OR json_extract(metadata, '$.telegram_id') = ?",
			tgID,
			tgIDStr,
		)
	}

	result := query.First(&user)
	if result.Error != nil {
		return nil, result.Error
	}
	return &user, nil
}

// buildUserResponse assembles the full user JSON object the Python bot expects.
// It enriches a User with subscription, referral counts, and link.
func (r *Router) buildUserResponse(db *gorm.DB, user *database.User) map[string]interface{} {
	// Load the subscription for this user (there should be one; tolerate absence).
	var sub database.Subscription
	db.Where("user_id = ?", user.ID).First(&sub)

	// Count referral rows where this user is the referrer.
	var referralCount int64
	db.Model(&database.ReferralReward{}).Where("referrer_id = ?", user.ID).Count(&referralCount)

	// Sum referral rewards earned by this user.
	var referralEarned int64
	db.Model(&database.ReferralReward{}).
		Where("referrer_id = ?", user.ID).
		Select("COALESCE(SUM(amount),0)").
		Scan(&referralEarned)

	// Extract telegram_id from metadata (stored as float64 by JSON unmarshal, or string if migrated).
	var tgID int64
	if user.Metadata != nil {
		switch v := user.Metadata["telegram_id"].(type) {
		case float64:
			tgID = int64(v)
		case int64:
			tgID = v
		case int:
			tgID = int64(v)
		case string:
			parsed, _ := strconv.ParseInt(v, 10, 64)
			tgID = parsed
		}
	}

	// Build the subscription link.
	link := ""
	if r.cfg != nil && r.cfg.Server.Domain != "" && sub.XrayUUID != "" {
		link = fmt.Sprintf("https://%s/client?id=%s", r.cfg.Server.Domain, sub.XrayUUID)
	}

	// email field the bot uses: "bot_client_<tgID>"
	email := sub.Email
	if email == "" && tgID != 0 {
		email = fmt.Sprintf("bot_client_%d", tgID)
	}

	// Format nullable times as strings (empty string if nil).
	fmtTime := func(t *time.Time) interface{} {
		if t == nil || t.IsZero() {
			return nil
		}
		return t.UTC().Format(time.RFC3339)
	}

	resp := map[string]interface{}{
		"id":                     user.ID,
		"username":               user.Username,
		"balance":                user.Balance,
		"is_admin":               user.IsAdmin,
		"max_devices":            sub.MaxDevices,
		"ref_code":               user.RefCode,
		"referred_by":            user.ReferredBy,
		"sub_status":             sub.Status,
		"ends_at":                fmtTime(sub.EndsAt),
		"starts_at":              fmtTime(sub.StartsAt),
		"auto_renew":             sub.AutoRenew,
		"referral_count":         referralCount,
		"referral_earned_amount": referralEarned,
		"email":                  email,
		"link":                   link,
		"metadata":               user.Metadata,
		"created_at":             user.CreatedAt.UTC().Format(time.RFC3339),
	}
	return resp
}

// generateRefCode creates a unique referral code. It retries on collision.
func generateRefCode(db *gorm.DB) string {
	for {
		code := "ref_" + generate.Secret(8)
		var count int64
		db.Model(&database.User{}).Where("ref_code = ?", code).Count(&count)
		if count == 0 {
			return code
		}
	}
}

// readBody reads and JSON-decodes the request body into dst. Returns false and
// writes a 400 if parsing fails.
func readBody(w http.ResponseWriter, req *http.Request, dst interface{}) bool {
	req.Body = http.MaxBytesReader(w, req.Body, 1<<20) // 1 MB
	data, err := io.ReadAll(req.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return false
	}
	if err := json.Unmarshal(data, dst); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return false
	}
	return true
}

// ─────────────────────────────────────────────────────────────────────────────
// POST /api/v1/users/register
// Body: {"telegram_id":123,"username":"name","telegram_username":"@user"}
// ─────────────────────────────────────────────────────────────────────────────

var registerLimiter = rate.NewLimiter(rate.Every(time.Millisecond), 100)

func (r *Router) handleRegisterUser(w http.ResponseWriter, req *http.Request) {
	if !registerLimiter.Allow() {
		writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
		return
	}
	var body struct {
		TelegramID       int64  `json:"telegram_id"`
		Username         string `json:"username"`
		TelegramUsername string `json:"telegram_username"`
	}
	if !readBody(w, req, &body) {
		return
	}
	if body.TelegramID == 0 {
		writeError(w, http.StatusBadRequest, "telegram_id is required")
		return
	}

	db := database.DB()

	// Idempotency: if user already exists return it.
	existing, err := findUserByTelegramID(db, body.TelegramID)
	if err == nil && existing != nil {
		writeJSON(w, http.StatusOK, r.buildUserResponse(db, existing))
		return
	}

	// Create new user.
	userID := uuid.New().String()
	refCode := generateRefCode(db)

	user := database.User{
		ID:       userID,
		Username: body.Username,
		Balance:  0,
		IsAdmin:  false,
		RefCode:  refCode,
		Metadata: database.Metadata{
			"telegram_id":       body.TelegramID,
			"telegram_username": body.TelegramUsername,
			"source":            "telegram_bot",
		},
	}

	if result := db.Create(&user); result.Error != nil {
		r.log.Error("register user: db create user", "err", result.Error)
		writeError(w, http.StatusInternalServerError, "failed to create user")
		return
	}

	// Create companion subscription record.
	subID := uuid.New().String()
	xrayUUID := uuid.New().String()
	email := fmt.Sprintf("bot_client_%d", body.TelegramID)

	sub := database.Subscription{
		ID:         subID,
		UserID:     userID,
		Email:      email,
		XrayUUID:   xrayUUID,
		Status:     "inactive",
		MaxDevices: 3,
		AutoRenew:  false,
	}

	if result := db.Create(&sub); result.Error != nil {
		r.log.Error("register user: db create subscription", "err", result.Error)
		// User created but no sub — not fatal, return user anyway.
	}

	writeJSON(w, http.StatusCreated, r.buildUserResponse(db, &user))
}

// ─────────────────────────────────────────────────────────────────────────────
// GET /api/v1/users
// Returns array of full user objects.
// ─────────────────────────────────────────────────────────────────────────────

func (r *Router) handleListUsers(w http.ResponseWriter, req *http.Request) {
	db := database.DB()
	var users []database.User
	if result := db.Find(&users); result.Error != nil {
		r.log.Error("list users", "err", result.Error)
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}

	out := make([]map[string]interface{}, 0, len(users))
	for i := range users {
		out = append(out, r.buildUserResponse(db, &users[i]))
	}
	writeJSON(w, http.StatusOK, out)
}

// ─────────────────────────────────────────────────────────────────────────────
// GET /api/v1/users/admins
// Returns [123456, 789012] — array of telegram IDs for admin users.
// ─────────────────────────────────────────────────────────────────────────────

func (r *Router) handleListAdmins(w http.ResponseWriter, req *http.Request) {
	db := database.DB()
	var users []database.User
	if result := db.Where("is_admin = ?", true).Find(&users); result.Error != nil {
		r.log.Error("list admins", "err", result.Error)
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}

	ids := make([]int64, 0, len(users))
	for _, u := range users {
		if u.Metadata == nil {
			continue
		}
		switch v := u.Metadata["telegram_id"].(type) {
		case float64:
			ids = append(ids, int64(v))
		case int64:
			ids = append(ids, v)
		case int:
			ids = append(ids, int64(v))
		}
	}
	writeJSON(w, http.StatusOK, ids)
}

// ─────────────────────────────────────────────────────────────────────────────
// GET /api/v1/users/telegram/{id}
// ─────────────────────────────────────────────────────────────────────────────

func (r *Router) handleGetUserByTelegram(w http.ResponseWriter, req *http.Request) {
	idStr := req.PathValue("id")
	tgID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || tgID == 0 {
		writeError(w, http.StatusBadRequest, "invalid telegram id")
		return
	}

	db := database.DB()
	user, err := findUserByTelegramID(db, tgID)
	if err != nil {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}
	writeJSON(w, http.StatusOK, r.buildUserResponse(db, user))
}

// ─────────────────────────────────────────────────────────────────────────────
// GET /api/v1/users/ref/{code}
// Returns user object or 404.
// ─────────────────────────────────────────────────────────────────────────────

func (r *Router) handleGetUserByRef(w http.ResponseWriter, req *http.Request) {
	code := req.PathValue("code")
	if code == "" {
		writeError(w, http.StatusBadRequest, "ref code is required")
		return
	}

	db := database.DB()
	var user database.User
	if result := db.Where("ref_code = ?", code).First(&user); result.Error != nil {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}
	writeJSON(w, http.StatusOK, r.buildUserResponse(db, &user))
}

// ─────────────────────────────────────────────────────────────────────────────
// POST /api/v1/users/telegram/{id}/balance
// Body: {"amount":100}  →  {"balance":200}
// ─────────────────────────────────────────────────────────────────────────────

func (r *Router) handleAdjustBalance(w http.ResponseWriter, req *http.Request) {
	idStr := req.PathValue("id")
	tgID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || tgID == 0 {
		writeError(w, http.StatusBadRequest, "invalid telegram id")
		return
	}

	var body struct {
		Amount int `json:"amount"`
	}
	if !readBody(w, req, &body) {
		return
	}
	if body.Amount < 0 || body.Amount > 100000000 {
		writeError(w, http.StatusBadRequest, "amount must be between 0 and 100_000_000")
		return
	}

	db := database.DB()
	user, err := findUserByTelegramID(db, tgID)
	if err != nil {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}

	// Atomic update using raw query
	query := "UPDATE users SET balance = CASE WHEN balance + ? < 0 THEN 0 ELSE balance + ? END WHERE id = ?"
	if result := db.Exec(query, body.Amount, body.Amount, user.ID); result.Error != nil {
		r.log.Error("adjust balance", "err", result.Error)
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}

	var updatedBalance int
	db.Model(user).Select("balance").Scan(&updatedBalance)
	writeJSON(w, http.StatusOK, map[string]interface{}{"balance": updatedBalance})
}

// ─────────────────────────────────────────────────────────────────────────────
// POST /api/v1/users/telegram/{id}/max-devices
// Body: {"max_devices":5}  →  {"ok":true}
// ─────────────────────────────────────────────────────────────────────────────

func (r *Router) handleSetMaxDevices(w http.ResponseWriter, req *http.Request) {
	idStr := req.PathValue("id")
	tgID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || tgID == 0 {
		writeError(w, http.StatusBadRequest, "invalid telegram id")
		return
	}

	var body struct {
		MaxDevices int `json:"max_devices"`
	}
	if !readBody(w, req, &body) {
		return
	}
	if body.MaxDevices <= 0 {
		writeError(w, http.StatusBadRequest, "max_devices must be positive")
		return
	}

	db := database.DB()
	user, err := findUserByTelegramID(db, tgID)
	if err != nil {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}

	if result := db.Model(&database.Subscription{}).
		Where("user_id = ?", user.ID).
		Update("max_devices", body.MaxDevices); result.Error != nil {
		r.log.Error("set max devices", "err", result.Error)
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}

	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ─────────────────────────────────────────────────────────────────────────────
// POST /api/v1/users/telegram/{id}/auto-renew-toggle
// Body: {"auto_renew":true}  →  {"ok":true}
// ─────────────────────────────────────────────────────────────────────────────

func (r *Router) handleAutoRenewToggle(w http.ResponseWriter, req *http.Request) {
	idStr := req.PathValue("id")
	tgID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || tgID == 0 {
		writeError(w, http.StatusBadRequest, "invalid telegram id")
		return
	}

	var body struct {
		AutoRenew bool `json:"auto_renew"`
	}
	if !readBody(w, req, &body) {
		return
	}

	db := database.DB()
	user, err := findUserByTelegramID(db, tgID)
	if err != nil {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}

	if result := db.Model(&database.Subscription{}).
		Where("user_id = ?", user.ID).
		Update("auto_renew", body.AutoRenew); result.Error != nil {
		r.log.Error("auto-renew toggle", "err", result.Error)
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}

	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ─────────────────────────────────────────────────────────────────────────────
// POST /api/v1/users/telegram/{id}/auto-renew
// Body: {"plan_total_price":159,"new_ends_at":"2026-07-04T..."}
// Atomically deducts balance and extends subscription. →  {"ok":true}
// ─────────────────────────────────────────────────────────────────────────────

func (r *Router) handleAutoRenew(w http.ResponseWriter, req *http.Request) {
	idStr := req.PathValue("id")
	tgID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || tgID == 0 {
		writeError(w, http.StatusBadRequest, "invalid telegram id")
		return
	}

	var body struct {
		PlanTotalPrice int    `json:"plan_total_price"`
		NewEndsAt      string `json:"new_ends_at"`
	}
	if !readBody(w, req, &body) {
		return
	}
	if body.PlanTotalPrice < 0 || body.PlanTotalPrice > 10000000 {
		writeError(w, http.StatusBadRequest, "invalid plan total price")
		return
	}
	if body.NewEndsAt == "" {
		writeError(w, http.StatusBadRequest, "new_ends_at is required")
		return
	}
	if body.PlanTotalPrice <= 0 {
		writeError(w, http.StatusBadRequest, "plan_total_price must be positive")
		return
	}

	newEndsAt, err := time.Parse(time.RFC3339, body.NewEndsAt)
	if err != nil {
		// Try other common formats.
		newEndsAt, err = time.Parse("2006-01-02T15:04:05Z", body.NewEndsAt)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid new_ends_at format, expected RFC3339")
			return
		}
	}

	db := database.DB()
	user, err := findUserByTelegramID(db, tgID)
	if err != nil {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}

	// Atomic: deduct balance and update subscription in a transaction.
	txErr := db.Transaction(func(tx *gorm.DB) error {
		// Only deduct if price > 0.
		if body.PlanTotalPrice > 0 {
			result := tx.Model(&database.User{}).
				Where("id = ? AND balance >= ?", user.ID, body.PlanTotalPrice).
				Update("balance", gorm.Expr("balance - ?", body.PlanTotalPrice))
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected == 0 {
				return fmt.Errorf("insufficient balance")
			}
		}

		now := time.Now()
		return tx.Model(&database.Subscription{}).
			Where("user_id = ?", user.ID).
			Updates(map[string]interface{}{
				"status":     "active",
				"ends_at":    newEndsAt,
				"starts_at":  now,
				"updated_at": now,
			}).Error
	})

	if txErr != nil {
		if txErr.Error() == "insufficient balance" {
			writeError(w, http.StatusPaymentRequired, "insufficient balance")
			return
		}
		r.log.Error("auto-renew transaction", "err", txErr)
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}

	// Fetch the updated subscription and ensure user is unbanned in Xray config
	var updatedSub database.Subscription
	if err := db.Where("user_id = ?", user.ID).First(&updatedSub).Error; err == nil {
		r.unbanUserInXray(updatedSub)
	} else {
		r.log.Error("failed to find subscription after auto-renew for unban", "user_id", user.ID, "err", err)
	}

	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ─────────────────────────────────────────────────────────────────────────────
// POST /api/v1/users/telegram/{id}/metadata
// Body: {"key":"cash_available","value":true}  →  {"ok":true}
// ─────────────────────────────────────────────────────────────────────────────

func (r *Router) handleSetMetadata(w http.ResponseWriter, req *http.Request) {
	idStr := req.PathValue("id")
	tgID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || tgID == 0 {
		writeError(w, http.StatusBadRequest, "invalid telegram id")
		return
	}

	var body struct {
		Key   string      `json:"key"`
		Value interface{} `json:"value"`
	}
	if !readBody(w, req, &body) {
		return
	}
	if body.Key == "" {
		writeError(w, http.StatusBadRequest, "key is required")
		return
	}

	db := database.DB()
	user, err := findUserByTelegramID(db, tgID)
	if err != nil {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}

	// Merge into existing metadata map.
	if user.Metadata == nil {
		user.Metadata = database.Metadata{}
	}
	user.Metadata[body.Key] = body.Value

	if result := db.Model(user).Update("metadata", user.Metadata); result.Error != nil {
		r.log.Error("set metadata", "err", result.Error)
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}

	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ─────────────────────────────────────────────────────────────────────────────
// Admin handlers
// ─────────────────────────────────────────────────────────────────────────────

// POST /api/v1/admin/users/{email}/block  →  {"ok":true}
func (r *Router) handleAdminBlockUser(w http.ResponseWriter, req *http.Request) {
	email := req.PathValue("email")
	if email == "" {
		writeError(w, http.StatusBadRequest, "email path parameter is required")
		return
	}

	db := database.DB()

	// Find subscription by email.
	var sub database.Subscription
	if result := db.Where("email = ?", email).First(&sub); result.Error != nil {
		writeError(w, http.StatusNotFound, "subscription not found")
		return
	}

	// 1. Remove from Xray config and API to make the ban instant
	var tags []string
	modErr := xrayconfig.Modify(r.cfg.Paths.XrayConfig, func(cfg xrayconfig.RawConfig) error {
		t, _ := xrayconfig.InboundTagsForUser(cfg, sub.Email)
		tags = t
		return xrayconfig.RemoveUserFromAllInbounds(cfg, sub.Email)
	})
	
	if modErr != nil {
		r.log.Error("admin block user: xrayconfig update failed", "err", modErr)
		writeError(w, http.StatusInternalServerError, "failed to update xray config")
		return
	}
	
	if len(tags) > 0 {
		apiClient := xrayapi.New(r.cfg.Xray.APIAddr)
		_ = apiClient.RemoveUser(sub.Email, tags)
	}

	// 2. Add to limitedDB
	limitDB := userdb.New(r.cfg.Paths.LimitedDB)
	subfile := ""
	if sub.Metadata != nil {
		if sf, ok := sub.Metadata["subfile"].(string); ok {
			subfile = sf
		}
	}
	limitPtr := float64(sub.MaxDevices)
	_ = limitDB.Upsert(userdb.Entry{
		Email:   sub.Email,
		Subfile: subfile,
		Limit:   &limitPtr,
	})

	// 3. Update DB Status
	if result := db.Model(&sub).Updates(map[string]interface{}{
		"status":     "blocked",
		"updated_at": time.Now(),
	}); result.Error != nil {
		r.log.Error("admin block user: db status update failed", "err", result.Error)
		// We still return 200 because Xray was updated successfully
	}

	r.log.Warn("admin action", "action", "block", "email", email, "caller_ip", getClientIP(req))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// POST /api/v1/admin/users/{email}/unblock
// Body (optional): {"limit":5}  →  {"ok":true}
func (r *Router) handleAdminUnblockUser(w http.ResponseWriter, req *http.Request) {
	email := req.PathValue("email")
	if email == "" {
		writeError(w, http.StatusBadRequest, "email path parameter is required")
		return
	}

	// Optional body.
	var body struct {
		Limit *int `json:"limit"`
	}
	// Ignore body parse errors — body is optional.
	req.Body = http.MaxBytesReader(w, req.Body, 1<<20)
	data, _ := io.ReadAll(req.Body)
	if len(data) > 0 {
		_ = json.Unmarshal(data, &body)
	}

	db := database.DB()

	var sub database.Subscription
	if result := db.Where("email = ?", email).First(&sub); result.Error != nil {
		writeError(w, http.StatusNotFound, "subscription not found")
		return
	}

	updates := map[string]interface{}{
		"status":     "active",
		"updated_at": time.Now(),
	}
	if body.Limit != nil && *body.Limit > 0 {
		updates["max_devices"] = *body.Limit
	}

	if result := db.Model(&sub).Updates(updates); result.Error != nil {
		r.log.Error("admin unblock user", "err", result.Error)
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}

	// 2. Put user back into Xray config & API memory
	r.unbanUserInXray(sub)

	r.log.Warn("admin action", "action", "unblock", "email", email, "caller_ip", getClientIP(req))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// POST /api/v1/admin/users/{email}/set-expire
// Body: {"expire":"2026-07-04T00:00:00Z"}  →  {"ok":true}
func (r *Router) handleAdminSetExpire(w http.ResponseWriter, req *http.Request) {
	email := req.PathValue("email")
	if email == "" {
		writeError(w, http.StatusBadRequest, "email path parameter is required")
		return
	}

	var body struct {
		Expire string `json:"expire"`
	}
	if !readBody(w, req, &body) {
		return
	}
	if body.Expire == "" {
		writeError(w, http.StatusBadRequest, "expire is required")
		return
	}

	// Accept RFC3339 or plain date.
	var expireTime time.Time
	var err error
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05Z", "2006-01-02"} {
		expireTime, err = time.Parse(layout, body.Expire)
		if err == nil {
			break
		}
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid expire format, expected RFC3339 or YYYY-MM-DD")
		return
	}

	db := database.DB()

	var sub database.Subscription
	if result := db.Where("email = ?", email).First(&sub); result.Error != nil {
		writeError(w, http.StatusNotFound, "subscription not found")
		return
	}

	if result := db.Model(&sub).Updates(map[string]interface{}{
		"ends_at":    expireTime,
		"status":     "active",
		"updated_at": time.Now(),
	}); result.Error != nil {
		r.log.Error("admin set expire", "err", result.Error)
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}

	r.unbanUserInXray(sub)

	r.log.Warn("admin action", "action", "set-expire", "email", email, "caller_ip", getClientIP(req))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---------------------------------------------------------------------------
// Device Management
// ---------------------------------------------------------------------------

// GET /api/v1/users/telegram/{id}/devices
func (r *Router) handleGetDevices(w http.ResponseWriter, req *http.Request) {
	tgIDStr := req.PathValue("id")
	tgID, err := strconv.ParseInt(tgIDStr, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid telegram id")
		return
	}

	db := database.DB()
	user, err := findUserByTelegramID(db, tgID)
	if err != nil || user == nil {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}

	var sub database.Subscription
	if err := db.Where("user_id = ?", user.ID).Order("created_at desc").First(&sub).Error; err != nil {
		writeError(w, http.StatusNotFound, "subscription not found")
		return
	}

	var devices []database.Device
	if err := db.Where("subscription_id = ?", sub.ID).Find(&devices).Error; err != nil {
		writeError(w, http.StatusInternalServerError, "failed to query devices")
		return
	}

	writeJSON(w, http.StatusOK, devices)
}

// DELETE /api/v1/users/telegram/{id}/devices/{device_id}
func (r *Router) handleDeleteDevice(w http.ResponseWriter, req *http.Request) {
	tgIDStr := req.PathValue("id")
	deviceIDStr := req.PathValue("device_id")

	tgID, err := strconv.ParseInt(tgIDStr, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid telegram id")
		return
	}

	db := database.DB()
	user, err := findUserByTelegramID(db, tgID)
	if err != nil || user == nil {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}

	var sub database.Subscription
	if err := db.Where("user_id = ?", user.ID).Order("created_at desc").First(&sub).Error; err != nil {
		writeError(w, http.StatusNotFound, "subscription not found")
		return
	}

	var device database.Device
	if err := db.Where("id = ? AND subscription_id = ?", deviceIDStr, sub.ID).First(&device).Error; err != nil {
		writeError(w, http.StatusNotFound, "device not found")
		return
	}

	if err := db.Delete(&device).Error; err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete device")
		return
	}

	// Auto-unblock if limit is resolved
	var count int64
	db.Model(&database.Device{}).Where("subscription_id = ?", sub.ID).Count(&count)
	if count <= int64(sub.MaxDevices) && sub.Status == "blocked" {
		if sub.EndsAt == nil || sub.EndsAt.After(time.Now()) {
			db.Model(&sub).Update("status", "active")
			r.unbanUserInXray(sub)
			r.log.Info("auto-unblocked user after device deletion", "email", sub.Email)
		}
	}

	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (r *Router) unbanUserInXray(sub database.Subscription) {
	// Remove from limitedDB
	limitDB := userdb.New(r.cfg.Paths.LimitedDB)
	_ = limitDB.Remove(sub.Email)

	// We must re-add them to Xray config json & hot reload
	xrayCfg, err := xrayconfig.Read(r.cfg.Paths.XrayConfig)
	if err != nil {
		r.log.Error("failed to read xray config for unban", "err", err)
		return
	}

	subfile := ""
	if sub.Metadata != nil {
		if sf, ok := sub.Metadata["subfile"].(string); ok {
			subfile = sf
		}
	}
	expireVal := ""
	if sub.EndsAt != nil {
		expireVal = sub.EndsAt.Format("02.01.2006")
	}

	params := templates.ClientParams{
		Email:   sub.Email,
		UUID:    sub.XrayUUID,
		Auth:    "", // Not stored in DB. Will be generated if missing, but ideally we don't break hy2
		Subfile: subfile,
		Expire:  expireVal,
	}

	// Try to retain existing HY2 auth if they are somehow still in config
	if c, _ := xrayconfig.FindUser(xrayCfg, sub.Email); c != nil {
		if a := c.GetString("auth"); a != "" {
			params.Auth = a
		}
	}

	payload, err := templates.BuildForAllInbounds(r.cfg.Paths.TemplatesDir, xrayCfg, params)
	if err != nil {
		r.log.Error("failed to build payload for unban", "err", err)
		return
	}

	_ = xrayconfig.AddUserToInbounds(xrayCfg, payload)
	_ = xrayconfig.Write(r.cfg.Paths.XrayConfig, xrayCfg)

	apiClient := xrayapi.New(r.cfg.Xray.APIAddr)
	if err := apiClient.AddUser(payload, r.cfg.Paths.XrayConfig); err != nil {
		r.log.Error("hot-add failed", "email", sub.Email, "err", err)
	}
}
