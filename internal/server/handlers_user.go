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
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"golang.org/x/time/rate"
	"gorm.io/gorm"

	"xraytool/internal/convert"
	"xraytool/internal/database"
	"xraytool/internal/generate"
	"xraytool/internal/slave"
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
	var users []database.User
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

	result := query.Limit(1).Find(&users)
	if result.Error != nil {
		return nil, result.Error
	}
	if len(users) == 0 {
		return nil, gorm.ErrRecordNotFound
	}
	return &users[0], nil
}

// buildUserResponse assembles the full user JSON object the Python bot expects.
// It enriches a User with subscription, referral counts, and link.
func (r *Router) buildUserResponse(db *gorm.DB, user *database.User) map[string]interface{} {
	// Load the subscription for this user (there should be one; tolerate absence).
	var sub database.Subscription
	db.Where("user_id = ?", user.ID).Order("created_at desc").First(&sub)

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
	if r.cfg != nil && r.cfg.Server.Domain != "" && sub.ID != "" {
		link = fmt.Sprintf("https://%s/client?id=%s", r.cfg.Server.Domain, sub.ID)
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

	var activeDevices int64
	if sub.ID != "" {
		db.Model(&database.Device{}).Where("subscription_id = ?", sub.ID).Count(&activeDevices)
	}

	resp := map[string]interface{}{
		"id":                     user.ID,
		"username":               user.Username,
		"balance":                user.Balance,
		"is_admin":               user.IsAdmin,
		"is_blocked":             user.IsBlocked,
		"max_devices":            sub.MaxDevices,
		"active_devices":         activeDevices,
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

// buildUsersResponseBulk efficiently builds user responses for a batch of users using 4 queries total.
func (r *Router) buildUsersResponseBulk(db *gorm.DB, users []database.User) []map[string]interface{} {
	if len(users) == 0 {
		return []map[string]interface{}{}
	}

	userIds := make([]string, len(users))
	for i, u := range users {
		userIds[i] = u.ID
	}

	// 1. Fetch latest subscription for each user
	var subs []database.Subscription
	db.Where("user_id IN ?", userIds).Order("created_at desc").Find(&subs)
	
	subByUser := make(map[string]database.Subscription)
	subIds := make([]string, 0, len(subs))
	for _, s := range subs {
		// Only take the first (latest) we see since they are ordered by created_at desc
		if _, ok := subByUser[s.UserID]; !ok {
			subByUser[s.UserID] = s
			subIds = append(subIds, s.ID)
		}
	}

	// 2. Fetch referral counts and sums
	type RefStats struct {
		ReferrerID string
		Count      int64
		Total      int64
	}
	var refStats []RefStats
	db.Model(&database.ReferralReward{}).
		Where("referrer_id IN ?", userIds).
		Select("referrer_id, count(*) as count, coalesce(sum(amount),0) as total").
		Group("referrer_id").
		Scan(&refStats)

	refCountByUser := make(map[string]int64)
	refSumByUser := make(map[string]int64)
	for _, rs := range refStats {
		refCountByUser[rs.ReferrerID] = rs.Count
		refSumByUser[rs.ReferrerID] = rs.Total
	}

	// 3. Fetch active devices
	type DevStats struct {
		SubscriptionID string
		Count          int64
	}
	var devStats []DevStats
	if len(subIds) > 0 {
		db.Model(&database.Device{}).
			Where("subscription_id IN ?", subIds).
			Select("subscription_id, count(*) as count").
			Group("subscription_id").
			Scan(&devStats)
	}

	devCountBySub := make(map[string]int64)
	for _, ds := range devStats {
		devCountBySub[ds.SubscriptionID] = ds.Count
	}

	fmtTime := func(t *time.Time) interface{} {
		if t == nil || t.IsZero() {
			return nil
		}
		return t.UTC().Format(time.RFC3339)
	}

	out := make([]map[string]interface{}, 0, len(users))
	for _, user := range users {
		sub := subByUser[user.ID]

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

		link := ""
		if r.cfg != nil && r.cfg.Server.Domain != "" && sub.ID != "" {
			link = fmt.Sprintf("https://%s/client?id=%s", r.cfg.Server.Domain, sub.ID)
		}

		email := sub.Email
		if email == "" && tgID != 0 {
			email = fmt.Sprintf("bot_client_%d", tgID)
		}

		out = append(out, map[string]interface{}{
			"id":                     user.ID,
			"username":               user.Username,
			"balance":                user.Balance,
			"is_admin":               user.IsAdmin,
			"is_blocked":             user.IsBlocked,
			"max_devices":            sub.MaxDevices,
			"active_devices":         devCountBySub[sub.ID],
			"ref_code":               user.RefCode,
			"referred_by":            user.ReferredBy,
			"sub_status":             sub.Status,
			"ends_at":                fmtTime(sub.EndsAt),
			"starts_at":              fmtTime(sub.StartsAt),
			"auto_renew":             sub.AutoRenew,
			"referral_count":         refCountByUser[user.ID],
			"referral_earned_amount": refSumByUser[user.ID],
			"email":                  email,
			"link":                   link,
			"metadata":               user.Metadata,
			"created_at":             user.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	return out
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
		ReferredByCode   string `json:"referred_by_code"`
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

	var referredByID *string
	if body.ReferredByCode != "" {
		var referrer database.User
		if err := db.Where("ref_code = ?", body.ReferredByCode).First(&referrer).Error; err == nil {
			referredByID = &referrer.ID
		} else {
			r.log.Warn("register user: invalid ref code provided", "code", body.ReferredByCode)
		}
	}

	user := database.User{
		ID:         userID,
		Username:   body.Username,
		Balance:    0,
		IsAdmin:    false,
		RefCode:    refCode,
		ReferredBy: referredByID,
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

	out := r.buildUsersResponseBulk(db, users)
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
	if user.IsBlocked {
		writeError(w, http.StatusForbidden, "user is globally blocked")
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
	if user.IsBlocked {
		writeError(w, http.StatusForbidden, "user is globally blocked")
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
	if user.IsBlocked {
		writeError(w, http.StatusForbidden, "user is globally blocked")
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
		MaxDevices     int    `json:"max_devices"`
		PlanID         *int64 `json:"plan_id"`
	}
	if !readBody(w, req, &body) {
		return
	}
	if body.PlanTotalPrice < 0 || body.PlanTotalPrice > 10000000 {
		writeError(w, http.StatusBadRequest, "invalid plan total price")
		return
	}
	if body.PlanID == nil && body.NewEndsAt == "" {
		writeError(w, http.StatusBadRequest, "new_ends_at is required if plan_id is missing")
		return
	}
	if body.PlanID == nil && body.PlanTotalPrice <= 0 {
		writeError(w, http.StatusBadRequest, "plan_total_price must be positive if plan_id is missing")
		return
	}
	if body.MaxDevices == 0 {
		body.MaxDevices = 3
	}

	db := database.DB()
	user, err := findUserByTelegramID(db, tgID)
	if err != nil {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}

	var newEndsAt time.Time
	if body.PlanID != nil {
		var plan database.Plan
		if err := db.First(&plan, *body.PlanID).Error; err != nil {
			writeError(w, http.StatusBadRequest, "invalid plan_id")
			return
		}
		
		body.PlanTotalPrice = plan.BasePrice
		if plan.GlobalDiscountPercent > 0 {
			body.PlanTotalPrice = plan.BasePrice - (plan.BasePrice * plan.GlobalDiscountPercent / 100)
		}

		var sub database.Subscription
		db.Where("user_id = ?", user.ID).First(&sub)
		
		baseTime := time.Now()
		if sub.EndsAt != nil && sub.EndsAt.After(time.Now()) {
			baseTime = *sub.EndsAt
		}
		newEndsAt = baseTime.AddDate(0, plan.Months, 0)
	} else {
		newEndsAt, err = convert.ParseExpiryDate(body.NewEndsAt)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid new_ends_at format")
			return
		}
	}
	if user.IsBlocked {
		writeError(w, http.StatusForbidden, "user is globally blocked")
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
				"status":      "active",
				"ends_at":     newEndsAt,
				"max_devices": body.MaxDevices,
				"starts_at":   now,
				"updated_at":  now,
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
		// Delete any sent notification flags so they can be re-triggered when this sub nears expiration
		db.Where("subscription_id = ?", updatedSub.ID).Delete(&database.SubscriptionNotification{})
		
		go r.unbanUserInXray(updatedSub)
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
	if user.IsBlocked {
		writeError(w, http.StatusForbidden, "user is globally blocked")
		return
	}

	// Merge into existing metadata map.
	if user.Metadata == nil {
		user.Metadata = database.Metadata{}
	}
	user.Metadata[body.Key] = body.Value

	if result := db.Model(user).Updates(database.User{Metadata: user.Metadata}); result.Error != nil {
		r.log.Error("set metadata", "err", result.Error)
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}

	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ─────────────────────────────────────────────────────────────────────────────
// Admin handlers
// ─────────────────────────────────────────────────────────────────────────────

// GET /api/v1/admin/users
func (r *Router) handleAdminListUsers(w http.ResponseWriter, req *http.Request) {
	pageStr := req.URL.Query().Get("page")
	limitStr := req.URL.Query().Get("limit")
	search := req.URL.Query().Get("search")

	page := 1
	limit := 50
	if p, err := strconv.Atoi(pageStr); err == nil && p > 0 {
		page = p
	}
	if l, err := strconv.Atoi(limitStr); err == nil && l > 0 && l <= 100 {
		limit = l
	}

	db := database.DB()
	query := db.Model(&database.User{})

	if search != "" {
		if db.Dialector.Name() == "postgres" {
			likeQ := "%" + search + "%"
			query = query.Where("username ILIKE ? OR metadata::text ILIKE ?", likeQ, likeQ)
		} else {
			searchLower := strings.ToLower(search)
			searchUpper := strings.ToUpper(search)
			
			searchTitle := ""
			if r, n := utf8.DecodeRuneInString(searchLower); n > 0 {
				searchTitle = strings.ToUpper(string(r)) + searchLower[n:]
			}
			
			likeLower := "%" + searchLower + "%"
			likeUpper := "%" + searchUpper + "%"
			likeTitle := "%" + searchTitle + "%"
			likeOrig := "%" + search + "%"
			
			query = query.Where(
				"username LIKE ? OR username LIKE ? OR username LIKE ? OR username LIKE ? OR "+
				"metadata LIKE ? OR metadata LIKE ? OR metadata LIKE ? OR metadata LIKE ?",
				likeLower, likeUpper, likeTitle, likeOrig,
				likeLower, likeUpper, likeTitle, likeOrig,
			)
		}
	}

	var total int64
	query.Count(&total)

	var users []database.User
	if err := query.Offset((page - 1) * limit).Limit(limit).Find(&users).Error; err != nil {
		r.log.Error("admin list users: db query error", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to query users")
		return
	}

	out := r.buildUsersResponseBulk(db, users)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"total": total,
		"page":  page,
		"limit": limit,
		"users": out,
	})
}

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
	if result := db.Where("email = ?", email).Order("created_at desc").First(&sub); result.Error != nil {
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
		apiClient := xrayapi.NewGRPCClient(r.cfg.Xray.APIAddr)
		_ = apiClient.RemoveUser(sub.Email, tags)
	}

	// 2. Add to limitedDB (Removed)
	// 3. Update DB Status
	if result := db.Model(&sub).Updates(map[string]interface{}{
		"status":     "blocked",
		"updated_at": time.Now(),
	}); result.Error != nil {
		r.log.Error("admin block user: db status update failed", "err", result.Error)
		writeError(w, http.StatusInternalServerError, "failed to update db status")
		return
	}

	// 4. Propagate to slaves
	if r.cfg.IsMaster() {
		client := slave.NewClient(
			r.cfg.SlaveAPI.ConnectTimeout,
			r.cfg.SlaveAPI.RequestTimeout,
			r.cfg.SlaveAPI.RemotePath,
		)
		reg := slave.NewRegistry(r.cfg.Paths.ServersJSON, client)
		go reg.PropagateAll("rmuser", map[string]string{"email": sub.Email})
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
	if result := db.Where("email = ?", email).Order("created_at desc").First(&sub); result.Error != nil {
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

	// Also unblock the global user record just in case they were globally banned
	db.Model(&database.User{}).Where("id = ?", sub.UserID).Update("is_blocked", false)

	// If Anti-Fraud module is active, force lift any soft-ban on this user.
	if r.forceUnban != nil {
		r.forceUnban(email)
	}

	// Reload sub from DB so unbanUserInXray receives fresh max_devices / status.
	if err := db.Where("email = ?", email).Order("created_at desc").First(&sub).Error; err != nil {
		r.log.Error("admin unblock user: reload subscription", "err", err)
		writeError(w, http.StatusInternalServerError, "db reload error")
		return
	}

	// 2. Put user back into Xray config & API memory
	go r.unbanUserInXray(sub)

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

	expireTime, err := convert.ParseExpiryDate(body.Expire)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid expire format")
		return
	}

	db := database.DB()

	var sub database.Subscription
	if result := db.Where("email = ?", email).Order("created_at desc").First(&sub); result.Error != nil {
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

	// Also unblock the global user record just in case they were globally banned
	db.Model(&database.User{}).Where("id = ?", sub.UserID).Update("is_blocked", false)

	// Reload sub from DB so unbanUserInXray receives the updated ends_at / status.
	if err := db.Where("email = ?", email).Order("created_at desc").First(&sub).Error; err != nil {
		r.log.Error("admin set expire: reload subscription", "err", err)
		writeError(w, http.StatusInternalServerError, "db reload error")
		return
	}

	// Delete any sent notification flags so they can be re-triggered
	db.Where("subscription_id = ?", sub.ID).Delete(&database.SubscriptionNotification{})

	go r.unbanUserInXray(sub)

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
			go r.unbanUserInXray(sub)
			r.log.Info("auto-unblocked user after device deletion", "email", sub.Email)
		}
	}

	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (r *Router) unbanUserInXray(sub database.Subscription) {
	// Remove from limitedDB (Removed)
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

	limitF := float64(sub.MaxDevices)
	params := xrayconfig.ClientParams{
		Email:   sub.Email,
		UUID:    sub.XrayUUID,
		Auth:    "", 
		Subfile: subfile,
		Expire:  expireVal,
		Limit:   &limitF,
	}

	payload, err := xrayconfig.BuildForAllInbounds(xrayCfg, params)
	if err != nil {
		r.log.Error("failed to build payload for unban", "err", err)
		return
	}

	_ = xrayconfig.AddUserToInbounds(xrayCfg, payload)
	_ = xrayconfig.Write(r.cfg.Paths.XrayConfig, xrayCfg)

	apiClient := xrayapi.NewGRPCClient(r.cfg.Xray.APIAddr)
	if err := apiClient.AddUser(payload, r.cfg.Paths.XrayConfig); err != nil {
		r.log.Error("hot-add failed", "email", sub.Email, "err", err)
	}

	// Propagate to slaves
	if r.cfg.IsMaster() {
		client := slave.NewClient(
			r.cfg.SlaveAPI.ConnectTimeout,
			r.cfg.SlaveAPI.RequestTimeout,
			r.cfg.SlaveAPI.RemotePath,
		)
		reg := slave.NewRegistry(r.cfg.Paths.ServersJSON, client)

		slaveParams := map[string]string{
			"email":   sub.Email,
			"uuid":    sub.XrayUUID,
			"subfile": subfile,
			"expire":  expireVal,
			"auth":    "",
			"limit":   fmt.Sprintf("%.0f", limitF),
		}

		go func() {
			results := reg.PropagateAll("newuser", slaveParams)
			for _, res := range results {
				if res.Err != nil {
					r.log.Error("slave propagate newuser failed", "server", res.Server, "err", res.Err)
				}
			}
		}()
	}
}

// GET /api/v1/users/uuid/{id}
func (r *Router) handleGetUserByUUID(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "uuid path parameter is required")
		return
	}

	db := database.DB()
	var existing database.User
	if err := db.Where("id = ?", id).First(&existing).Error; err != nil {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}

	writeJSON(w, http.StatusOK, r.buildUserResponse(db, &existing))
}

// POST /api/v1/admin/users/telegram/{id}/global-ban
func (r *Router) handleAdminGlobalBan(w http.ResponseWriter, req *http.Request) {
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

	// Set IsBlocked to true
	if result := db.Model(user).Update("is_blocked", true); result.Error != nil {
		r.log.Error("admin global ban user: update db failed", "err", result.Error)
		writeError(w, http.StatusInternalServerError, "failed to update user")
		return
	}

	// Find the user's subscription to remove them from Xray
	var sub database.Subscription
	if err := db.Where("user_id = ?", user.ID).Order("created_at desc").First(&sub).Error; err == nil && sub.Email != "" {
		// Remove from Xray config
		var tags []string
		modErr := xrayconfig.Modify(r.cfg.Paths.XrayConfig, func(cfg xrayconfig.RawConfig) error {
			t, _ := xrayconfig.InboundTagsForUser(cfg, sub.Email)
			tags = t
			return xrayconfig.RemoveUserFromAllInbounds(cfg, sub.Email)
		})
		if modErr == nil && len(tags) > 0 {
			apiClient := xrayapi.NewGRPCClient(r.cfg.Xray.APIAddr)
			_ = apiClient.RemoveUser(sub.Email, tags)
		}
		
		// Optional: propagate to slaves
		if r.cfg.IsMaster() {
			client := slave.NewClient(
				r.cfg.SlaveAPI.ConnectTimeout,
				r.cfg.SlaveAPI.RequestTimeout,
				r.cfg.SlaveAPI.RemotePath,
			)
			reg := slave.NewRegistry(r.cfg.Paths.ServersJSON, client)
			go reg.PropagateAll("rmuser", map[string]string{"email": sub.Email})
		}
	}

	r.log.Warn("admin action", "action", "global-ban", "telegram_id", tgID, "caller_ip", getClientIP(req))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// POST /api/v1/admin/users/telegram/{id}/global-unban
func (r *Router) handleAdminGlobalUnban(w http.ResponseWriter, req *http.Request) {
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

	// Set IsBlocked to false
	if result := db.Model(user).Update("is_blocked", false); result.Error != nil {
		r.log.Error("admin global unban user: update db failed", "err", result.Error)
		writeError(w, http.StatusInternalServerError, "failed to update user")
		return
	}

	// If the subscription is active, re-add to Xray
	var sub database.Subscription
	if err := db.Where("user_id = ?", user.ID).Order("created_at desc").First(&sub).Error; err == nil && sub.Email != "" && sub.Status == "active" {
		go r.unbanUserInXray(sub)
	}

	r.log.Warn("admin action", "action", "global-unban", "telegram_id", tgID, "caller_ip", getClientIP(req))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// parseExpiryDate parses human-readable dates from the admin bot.
// parseExpiryDate has been moved to internal/convert

// handleAdminAntiFraudState returns the current snapshot of Anti-Fraud IPs
func (r *Router) handleAdminAntiFraudState(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if r.getSnapshot == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"enabled": false,
		})
		return
	}

	state := r.getSnapshot()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"enabled": true,
		"state":   state,
	})
}
