package server

// handlers_payment.go — implements all payment-related REST handlers.
//
// Response shapes are designed to exactly match what the Python Telegram bot
// (clients-bot/sqltools.py) expects.

import (
	"crypto/subtle"
	"errors"
	"fmt"
	json "github.com/goccy/go-json"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"xraytool/internal/domain"
	"xraytool/internal/payment"
)

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

// buildPaymentResponse converts a database.Payment into the shape the bot expects:
//
//	{"id":42,"status":"pending_card","amount":159,"payment_type":"...","method":"...","external_id":"...","custom_data":{"telegram_id":123}}
func buildPaymentResponse(p *domain.Payment) map[string]interface{} {
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
		PlanID      *int64 `json:"plan_id"`
		PromoCode   string `json:"promo_code"`
		Platform    string `json:"platform"`
	}
	if !readBody(w, req, &body) {
		return
	}
	reqPayload := payment.CreatePaymentRequest{
		TelegramID:  body.TelegramID,
		Amount:      body.Amount,
		PaymentType: body.PaymentType,
		Method:      body.Method,
		ExternalID:  body.ExternalID,
		PlanID:      body.PlanID,
		PromoCode:   body.PromoCode,
		Platform:    body.Platform,
	}

	pay, err := r.paymentSvc.CreatePayment(req.Context(), reqPayload)
	if err != nil {
		if errors.Is(err, payment.ErrUserNotFound) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		if errors.Is(err, payment.ErrInvalidPlanID) ||
			errors.Is(err, payment.ErrPromoLimitReached) ||
			errors.Is(err, payment.ErrPromoAlreadyUsed) ||
			strings.Contains(err.Error(), "is required") ||
			strings.Contains(err.Error(), "must be positive") {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		r.log.Error("create payment", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to create payment")
		return
	}

	writeJSON(w, http.StatusCreated, map[string]interface{}{"payment_id": pay.ID})
}

// ─────────────────────────────────────────────────────────────────────────────
// GET /api/v1/payments
// Optional query filters: status=..., method=..., payment_type=..., telegram_id=...
// Returns array of payment objects.
// ─────────────────────────────────────────────────────────────────────────────

func (r *Router) handleListPayments(w http.ResponseWriter, req *http.Request) {
	q := req.URL.Query()

	status := q.Get("status")
	method := q.Get("method")
	pt := q.Get("payment_type")
	tgIDStr := q.Get("telegram_id")

	payments, err := r.paymentSvc.FindPaymentsByFilters(req.Context(), status, method, pt, tgIDStr)
	if err != nil {
		r.log.Error("list payments", "err", err)
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

	payment, err := r.paymentSvc.FindPaymentByID(req.Context(), fmt.Sprintf("%d", paymentID))
	if err != nil {
		writeError(w, http.StatusNotFound, "payment not found")
		return
	}

	writeJSON(w, http.StatusOK, buildPaymentResponse(payment))
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

	updated, err := r.paymentSvc.UpdatePaymentStatus(req.Context(), paymentID, body.Status, body.ExpectedStatuses)
	if err != nil {
		r.log.Error("update payment status", "err", err)
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	if !updated {
		writeError(w, http.StatusConflict, "payment not in expected status")
		return
	}

	if body.Status == "completed" {
		payment, err := r.paymentSvc.FindPaymentByID(req.Context(), fmt.Sprintf("%d", paymentID))
		if err == nil && payment != nil {
			updatedSub, err := r.userSvc.GetSubscriptionByUserID(req.Context(), payment.UserID)
			if err == nil && updatedSub != nil {
				r.userSvc.DeleteNotificationsBySubID(req.Context(), updatedSub.ID) //nolint:errcheck
				r.unbanUserInXrayAsync(*updatedSub)
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ─────────────────────────────────────────────────────────────────────────────
// POST /api/v1/payments/platega/callback
// Platega webhook. Signature verification is TODO — for now we accept, parse,
// and forward the event to the dispatcher.
// ─────────────────────────────────────────────────────────────────────────────

func (r *Router) handlePlatgaCallback(w http.ResponseWriter, req *http.Request) {
	rawBody, err := io.ReadAll(http.MaxBytesReader(w, req.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}

	var body map[string]interface{}
	if err := json.Unmarshal(rawBody, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}

	secretHeader := req.Header.Get("X-Secret")
	if secretHeader == "" {
		r.log.Warn("platega callback missing X-Secret header")
		writeError(w, http.StatusUnauthorized, "missing secret")
		return
	}

	if r.cfg.PlategaSecret == "" {
		r.log.Error("platega webhook received but platega_secret is not configured")
		writeError(w, http.StatusServiceUnavailable, "webhook not configured")
		return
	}

	// Platega sends the secret in plain text
	if subtle.ConstantTimeCompare([]byte(secretHeader), []byte(r.cfg.PlategaSecret)) != 1 {
		r.log.Warn("platega callback secret mismatch", "ip", getClientIP(req), "expected_len", len(r.cfg.PlategaSecret))
		writeError(w, http.StatusUnauthorized, "invalid secret")
		return
	}

	// json is already unmarshaled into `body` above

	// Platega sends "id" or "transactionId"
	extID, _ := body["id"].(string)
	if extID == "" {
		extID, _ = body["transactionId"].(string)
	}

	status, _ := body["status"].(string)
	r.log.Info("platega callback received", "external_id", extID, "status", status)

	// Always dispatch the raw Platega callback event
	r.dispatcher.Dispatch("platega.callback", body, nil)

	if extID != "" && status != "" {
		if err := r.paymentSvc.ProcessExternalPaymentStatus(req.Context(), extID, status); err != nil {
			r.log.Error("failed to process external payment status", "err", err, "extID", extID)
			// Don't fail the webhook — Platega expects 200 OK regardless.
		}
		// Only unban if the payment was actually completed successfully.
		if status == "success" || status == "SUCCESS" || status == "CONFIRMED" || status == "COMPLETED" || status == "completed" {
			payment, err := r.paymentSvc.FindPaymentByExternalID(req.Context(), extID)
			if err == nil && payment != nil && payment.Status == "completed" {
				updatedSub, err := r.userSvc.GetSubscriptionByUserID(req.Context(), payment.UserID)
				if err == nil && updatedSub != nil {
					r.userSvc.DeleteNotificationsBySubID(req.Context(), updatedSub.ID) //nolint:errcheck
					r.unbanUserInXrayAsync(*updatedSub)
				}
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// GET /api/v1/admin/payments/stats
func (r *Router) handleAdminPaymentsStats(w http.ResponseWriter, req *http.Request) {
	payments, err := r.paymentSvc.FindAll(req.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}

	type MonthStat struct {
		Month          string `json:"month"`
		TotalRevenue   int    `json:"total_revenue"`
		CompletedCount int    `json:"completed_count"`
		TotalCount     int    `json:"total_count"`
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
