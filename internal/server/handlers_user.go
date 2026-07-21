package server

// handlers_user.go — implements all user-related REST handlers.
//
// Response shapes are designed to exactly match what the Python Telegram bot
// (clients-bot/sqltools.py) expects. Any deviation will break the bot.

import (
	"context"
	"errors"
	"fmt"
	json "github.com/goccy/go-json"
	"io"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"xraytool/internal/convert"
	"xraytool/internal/domain"
	"xraytool/internal/slave"
	"xraytool/internal/user"
)

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

// buildUserResponse assembles the full user JSON object the Python bot expects.
// It enriches a User with subscription, referral counts, and link.
func (r *Router) buildUserResponse(user *domain.User) map[string]interface{} {
	// Load the subscription for this user (there should be one; tolerate absence).
	var sub domain.Subscription
	subPtr, _ := r.userSvc.GetSubscriptionByUserID(context.Background(), user.ID)
	if subPtr != nil {
		sub = *subPtr
	}

	// Count referral rows where this user is the referrer.
	referralCount, _ := r.userSvc.CountReferrals(context.Background(), user.ID)

	// Sum referral rewards earned by this user.
	referralEarned, _ := r.userSvc.SumReferralRewards(context.Background(), user.ID)

	// Extract telegram_id from metadata
	tgID := extractTelegramID(user.Metadata)

	// Build the subscription link.
	link := ""
	if r.cfg != nil && r.cfg.Server.Domain != "" && sub.ID != "" {
		svc := r.userSvc
		link = svc.GenerateShareLink("", sub.ID)
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
		activeDevices, _ = r.userSvc.CountActiveDevices(context.Background(), sub.ID)
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
func (r *Router) buildUsersResponseBulk(users []domain.User) []map[string]interface{} {
	if len(users) == 0 {
		return []map[string]interface{}{}
	}

	userIds := make([]string, len(users))
	for i, u := range users {
		userIds[i] = u.ID
	}

	// 1. Fetch latest subscription for each user
	subs, _ := r.userSvc.GetLatestSubscriptionsByUserIDs(context.Background(), userIds)

	subByUser := make(map[string]domain.Subscription)
	subIds := make([]string, 0, len(subs))
	for _, s := range subs {
		// Only take the first (latest) we see since they are ordered by created_at desc
		if _, ok := subByUser[s.UserID]; !ok {
			subByUser[s.UserID] = s
			subIds = append(subIds, s.ID)
		}
	}

	// 2. Fetch referral counts and sums
	refStats, _ := r.userSvc.GetReferralStats(context.Background(), userIds)

	refCountByUser := make(map[string]int64)
	refSumByUser := make(map[string]int64)
	for _, rs := range refStats {
		refCountByUser[rs.ReferrerID] = rs.Count
		refSumByUser[rs.ReferrerID] = rs.Total
	}

	// 3. Fetch active devices
	devCountBySub := make(map[string]int64)
	if len(subIds) > 0 {
		devCountBySub, _ = r.userSvc.CountDevicesBySubscriptions(context.Background(), subIds)
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

		tgID := extractTelegramID(user.Metadata)

		link := ""
		if r.cfg != nil && r.cfg.Server.Domain != "" && sub.ID != "" {
			svc := r.userSvc
			link = svc.GenerateShareLink("", sub.ID)
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

	// Idempotency: if user already exists return it.
	tgIDStr := strconv.FormatInt(body.TelegramID, 10)
	existing, err := r.userSvc.FindUserByPlatformID(context.Background(), "telegram", tgIDStr)
	if err == nil && existing != nil {
		writeJSON(w, http.StatusOK, r.buildUserResponse(existing))
		return
	}

	u, err := r.userSvc.RegisterTelegramUser(context.Background(), user.RegisterTelegramUserRequest{
		TelegramID:       body.TelegramID,
		Username:         body.Username,
		TelegramUsername: body.TelegramUsername,
		ReferredByCode:   body.ReferredByCode,
	})
	if err != nil {
		r.log.Error("register user", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to create user")
		return
	}

	writeJSON(w, http.StatusCreated, r.buildUserResponse(u))
}

// ─────────────────────────────────────────────────────────────────────────────
// GET /api/v1/users
// Returns array of full user objects.
// ─────────────────────────────────────────────────────────────────────────────

func (r *Router) handleListUsers(w http.ResponseWriter, req *http.Request) {
	users, err := r.userSvc.FindAllUsers(context.Background())
	if err != nil {
		r.log.Error("list users", "err", err)
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}

	out := r.buildUsersResponseBulk(users)
	writeJSON(w, http.StatusOK, out)
}

// ─────────────────────────────────────────────────────────────────────────────
// GET /api/v1/users/admins
// Returns [123456, 789012] — array of telegram IDs for admin users.
// ─────────────────────────────────────────────────────────────────────────────

func (r *Router) handleListAdmins(w http.ResponseWriter, req *http.Request) {
	users, err := r.userSvc.FindAdmins(context.Background())
	if err != nil {
		r.log.Error("list admins", "err", err)
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}

	ids := make([]int64, 0, len(users))
	for _, u := range users {
		if id := extractTelegramID(u.Metadata); id != 0 {
			ids = append(ids, id)
		}
	}
	writeJSON(w, http.StatusOK, ids)
}

// emailRe is a permissive RFC-5322-like regex for basic email validation.
// We intentionally keep it simple — the Resend API will reject truly invalid
// addresses at delivery time.
var emailRe = regexp.MustCompile(`^[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}$`)

// normalizeEmail lower-cases and trims whitespace so that Admin@Mail.com and
// admin@mail.com resolve to the same account.
func normalizeEmail(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

func extractTelegramID(metadata domain.Metadata) int64 {
	if metadata == nil {
		return 0
	}
	switch v := metadata["telegram_id"].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	case string:
		parsed, _ := strconv.ParseInt(v, 10, 64)
		return parsed
	default:
		return 0
	}
}

// findOrCreateWebUser looks up a user by email. If the user does not exist, it
// creates a new User + Subscription record inside a single DB transaction.
// This implements the "open registration" flow (Variant A).
func (r *Router) findOrCreateWebUser(email string) (*domain.User, error) {
	return r.userSvc.FindOrCreateWebUser(context.Background(), email)
}

// ─────────────────────────────────────────────────────────────────────────────
// POST /api/v1/users/request_code
// Body (Telegram bot):  {"telegram_id": 123}
// Body (Web / email):   {"platform": "web", "email": "user@example.com"}
// ─────────────────────────────────────────────────────────────────────────────

func (r *Router) handleRequestCode(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Platform   string `json:"platform"`
		TelegramID int64  `json:"telegram_id"` // kept for bot backwards-compatibility
		Email      string `json:"email"`
	}
	if !readBody(w, req, &body) {
		return
	}

	// ── Web / Email flow ──────────────────────────────────────────────────────
	if body.Platform == "web" {
		email := normalizeEmail(body.Email)
		if !emailRe.MatchString(email) {
			writeError(w, http.StatusBadRequest, "invalid email address")
			return
		}

		// Find or auto-create the user account.
		_, err := r.findOrCreateWebUser(email)
		if err != nil {
			r.log.Error("request_code: findOrCreateWebUser", "email", email, "err", err)
			writeError(w, http.StatusInternalServerError, "failed to process user")
			return
		}

		code, err := requestOTP(email, 5*time.Minute)
		if err != nil {
			if errors.Is(err, ErrMaxRequestsReached) {
				r.log.Warn("request_code: rate limited", "email", email, "ip", getClientIP(req))
				writeError(w, http.StatusTooManyRequests, "too many requests, try again later")
				return
			}
			writeError(w, http.StatusInternalServerError, "failed to generate otp")
			return
		}

		// Send email asynchronously — do not block the HTTP response.
		if r.mailer != nil {
			go func() {
				if sendErr := r.mailer.SendCode(email, code); sendErr != nil {
					r.log.Error("request_code: mailer.SendCode failed", "email", email, "err", sendErr)
				}
			}()
		} else {
			// Fallback for development: log the code instead of sending email.
			r.log.Warn("request_code: mailer not configured, logging code for debug", "email", email, "code", code)
		}

		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}

	// ── Telegram flow (backwards-compatible) ──────────────────────────────────
	if body.TelegramID == 0 {
		writeError(w, http.StatusBadRequest, "telegram_id or (platform=web + email) is required")
		return
	}

	tgIDStr := strconv.FormatInt(body.TelegramID, 10)
	_, err := r.userSvc.FindUserByPlatformID(context.Background(), "telegram", tgIDStr)
	if err != nil {
		writeError(w, http.StatusNotFound, "user not found. please start the bot first")
		return
	}

	code, err := requestOTP(tgIDStr, 5*time.Minute)
	if err != nil {
		if errors.Is(err, ErrMaxRequestsReached) {
			r.log.Warn("request_code: rate limited", "telegram_id", body.TelegramID, "ip", getClientIP(req))
			writeError(w, http.StatusTooManyRequests, "too many requests, try again later")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to generate otp")
		return
	}

	if r.dispatcher != nil {
		r.dispatcher.Dispatch("auth.request_code", map[string]interface{}{
			"telegram_id": body.TelegramID,
			"code":        code,
		}, nil)
	} else {
		r.log.Warn("request_code: dispatcher is nil, cannot send webhook")
	}

	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ─────────────────────────────────────────────────────────────────────────────
// POST /api/v1/users/verify_code
// Body (Telegram bot):  {"telegram_id": 123, "code": "123456"}
// Body (Web / email):   {"platform": "web", "email": "user@example.com", "code": "123456"}
// ─────────────────────────────────────────────────────────────────────────────

func (r *Router) handleVerifyCode(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Platform   string `json:"platform"`
		TelegramID int64  `json:"telegram_id"`
		Email      string `json:"email"`
		Code       string `json:"code"`
	}
	if !readBody(w, req, &body) {
		return
	}
	if body.Code == "" {
		writeError(w, http.StatusBadRequest, "code is required")
		return
	}

	var identifier string
	if body.Platform == "web" {
		email := normalizeEmail(body.Email)
		if !emailRe.MatchString(email) {
			writeError(w, http.StatusBadRequest, "invalid email address")
			return
		}
		identifier = email
	} else {
		if body.TelegramID == 0 {
			writeError(w, http.StatusBadRequest, "telegram_id or (platform=web + email) is required")
			return
		}
		identifier = strconv.FormatInt(body.TelegramID, 10)
	}

	ok, _, err := verifyOTP(identifier, body.Code)
	if err != nil {
		if errors.Is(err, ErrMaxAttemptsReached) {
			r.log.Warn("verify_code: brute force detected", "identifier", identifier, "ip", getClientIP(req))
			writeError(w, http.StatusForbidden, "too many failed attempts. please request a new code")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !ok {
		writeError(w, http.StatusUnauthorized, "invalid or expired code")
		return
	}

	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ─────────────────────────────────────────────────────────────────────────────
// POST /api/v1/users/link_session
// Body: {"session_id": "uuid...", "telegram_id": 123456}
// ─────────────────────────────────────────────────────────────────────────────

func (r *Router) handleLinkSession(w http.ResponseWriter, req *http.Request) {
	var body struct {
		SessionID  string `json:"session_id"`
		TelegramID int64  `json:"telegram_id"`
	}
	if !readBody(w, req, &body) {
		return
	}
	if body.SessionID == "" || body.TelegramID == 0 {
		writeError(w, http.StatusBadRequest, "session_id and telegram_id are required")
		return
	}
	if len(body.SessionID) != 36 || body.SessionID[8] != '-' || body.SessionID[13] != '-' || body.SessionID[18] != '-' || body.SessionID[23] != '-' {
		writeError(w, http.StatusBadRequest, "invalid session_id format")
		return
	}

	tgIDStr := strconv.FormatInt(body.TelegramID, 10)
	
	// Rate limit: max 2 requests per 30 seconds per telegram_id to prevent DoS
	_, rateLimitErr := requestOTP("tg_ratelimit_"+tgIDStr, 30*time.Second)
	if errors.Is(rateLimitErr, ErrMaxRequestsReached) {
		writeError(w, http.StatusTooManyRequests, "too many requests, please wait")
		return
	}

	code, err := requestOTPWithPayload(body.SessionID, tgIDStr, 5*time.Minute)
	if err != nil {
		if errors.Is(err, ErrMaxRequestsReached) {
			writeError(w, http.StatusTooManyRequests, "too many requests")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to generate otp")
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":   true,
		"code": code,
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// POST /api/v1/users/verify_session
// Body: {"session_id": "uuid...", "code": "123456"}
// ─────────────────────────────────────────────────────────────────────────────

func (r *Router) handleVerifySession(w http.ResponseWriter, req *http.Request) {
	var body struct {
		SessionID string `json:"session_id"`
		Code      string `json:"code"`
	}
	if !readBody(w, req, &body) {
		return
	}
	if body.SessionID == "" || body.Code == "" {
		writeError(w, http.StatusBadRequest, "session_id and code are required")
		return
	}

	ok, payload, err := verifyOTP(body.SessionID, body.Code)
	if err != nil {
		if errors.Is(err, ErrMaxAttemptsReached) {
			writeError(w, http.StatusForbidden, "too many failed attempts")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !ok {
		writeError(w, http.StatusUnauthorized, "invalid or expired code")
		return
	}

	tgIDStr := payload
	if tgIDStr == "" {
		writeError(w, http.StatusUnauthorized, "invalid session payload")
		return
	}

	user, err := r.userSvc.FindUserByPlatformID(context.Background(), "telegram", tgIDStr)
	if err != nil {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}

	tgIDInt, _ := strconv.ParseInt(tgIDStr, 10, 64)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":          true,
		"telegram_id": tgIDInt,
		"is_admin":    user.IsAdmin,
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// GET /api/v1/users/{platform}/{id}
// ─────────────────────────────────────────────────────────────────────────────

func (r *Router) handleGetUserByPlatform(w http.ResponseWriter, req *http.Request) {
	platform := req.PathValue("platform")
	idStr := req.PathValue("id")

	user, err := r.userSvc.FindUserByPlatformID(context.Background(), platform, idStr)
	if err != nil {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}
	writeJSON(w, http.StatusOK, r.buildUserResponse(user))
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

	user, err := r.userSvc.FindUserByRefCode(context.Background(), code)
	if err != nil {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}
	writeJSON(w, http.StatusOK, r.buildUserResponse(user))
}

// ─────────────────────────────────────────────────────────────────────────────
// POST /api/v1/users/{platform}/{id}/balance
// Body: {"amount":100}  →  {"balance":200}
// ─────────────────────────────────────────────────────────────────────────────

func (r *Router) handleAdjustBalance(w http.ResponseWriter, req *http.Request) {
	platform := req.PathValue("platform")
	idStr := req.PathValue("id")

	var body struct {
		Amount int `json:"amount"`
	}
	if !readBody(w, req, &body) {
		return
	}
	if body.Amount <= 0 || body.Amount > 100000000 {
		writeError(w, http.StatusBadRequest, "amount must be positive and less than 100000000")
		return
	}

	user, err := r.userSvc.FindUserByPlatformID(context.Background(), platform, idStr)
	if err != nil {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}
	if user.IsBlocked {
		writeError(w, http.StatusForbidden, "user is globally blocked")
		return
	}

	// Atomic update using raw query
	if err := r.userSvc.AdjustBalance(context.Background(), user.ID, body.Amount); err != nil {
		r.log.Error("adjust balance", "err", err)
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}

	updatedUser, _ := r.userSvc.FindUserByID(context.Background(), user.ID)
	writeJSON(w, http.StatusOK, map[string]interface{}{"balance": updatedUser.Balance})
}

// ─────────────────────────────────────────────────────────────────────────────
// POST /api/v1/users/{platform}/{id}/max-devices
// Body: {"max_devices":5}  →  {"ok":true}
// ─────────────────────────────────────────────────────────────────────────────

func (r *Router) handleSetMaxDevices(w http.ResponseWriter, req *http.Request) {
	platform := req.PathValue("platform")
	idStr := req.PathValue("id")

	var body struct {
		MaxDevices int `json:"max_devices"`
	}
	if !readBody(w, req, &body) {
		return
	}
	if body.MaxDevices <= 0 || body.MaxDevices > 100 {
		writeError(w, http.StatusBadRequest, "max_devices must be between 1 and 100")
		return
	}

	user, err := r.userSvc.FindUserByPlatformID(context.Background(), platform, idStr)
	if err != nil {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}
	if user.IsBlocked {
		writeError(w, http.StatusForbidden, "user is globally blocked")
		return
	}

	if err := r.userSvc.UpdateMaxDevices(context.Background(), user.ID, body.MaxDevices); err != nil {
		r.log.Error("set max devices", "err", err)
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
	platform := req.PathValue("platform")
	idStr := req.PathValue("id")

	var body struct {
		AutoRenew bool `json:"auto_renew"`
	}
	if !readBody(w, req, &body) {
		return
	}

	user, err := r.userSvc.FindUserByPlatformID(context.Background(), platform, idStr)
	if err != nil {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}
	if user.IsBlocked {
		writeError(w, http.StatusForbidden, "user is globally blocked")
		return
	}

	if err := r.userSvc.UpdateAutoRenew(context.Background(), user.ID, body.AutoRenew); err != nil {
		r.log.Error("auto-renew toggle", "err", err)
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}

	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ─────────────────────────────────────────────────────────────────────────────
// POST /api/v1/users/{platform}/{id}/auto-renew
// Body: {"plan_total_price":159,"new_ends_at":"2026-07-04T..."}
// Atomically deducts balance and extends subscription. →  {"ok":true}
// ─────────────────────────────────────────────────────────────────────────────

func (r *Router) handleAutoRenew(w http.ResponseWriter, req *http.Request) {
	platform := req.PathValue("platform")
	idStr := req.PathValue("id")

	var body struct {
		PlanTotalPrice int    `json:"plan_total_price"`
		NewEndsAt      string `json:"new_ends_at"`
		MaxDevices     int    `json:"max_devices"`
		PlanID         *int64 `json:"plan_id"`
		PromoCode      string `json:"promo_code"`
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
	if body.MaxDevices <= 0 {
		body.MaxDevices = 3
	}
	if body.MaxDevices > 100 {
		writeError(w, http.StatusBadRequest, "max_devices must be between 1 and 100")
		return
	}

	user, err := r.userSvc.FindUserByPlatformID(context.Background(), platform, idStr)
	if err != nil {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}
	if user.IsBlocked {
		writeError(w, http.StatusForbidden, "user is globally blocked")
		return
	}

	var newEndsAtPtr *time.Time

	if body.PlanID != nil {
		plans, err := r.paymentSvc.FindActivePlans(context.Background())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to get plans")
			return
		}
		var plan *domain.Plan
		for _, p := range plans {
			if p.ID == *body.PlanID {
				planCopy := p
				plan = &planCopy
				break
			}
		}
		if plan == nil {
			writeError(w, http.StatusBadRequest, "invalid plan_id")
			return
		}
		
		extraDevicesCost := 0
		if body.MaxDevices > 3 {
			extraDevicesCost = (body.MaxDevices - 3) * 40 * plan.Months
		}
		
		sub, subErr := r.userSvc.GetSubscriptionByUserID(context.Background(), user.ID)
		if subErr == nil && sub.EndsAt != nil && sub.EndsAt.After(time.Now()) && body.MaxDevices > sub.MaxDevices {
			remainingDuration := sub.EndsAt.Sub(time.Now())
			remainingDays := float64(remainingDuration.Hours() / 24.0)
			if remainingDays > 7 {
				upgradeMonths := int(math.Ceil(remainingDays / 30.0))
				currentDevices := sub.MaxDevices
				if currentDevices < 3 {
					currentDevices = 3
				}
				newExtraDevices := body.MaxDevices - currentDevices
				if newExtraDevices > 0 {
					extraDevicesCost += newExtraDevices * 40 * upgradeMonths
				}
			}
		}
		
		baseAmount := plan.BasePrice + extraDevicesCost
		globalPrice := baseAmount
		if plan.GlobalDiscountPercent > 0 {
			globalPrice = baseAmount - (baseAmount * plan.GlobalDiscountPercent / 100)
		}
		
		promoPrice := baseAmount
		if body.PromoCode != "" {
			code := strings.ToUpper(strings.TrimSpace(body.PromoCode))
			promo, err := r.paymentSvc.FindPromoCodeByCode(context.Background(), code)
			if err == nil {
				if promo.IsActive && (promo.ExpiresAt == nil || time.Now().Before(*promo.ExpiresAt)) {
					// We only check if it applies to platform
					if promo.TargetPlatform == "all" || promo.TargetPlatform == platform {
						promoPrice = baseAmount - (baseAmount * promo.DiscountPercent / 100)
					}
				}
			}
		}
		
		finalAmount := globalPrice
		if promoPrice < globalPrice {
			finalAmount = promoPrice
		}
		
		body.PlanTotalPrice = finalAmount

		// Also calculate the date since plan is specified
		baseTime := time.Now()
		if subErr == nil && sub.EndsAt != nil && sub.EndsAt.After(time.Now()) {
			baseTime = *sub.EndsAt
		}
		newEnds := baseTime.AddDate(0, plan.Months, 0)
		newEndsAtPtr = &newEnds
	} else if body.NewEndsAt != "" {
		t, err := time.Parse(time.RFC3339, body.NewEndsAt)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid new_ends_at format")
			return
		}
		newEndsAtPtr = &t
	}

	txErr := r.userSvc.AutoRenewSubscription(context.Background(), user.ID, body.PlanID, body.PlanTotalPrice, newEndsAtPtr, body.MaxDevices)
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
	updatedSub, err := r.userSvc.GetSubscriptionByUserID(context.Background(), user.ID)
	if err == nil {
		// Delete any sent notification flags so they can be re-triggered when this sub nears expiration
		r.userSvc.DeleteNotificationsBySubID(context.Background(), updatedSub.ID) //nolint:errcheck

		r.unbanUserInXrayAsync(*updatedSub)
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
	platform := req.PathValue("platform")
	idStr := req.PathValue("id")

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

	user, err := r.userSvc.FindUserByPlatformID(context.Background(), platform, idStr)
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
		user.Metadata = domain.Metadata{}
	}
	user.Metadata[body.Key] = body.Value

	if err := r.userSvc.UpdateUser(context.Background(), user); err != nil {
		r.log.Error("set metadata", "err", err)
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

	users, total, err := r.userSvc.ListUsers(context.Background(), page, limit, search)
	if err != nil {
		r.log.Error("admin list users", "err", err)
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}

	out := r.buildUsersResponseBulk(users)

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

	// Find subscription by email.
	subPtr, err := r.userSvc.GetSubscriptionByEmail(context.Background(), email)
	if err != nil {
		writeError(w, http.StatusNotFound, "subscription not found")
		return
	}
	sub := *subPtr

	// 1. Remove from engine config and memory via abstraction
	if err := r.engine.RemoveUser(context.Background(), sub.Email); err != nil {
		r.log.Error("admin block user: engine remove failed", "email", sub.Email, "err", err)
		writeError(w, http.StatusInternalServerError, "failed to remove user from engine")
		return
	}

	// 2. Add to limitedDB (Removed)
	// 3. Update DB Status
	if err := r.userSvc.UpdateSubscriptionFields(context.Background(), sub.ID, map[string]interface{}{
		"status":     "blocked",
		"updated_at": time.Now(),
	}); err != nil {
		r.log.Error("admin block user: db status update failed", "err", err)
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
		reg := slave.NewRegistry(r.cfg.SlaveServers, client)
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

	sub, err := r.userSvc.GetSubscriptionByEmail(context.Background(), email)
	if err != nil {
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

	if err := r.userSvc.UpdateSubscriptionFields(context.Background(), sub.ID, updates); err != nil {
		r.log.Error("admin unblock user", "err", err)
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}

	// Also unblock the global user record just in case they were globally banned
	r.userSvc.UpdateIsBlocked(context.Background(), sub.UserID, false) //nolint:errcheck

	// If Anti-Fraud module is active, force lift any soft-ban on this user.
	if r.forceUnban != nil {
		r.forceUnban(email)
	}

	// Reload sub from DB so unbanUserInXray receives fresh max_devices / status.
	sub, err = r.userSvc.GetSubscriptionByEmail(context.Background(), email)
	if err != nil {
		r.log.Error("admin unblock user: reload subscription", "err", err)
		writeError(w, http.StatusInternalServerError, "db reload error")
		return
	}

	// 2. Put user back into Xray config & API memory
	r.unbanUserInXrayAsync(*sub)

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

	sub, err := r.userSvc.GetSubscriptionByEmail(context.Background(), email)
	if err != nil {
		writeError(w, http.StatusNotFound, "subscription not found")
		return
	}

	if err := r.userSvc.UpdateSubscriptionFields(context.Background(), sub.ID, map[string]interface{}{
		"ends_at":    expireTime,
		"status":     "active",
		"updated_at": time.Now(),
	}); err != nil {
		r.log.Error("admin set expire", "err", err)
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}

	// Also unblock the global user record just in case they were globally banned
	r.userSvc.UpdateIsBlocked(context.Background(), sub.UserID, false) //nolint:errcheck

	// Reload sub from DB so unbanUserInXray receives the updated ends_at / status.
	sub, err = r.userSvc.GetSubscriptionByEmail(context.Background(), email)
	if err != nil {
		r.log.Error("admin set expire: reload subscription", "err", err)
		writeError(w, http.StatusInternalServerError, "db reload error")
		return
	}

	// Delete any sent notification flags so they can be re-triggered
	r.userSvc.DeleteNotificationsBySubID(context.Background(), sub.ID) //nolint:errcheck

	r.unbanUserInXrayAsync(*sub)

	r.log.Warn("admin action", "action", "set-expire", "email", email, "caller_ip", getClientIP(req))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---------------------------------------------------------------------------
// Device Management
// ---------------------------------------------------------------------------

// GET /api/v1/users/{platform}/{id}/devices
func (r *Router) handleGetDevices(w http.ResponseWriter, req *http.Request) {
	platform := req.PathValue("platform")
	idStr := req.PathValue("id")

	user, err := r.userSvc.FindUserByPlatformID(context.Background(), platform, idStr)
	if err != nil || user == nil {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}

	subPtr, _ := r.userSvc.GetSubscriptionByUserID(context.Background(), user.ID)
	if err != nil {
		writeError(w, http.StatusNotFound, "subscription not found")
		return
	}
	sub := *subPtr

	devices, err := r.userSvc.FindDevicesBySubscriptionID(context.Background(), sub.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to query devices")
		return
	}

	writeJSON(w, http.StatusOK, devices)
}

// DELETE /api/v1/users/{platform}/{id}/devices/{device_id}
func (r *Router) handleDeleteDevice(w http.ResponseWriter, req *http.Request) {
	platform := req.PathValue("platform")
	idStr := req.PathValue("id")
	deviceIDStr := req.PathValue("device_id")

	user, err := r.userSvc.FindUserByPlatformID(context.Background(), platform, idStr)
	if err != nil || user == nil {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}

	subPtr, _ := r.userSvc.GetSubscriptionByUserID(context.Background(), user.ID)
	if err != nil {
		writeError(w, http.StatusNotFound, "subscription not found")
		return
	}
	sub := *subPtr

	deviceID, err := strconv.ParseInt(deviceIDStr, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid device ID")
		return
	}

	device, err := r.userSvc.FindDeviceByIDAndSubscription(context.Background(), deviceID, sub.ID)
	if err != nil {
		writeError(w, http.StatusNotFound, "device not found")
		return
	}

	if err := r.userSvc.DeleteDevice(context.Background(), device.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete device")
		return
	}

	// Auto-unblock if limit is resolved
	count, _ := r.userSvc.CountActiveDevices(context.Background(), sub.ID)
	if count <= int64(sub.MaxDevices) && sub.Status == "blocked" {
		if sub.EndsAt == nil || sub.EndsAt.After(time.Now()) {
			r.userSvc.UpdateSubscriptionFields(context.Background(), sub.ID, map[string]interface{}{"status": "active"}) //nolint:errcheck
			r.unbanUserInXrayAsync(sub)
			r.log.Info("auto-unblocked user after device deletion", "email", sub.Email)
		}
	}

	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (r *Router) unbanUserInXrayAsync(sub domain.Subscription) {
	r.bgTasks.Add(1)
	go func() {
		defer r.bgTasks.Done()
		r.unbanUserInXray(sub)
	}()
}

func (r *Router) unbanUserInXray(sub domain.Subscription) {
	// Remove from limitedDB (Removed)
	// We must re-add them to Xray config json & hot reload
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

	limitInt := sub.MaxDevices

	userCfg := domain.VPNUserConfig{
		Email:      sub.Email,
		UUID:       sub.XrayUUID,
		Subfile:    subfile,
		Expire:     expireVal,
		MaxDevices: limitInt,
	}

	if err := r.engine.AddUser(context.Background(), userCfg); err != nil {
		r.log.Error("failed to add user to engine for unban", "email", sub.Email, "err", err)
	}

	if r.cfg.IsMaster() {
		client := slave.NewClient(
			r.cfg.SlaveAPI.ConnectTimeout,
			r.cfg.SlaveAPI.RequestTimeout,
			r.cfg.SlaveAPI.RemotePath,
		)
		reg := slave.NewRegistry(r.cfg.SlaveServers, client)

		slaveParams := map[string]string{
			"email":   sub.Email,
			"uuid":    sub.XrayUUID,
			"subfile": subfile,
			"expire":  expireVal,
			"auth":    "",
			"limit":   fmt.Sprintf("%d", limitInt),
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

// POST /api/v1/admin/users/{platform}/{id}/global-ban
func (r *Router) handleAdminGlobalBan(w http.ResponseWriter, req *http.Request) {
	platform := req.PathValue("platform")
	idStr := req.PathValue("id")

	user, err := r.userSvc.FindUserByPlatformID(context.Background(), platform, idStr)
	if err != nil {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}

	// Set IsBlocked to true
	if err := r.userSvc.UpdateIsBlocked(context.Background(), user.ID, true); err != nil {
		r.log.Error("admin global ban user: update db failed", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to update user")
		return
	}

	// Find the user's subscription to remove them from Xray
	subPtr, _ := r.userSvc.GetSubscriptionByUserID(context.Background(), user.ID)
	if err == nil && subPtr != nil && subPtr.Email != "" {
		sub := *subPtr
		if err := r.engine.RemoveUser(context.Background(), sub.Email); err != nil {
			r.log.Warn("global-ban: engine remove failed", "email", sub.Email, "err", err)
		}

		// Optional: propagate to slaves
		if r.cfg.IsMaster() {
			client := slave.NewClient(
				r.cfg.SlaveAPI.ConnectTimeout,
				r.cfg.SlaveAPI.RequestTimeout,
				r.cfg.SlaveAPI.RemotePath,
			)
			reg := slave.NewRegistry(r.cfg.SlaveServers, client)
			go reg.PropagateAll("rmuser", map[string]string{"email": sub.Email})
		}
	}

	r.log.Warn("admin action", "action", "global-ban", "id", idStr, "caller_ip", getClientIP(req))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// POST /api/v1/admin/users/{platform}/{id}/global-unban
func (r *Router) handleAdminGlobalUnban(w http.ResponseWriter, req *http.Request) {
	platform := req.PathValue("platform")
	idStr := req.PathValue("id")

	user, err := r.userSvc.FindUserByPlatformID(context.Background(), platform, idStr)
	if err != nil {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}

	// Set IsBlocked to false
	if err := r.userSvc.UpdateIsBlocked(context.Background(), user.ID, false); err != nil {
		r.log.Error("admin global unban user: update db failed", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to update user")
		return
	}

	// If the subscription is active, re-add to Xray
	subPtr, _ := r.userSvc.GetSubscriptionByUserID(context.Background(), user.ID)
	if err == nil && subPtr != nil && subPtr.Email != "" && subPtr.Status == "active" {
		r.unbanUserInXrayAsync(*subPtr)
	}

	r.log.Warn("admin action", "action", "global-unban", "id", idStr, "caller_ip", getClientIP(req))
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

	snapshot := r.getSnapshot()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"enabled":       true,
		"state":         snapshot.State,
		"active_slaves": snapshot.ActiveSlaves,
	})
}

// handleAdminDeleteUser completely removes a user and all associated data from the DB and Xray config.
func (r *Router) handleAdminDeleteUser(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodDelete {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	platform := req.PathValue("platform")
	idStr := req.PathValue("id")

	user, err := r.userSvc.FindUserByPlatformID(context.Background(), platform, idStr)
	if err != nil {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}

	subPtr, _ := r.userSvc.GetSubscriptionByUserID(context.Background(), user.ID) //nolint:ineffassign //nolint:ineffassign //nolint:staticcheck //nolint:staticcheck //nolint:staticcheck

	if err := r.userSvc.DeleteUserAndData(context.Background(), user.ID); err != nil {
		r.log.Error("admin delete user: db transaction failed", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to delete user")
		return
	}

	// 5. Remove from Xray config and memory
	if subPtr != nil {
		sub := *subPtr
		if err := r.engine.RemoveUser(context.Background(), sub.Email); err != nil {
			r.log.Error("failed to remove user from engine", "email", sub.Email, "err", err)
		}

		if r.cfg.IsMaster() {
			client := slave.NewClient(
				r.cfg.SlaveAPI.ConnectTimeout,
				r.cfg.SlaveAPI.RequestTimeout,
				r.cfg.SlaveAPI.RemotePath,
			)
			reg := slave.NewRegistry(r.cfg.SlaveServers, client)
			go reg.PropagateAll("rmuser", map[string]string{"email": sub.Email})
		}
	}

	r.log.Warn("admin action", "action", "delete-user", "id", idStr, "caller_ip", getClientIP(req))
	writeJSON(w, http.StatusOK, map[string]interface{}{"status": "success", "ok": true, "message": "User permanently deleted"})
}

// ─────────────────────────────────────────────────────────────────────────────
// POST /api/v1/users/link/telegram
// Body: {"session_id": "uuid...", "code": "123456", "email": "user email"}
// ─────────────────────────────────────────────────────────────────────────────

func (r *Router) handleLinkTelegram(w http.ResponseWriter, req *http.Request) {
	var body struct {
		SessionID string `json:"session_id"`
		Code      string `json:"code"`
		Email     string `json:"email"`
	}
	if !readBody(w, req, &body) {
		return
	}
	if body.SessionID == "" || body.Code == "" || body.Email == "" {
		writeError(w, http.StatusBadRequest, "session_id, code and email are required")
		return
	}

	ok, payload, err := verifyOTP(body.SessionID, body.Code)
	if err != nil {
		if errors.Is(err, ErrMaxAttemptsReached) {
			writeError(w, http.StatusForbidden, "too many failed attempts")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !ok {
		writeError(w, http.StatusUnauthorized, "invalid or expired code")
		return
	}

	tgIDStr := payload
	if tgIDStr == "" {
		writeError(w, http.StatusUnauthorized, "invalid session payload")
		return
	}

	// Find the tg user to get tgUserID
	tgUser, err := r.userSvc.FindUserByPlatformID(req.Context(), "telegram", tgIDStr)
	if err != nil {
		writeError(w, http.StatusNotFound, "telegram user not found")
		return
	}

	// Find the web user to link to
	webUser, err := r.userSvc.FindUserByPlatformID(req.Context(), "web", body.Email)
	if err != nil {
		writeError(w, http.StatusNotFound, "web user not found")
		return
	}

	if err := r.userSvc.LinkTelegramAccount(req.Context(), webUser.ID, tgUser.ID); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok": true,
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// POST /api/v1/users/link/email
// Body: {"telegram_id": 123456, "email": "user@example.com", "code": "123456"}
// ─────────────────────────────────────────────────────────────────────────────

func (r *Router) handleLinkEmail(w http.ResponseWriter, req *http.Request) {
	var body struct {
		TelegramID int64  `json:"telegram_id"`
		Email      string `json:"email"`
		Code       string `json:"code"`
	}
	if !readBody(w, req, &body) {
		return
	}
	email := normalizeEmail(body.Email)
	if body.TelegramID == 0 || email == "" || body.Code == "" {
		writeError(w, http.StatusBadRequest, "telegram_id, email and code are required")
		return
	}
	if !emailRe.MatchString(email) {
		writeError(w, http.StatusBadRequest, "invalid email address")
		return
	}

	ok, _, err := verifyOTP(email, body.Code)
	if err != nil {
		if errors.Is(err, ErrMaxAttemptsReached) {
			writeError(w, http.StatusForbidden, "too many failed attempts")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !ok {
		writeError(w, http.StatusUnauthorized, "invalid or expired code")
		return
	}

	tgIDStr := strconv.FormatInt(body.TelegramID, 10)
	tgUser, err := r.userSvc.FindUserByPlatformID(req.Context(), "telegram", tgIDStr)
	if err != nil {
		writeError(w, http.StatusNotFound, "telegram user not found")
		return
	}

	if err := r.userSvc.LinkEmailAccount(req.Context(), tgUser.ID, email); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok": true,
	})
}

