package server

// handlers_payment.go — implements all payment-related REST handlers.
//
// Response shapes are designed to exactly match what the Python Telegram bot
// (clients-bot/sqltools.py) expects.

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strconv"
	"time"

	"gorm.io/gorm"

	"xraytool/internal/database"
)

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

// buildPaymentResponse converts a database.Payment into the shape the bot expects:
//
//	{"id":42,"status":"pending_card","amount":159,"payment_type":"...","method":"...","external_id":"...","custom_data":{"telegram_id":123}}
func buildPaymentResponse(p *database.Payment) map[string]interface{} {
	extID := ""
	if p.ExternalID != nil {
		extID = *p.ExternalID
	}
	return map[string]interface{}{
		"id":           p.ID,
		"status":       p.Status,
		"amount":       p.Amount,
		"payment_type": p.PaymentType,
		"method":       p.Method,
		"external_id":  extID,
		"custom_data":  p.CustomData,
		"created_at":   p.CreatedAt.UTC().Format(time.RFC3339),
		"user_id":      p.UserID,
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// POST /api/v1/payments/create
// Body: {"telegram_id":123,"amount":159,"payment_type":"subscription","method":"platega"}
// Returns: {"payment_id":42}
// ─────────────────────────────────────────────────────────────────────────────

func (r *Router) handleCreatePayment(w http.ResponseWriter, req *http.Request) {
	var body struct {
		TelegramID  int64  `json:"telegram_id"`
		Amount      int    `json:"amount"`
		PaymentType string `json:"payment_type"`
		Method      string `json:"method"`
		ExternalID  string `json:"external_id"`
	}
	if !readBody(w, req, &body) {
		return
	}
	if body.TelegramID == 0 {
		writeError(w, http.StatusBadRequest, "telegram_id is required")
		return
	}
	if body.Amount <= 0 {
		writeError(w, http.StatusBadRequest, "amount must be positive")
		return
	}
	if body.PaymentType == "" {
		writeError(w, http.StatusBadRequest, "payment_type is required")
		return
	}

	db := database.DB()

	// Find user by telegram_id.
	user, err := findUserByTelegramID(db, body.TelegramID)
	if err != nil {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}

	// Ensure ExternalID uniqueness: if non-empty store it; otherwise leave nil
	// so the NULL-allows-duplicates unique index is satisfied without collisions.
	var externalIDPtr *string
	if body.ExternalID != "" {
		externalIDPtr = &body.ExternalID
	}

	payment := database.Payment{
		UserID:      user.ID,
		Amount:      body.Amount,
		Status:      "pending_card",
		PaymentType: body.PaymentType,
		Method:      body.Method,
		ExternalID:  externalIDPtr,
		CustomData: database.Metadata{
			"telegram_id": body.TelegramID,
		},
	}

	if result := db.Create(&payment); result.Error != nil {
		r.log.Error("create payment", "err", result.Error)
		writeError(w, http.StatusInternalServerError, "failed to create payment")
		return
	}

	writeJSON(w, http.StatusCreated, map[string]interface{}{"payment_id": payment.ID})
}

// ─────────────────────────────────────────────────────────────────────────────
// GET /api/v1/payments
// Optional query filters: status=..., method=..., payment_type=..., telegram_id=...
// Returns array of payment objects.
// ─────────────────────────────────────────────────────────────────────────────

func (r *Router) handleListPayments(w http.ResponseWriter, req *http.Request) {
	db := database.DB()
	query := db.Model(&database.Payment{})

	q := req.URL.Query()

	// Filter by status.
	if status := q.Get("status"); status != "" {
		query = query.Where("status = ?", status)
	}
	// Filter by method.
	if method := q.Get("method"); method != "" {
		query = query.Where("method = ?", method)
	}
	// Filter by payment_type.
	if pt := q.Get("payment_type"); pt != "" {
		query = query.Where("payment_type = ?", pt)
	}
	// Filter by telegram_id (via user lookup).
	if tgIDStr := q.Get("telegram_id"); tgIDStr != "" {
		tgID, err := strconv.ParseInt(tgIDStr, 10, 64)
		if err == nil {
			user, err := findUserByTelegramID(db, tgID)
			if err == nil {
				query = query.Where("user_id = ?", user.ID)
			} else {
				// Unknown user → return empty list.
				writeJSON(w, http.StatusOK, []interface{}{})
				return
			}
		}
	}

	var payments []database.Payment
	if result := query.Order("id DESC").Find(&payments); result.Error != nil {
		r.log.Error("list payments", "err", result.Error)
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}

	out := make([]map[string]interface{}, 0, len(payments))
	for i := range payments {
		out = append(out, buildPaymentResponse(&payments[i]))
	}
	writeJSON(w, http.StatusOK, out)
}

// ─────────────────────────────────────────────────────────────────────────────
// GET /api/v1/payments/{id}
// Returns a single payment object.
// ─────────────────────────────────────────────────────────────────────────────

func (r *Router) handleGetPayment(w http.ResponseWriter, req *http.Request) {
	idStr := req.PathValue("id")
	paymentID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || paymentID <= 0 {
		writeError(w, http.StatusBadRequest, "invalid payment id")
		return
	}

	db := database.DB()
	var payment database.Payment
	if result := db.First(&payment, paymentID); result.Error != nil {
		writeError(w, http.StatusNotFound, "payment not found")
		return
	}

	writeJSON(w, http.StatusOK, buildPaymentResponse(&payment))
}

// ─────────────────────────────────────────────────────────────────────────────
// POST /api/v1/payments/{id}/status
// Body: {"status":"completed","expected_statuses":["pending_card"]}
// Returns {"ok":true} or 409 if the payment is not in expected status.
// ─────────────────────────────────────────────────────────────────────────────

func (r *Router) handleUpdatePaymentStatus(w http.ResponseWriter, req *http.Request) {
	idStr := req.PathValue("id")
	paymentID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || paymentID <= 0 {
		writeError(w, http.StatusBadRequest, "invalid payment id")
		return
	}

	var body struct {
		Status           string   `json:"status"`
		ExpectedStatuses []string `json:"expected_statuses"`
	}
	if !readBody(w, req, &body) {
		return
	}
	if body.Status == "" {
		writeError(w, http.StatusBadRequest, "status is required")
		return
	}

	db := database.DB()

	// Atomic conditional update: only succeeds if current status is in expectedStatuses.
	query := db.Model(&database.Payment{}).Where("id = ?", paymentID)
	if len(body.ExpectedStatuses) > 0 {
		query = query.Where("status IN ?", body.ExpectedStatuses)
	}
	result := query.Update("status", body.Status)
	if result.Error != nil {
		r.log.Error("update payment status", "err", result.Error)
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	if result.RowsAffected == 0 {
		writeError(w, http.StatusConflict, "payment not in expected status")
		return
	}

	// Dispatch event for completed payments so webhooks (e.g. the Python bot) are notified.
	if body.Status == "completed" {
		var payment database.Payment
		if db.First(&payment, paymentID).Error == nil {
			r.dispatcher.Dispatch("payment.completed", map[string]interface{}{
				"payment_id":   payment.ID,
				"amount":       payment.Amount,
				"payment_type": payment.PaymentType,
				"method":       payment.Method,
				"user_id":      payment.UserID,
			}, nil)

			// Apply referral reward if applicable (runs in background goroutine to
			// not delay the HTTP response).
			go r.applyReferralRewardForPayment(db, &payment)
		}
	}

	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// applyReferralRewardForPayment credits the referrer of the payer with 25% of
// the payment amount, and records a ReferralReward row. No-op if the user has
// no referrer.
func (r *Router) applyReferralRewardForPayment(db *gorm.DB, payment *database.Payment) {
	var user database.User
	if db.First(&user, "id = ?", payment.UserID).Error != nil {
		return
	}
	if user.ReferredBy == nil || *user.ReferredBy == "" {
		return
	}

	const referralPercent = 0.25
	reward := int(float64(payment.Amount) * referralPercent)
	if reward <= 0 {
		return
	}

	referrerID := *user.ReferredBy

	// Credit the referrer's balance atomically and record the reward inside a transaction.
	txErr := db.Transaction(func(tx *gorm.DB) error {
		var count int64
		if err := tx.Model(&database.ReferralReward{}).Where("payment_id = ?", payment.ID).Count(&count).Error; err != nil {
			return err
		}
		if count > 0 {
			// Reward already processed for this payment.
			return nil
		}

		if result := tx.Model(&database.User{}).
			Where("id = ?", referrerID).
			Update("balance", gorm.Expr("balance + ?", reward)); result.Error != nil {
			return result.Error
		}

		rewardRow := database.ReferralReward{
			ReferrerID: referrerID,
			ReferredID: user.ID,
			PaymentID:  payment.ID,
			Amount:     reward,
		}
		return tx.Create(&rewardRow).Error
	})

	if txErr != nil {
		r.log.Error("referral reward transaction failed", "err", txErr)
		return
	}

	var referrer database.User
	if err := db.First(&referrer, "id = ?", referrerID).Error; err == nil {
		if tgIDRaw, ok := referrer.Metadata["telegram_id"]; ok {
			r.dispatcher.Dispatch("referral.reward", map[string]interface{}{
				"telegram_id":       tgIDRaw,
				"reward_amount":     reward,
				"referred_username": user.Username,
			}, nil)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// POST /api/v1/payments/platega/callback
// Platega webhook. Signature verification is TODO — for now we accept, parse,
// and forward the event to the dispatcher.
// ─────────────────────────────────────────────────────────────────────────────

func (r *Router) handlePlatgaCallback(w http.ResponseWriter, req *http.Request) {
	signature := req.Header.Get("X-Platega-Signature")
	if signature == "" {
		writeError(w, http.StatusUnauthorized, "missing signature")
		return
	}
	rawBody, err := io.ReadAll(http.MaxBytesReader(w, req.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}

	if r.cfg.PlategaSecret == "" {
		r.log.Error("platega webhook received but platega_secret is not configured")
		writeError(w, http.StatusServiceUnavailable, "webhook not configured")
		return
	}

	mac := hmac.New(sha256.New, []byte(r.cfg.PlategaSecret))
	mac.Write(rawBody)
	expectedMAC := hex.EncodeToString(mac.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(signature), []byte(expectedMAC)) != 1 {
		r.log.Warn("platega callback signature mismatch", "ip", getClientIP(req))
		writeError(w, http.StatusUnauthorized, "invalid signature")
		return
	}

	var body map[string]interface{}
	if err := json.Unmarshal(rawBody, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}

	extID, _ := body["external_id"].(string)
	status, _ := body["status"].(string)
	r.log.Info("platega callback received", "external_id", extID, "status", status)

	// Always dispatch the raw Platega callback event
	r.dispatcher.Dispatch("platega.callback", body, nil)

	// Automatically update the payment status if external_id and status are present
	if extID != "" && status != "" {
		mappedStatus := status
		if status == "success" {
			mappedStatus = "completed"
		}

		db := database.DB()
		var payment database.Payment
		if err := db.Where("external_id = ?", extID).First(&payment).Error; err == nil {
			if payment.Status != mappedStatus && payment.Status != "completed" {
				db.Model(&payment).Update("status", mappedStatus)
				r.log.Info("auto-updated payment status", "payment_id", payment.ID, "status", mappedStatus)

				if mappedStatus == "completed" {
					// Dispatch payment.completed so external systems can process the renewal/balance update
					r.dispatcher.Dispatch("payment.completed", map[string]interface{}{
						"payment_id":   payment.ID,
						"amount":       payment.Amount,
						"payment_type": payment.PaymentType,
						"method":       payment.Method,
						"user_id":      payment.UserID,
					}, nil)

					// Apply referral logic
					go r.applyReferralRewardForPayment(db, &payment)
				}
			}
		} else {
			r.log.Warn("platega callback: payment not found by external_id", "external_id", extID)
		}
	}

	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// GET /api/v1/admin/payments/stats
func (r *Router) handleAdminPaymentsStats(w http.ResponseWriter, req *http.Request) {
	db := database.DB()
	var payments []database.Payment
	if err := db.Select("amount", "status", "created_at").Find(&payments).Error; err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}

	type MonthStat struct {
		Month             string `json:"month"`
		TotalRevenue      int    `json:"total_revenue"`
		CompletedCount    int    `json:"completed_count"`
		TotalCount        int    `json:"total_count"`
	}

	statsMap := make(map[string]*MonthStat)
	for i := range payments {
		p := &payments[i]
		m := p.CreatedAt.UTC().Format("2006-01")
		stat, ok := statsMap[m]
		if !ok {
			stat = &MonthStat{Month: m}
			statsMap[m] = stat
		}
		stat.TotalCount++
		if p.Status == "completed" {
			stat.CompletedCount++
			stat.TotalRevenue += p.Amount
		}
	}

	var results []MonthStat
	for _, v := range statsMap {
		results = append(results, *v)
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].Month > results[j].Month
	})

	writeJSON(w, http.StatusOK, results)
}
