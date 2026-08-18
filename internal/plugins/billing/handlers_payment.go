package billing

// handlers_payment.go — implements all payment-related REST handlers.
//
// Response shapes are designed to exactly match what the Python Telegram bot
// (clients-bot/sqltools.py) expects.

import (
	"context"
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
	"xraytool/internal/pluginapi"
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

func (p *Plugin) handleCreatePayment(w http.ResponseWriter, req *http.Request) {
	var body struct {
		TelegramID  int64                  `json:"telegram_id"`
		Email       string                 `json:"email"`
		Amount      int                    `json:"amount"`
		PaymentType string                 `json:"payment_type"`
		Method      string                 `json:"method"`
		ExternalID  string                 `json:"external_id"`
		PlanID      *int64                 `json:"plan_id"`
		MaxDevices  int                    `json:"max_devices"`
		PromoCode   string                 `json:"promo_code"`
		Platform    string                 `json:"platform"`
		SuccessURL  string                 `json:"success_url"`
		FailedURL   string                 `json:"failed_url"`
		CustomData  map[string]interface{} `json:"custom_data"`
	}
	if !readBody(w, req, &body) {
		return
	}
	reqPayload := CreatePaymentRequest{
		TelegramID:  body.TelegramID,
		Email:       body.Email,
		Amount:      body.Amount,
		PaymentType: body.PaymentType,
		Method:      body.Method,
		ExternalID:  body.ExternalID,
		PlanID:      body.PlanID,
		MaxDevices:  body.MaxDevices,
		PromoCode:   body.PromoCode,
		Platform:    body.Platform,
		CustomData:  body.CustomData,
	}

	pay, err := p.paymentSvc.CreatePayment(req.Context(), reqPayload)
	if err != nil {
		if errors.Is(err, ErrUserNotFound) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		if errors.Is(err, ErrInvalidPlanID) ||
			errors.Is(err, ErrPromoLimitReached) ||
			errors.Is(err, ErrPromoAlreadyUsed) ||
			strings.Contains(err.Error(), "is required") ||
			strings.Contains(err.Error(), "must be positive") {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		p.log.Error("create payment", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to create payment")
		return
	}

	if provider, ok := p.paymentProvider(body.Method); ok {
		paymentURL, err := p.createProviderPayment(req.Context(), provider, pay, body.TelegramID, body.Email, body.PlanID, body.SuccessURL, body.FailedURL)
		if err != nil {
			p.log.Error("payment provider CreateIntent failed", "method", body.Method, "payment_id", pay.ID, "err", err)
			writeError(w, http.StatusBadGateway, fmt.Sprintf("failed to initiate payment: %v", err))
			return
		}
		writeJSON(w, http.StatusCreated, map[string]interface{}{
			"payment_id":  pay.ID,
			"payment_url": paymentURL,
		})
		return
	}

	// A payment method without a loaded provider remains valid for manual and
	// offline workflows. Gateway-backed methods are routed exclusively through
	// PaymentProvider above; the HTTP layer deliberately has no concrete gateway
	// imports or credential handling.
	writeJSON(w, http.StatusCreated, map[string]interface{}{"payment_id": pay.ID})
}

func (p *Plugin) createProviderPayment(
	ctx context.Context,
	provider pluginapi.PaymentProvider,
	pay *domain.Payment,
	telegramID int64,
	email string,
	planID *int64,
	successURL string,
	failedURL string,
) (string, error) {
	if provider == nil || pay == nil {
		return "", errors.New("payment provider or payment is nil")
	}

	userID := strings.TrimSpace(email)
	if telegramID != 0 {
		userID = strconv.FormatInt(telegramID, 10)
	}
	if userID == "" {
		return "", errors.New("payment user identifier is empty")
	}

	description := "TorvaldsVPN balance top-up"
	if planID != nil {
		description = fmt.Sprintf("TorvaldsVPN subscription (plan ID: %d)", *planID)
	}
	if successURL == "" {
		successURL = "https://t.me/TorvaldsVPNBot"
	}
	if failedURL == "" {
		failedURL = successURL
	}

	result, err := provider.CreateIntent(ctx, pluginapi.PaymentIntentRequest{
		UserID:      userID,
		Amount:      pay.Amount,
		Currency:    "RUB",
		Description: description,
		ExternalRef: fmt.Sprintf("payment-%d", pay.ID),
		CustomData: map[string]any{
			"payment_id": pay.ID,
			"return_url": successURL,
			"failed_url": failedURL,
		},
	})
	if err != nil {
		return "", err
	}
	if result == nil || strings.TrimSpace(result.ExternalID) == "" || strings.TrimSpace(result.PaymentURL) == "" {
		return "", errors.New("payment provider returned an empty transaction ID or payment URL")
	}
	if err := p.paymentSvc.UpdateExternalID(ctx, pay.ID, result.ExternalID); err != nil {
		return "", fmt.Errorf("save provider transaction reference: %w", err)
	}
	pay.ExternalID = &result.ExternalID
	return result.PaymentURL, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// GET /api/v1/payments
// Optional query filters: status=..., method=..., payment_type=..., telegram_id=...
// Returns array of payment objects.
// ─────────────────────────────────────────────────────────────────────────────

func (p *Plugin) handleListPayments(w http.ResponseWriter, req *http.Request) {
	q := req.URL.Query()

	status := q.Get("status")
	method := q.Get("method")
	pt := q.Get("payment_type")
	tgIDStr := q.Get("telegram_id")

	payments, err := p.paymentSvc.FindPaymentsByFilters(req.Context(), status, method, pt, tgIDStr)
	if err != nil {
		p.log.Error("list payments", "err", err)
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

func (p *Plugin) handleGetPayment(w http.ResponseWriter, req *http.Request) {
	idStr := req.PathValue("id")
	paymentID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || paymentID <= 0 {
		writeError(w, http.StatusBadRequest, "invalid payment id")
		return
	}

	payment, err := p.paymentSvc.FindPaymentByID(req.Context(), fmt.Sprintf("%d", paymentID))
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

func (p *Plugin) handleUpdatePaymentStatus(w http.ResponseWriter, req *http.Request) {
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

	updated, err := p.paymentSvc.UpdatePaymentStatus(req.Context(), paymentID, body.Status, body.ExpectedStatuses)
	if err != nil {
		p.log.Error("update payment status", "err", err)
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	if !updated {
		writeError(w, http.StatusConflict, "payment not in expected status")
		return
	}

	if body.Status == "completed" {
		payment, err := p.paymentSvc.FindPaymentByID(req.Context(), fmt.Sprintf("%d", paymentID))
		if err == nil && payment != nil {
			updatedSub, err := p.userSvc.GetSubscriptionByUserID(req.Context(), payment.UserID)
			if err == nil && updatedSub != nil {
				p.userSvc.DeleteNotificationsBySubID(req.Context(), updatedSub.ID) //nolint:errcheck
				p.unbanUserInXrayAsync(*updatedSub)
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

// handlePaymentCallback delegates gateway-specific authentication and parsing
// to the selected PaymentProvider, then applies the provider-neutral payment
// lifecycle owned by core.
func (p *Plugin) handlePaymentCallback(w http.ResponseWriter, req *http.Request) {
	method := strings.ToLower(strings.TrimSpace(req.PathValue("method")))
	provider, ok := p.paymentProvider(method)
	if !ok {
		writeError(w, http.StatusNotFound, "payment provider not found")
		return
	}

	result, err := provider.VerifyCallback(req.Context(), req)
	if err != nil {
		status := http.StatusBadRequest
		message := "invalid payment callback"
		lowerErr := strings.ToLower(err.Error())
		switch {
		case strings.Contains(lowerErr, "not initialized"), strings.Contains(lowerErr, "not configured"):
			status = http.StatusServiceUnavailable
			message = "payment provider unavailable"
		case strings.Contains(lowerErr, "secret"), strings.Contains(lowerErr, "signature"), strings.Contains(lowerErr, "auth"):
			status = http.StatusUnauthorized
			message = "invalid callback credentials"
		}
		p.log.Warn("payment callback rejected", "method", method, "err", err, "ip", getClientIP(req))
		writeError(w, status, message)
		return
	}
	if result == nil || strings.TrimSpace(result.ExternalID) == "" || strings.TrimSpace(result.Status) == "" {
		writeError(w, http.StatusBadRequest, "payment callback result is incomplete")
		return
	}

	data := result.CustomData
	if data == nil {
		data = make(map[string]any)
	}
	data["external_id"] = result.ExternalID
	data["status"] = result.Status
	if result.Amount != 0 {
		data["amount"] = result.Amount
	}
	if result.Currency != "" {
		data["currency"] = result.Currency
	}
	p.dispatcher.Dispatch(method+".callback", data, nil)

	if err := p.paymentSvc.ProcessExternalPaymentStatus(req.Context(), result.ExternalID, result.Status); err != nil {
		// The callback is authenticated, so retain the previous idempotent
		// webhook response behaviour even if local persistence failed.
		p.log.Error("failed to process external payment status", "method", method, "external_id", result.ExternalID, "err", err)
	} else if result.Status == "completed" {
		p.afterCompletedExternalPayment(req.Context(), result.ExternalID)
	}

	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (p *Plugin) afterCompletedExternalPayment(ctx context.Context, externalID string) {
	payment, err := p.paymentSvc.FindPaymentByExternalID(ctx, externalID)
	if err != nil || payment == nil || payment.Status != "completed" {
		return
	}
	updatedSub, err := p.userSvc.GetSubscriptionByUserID(ctx, payment.UserID)
	if err != nil || updatedSub == nil {
		return
	}
	p.userSvc.DeleteNotificationsBySubID(ctx, updatedSub.ID) //nolint:errcheck
	p.unbanUserInXrayAsync(*updatedSub)
}

// handlePlatgaCallback remains as a source-compatible private bridge for
// callers compiled against the pre-plugin route. Registered routes use
// handlePaymentCallback above.
func (p *Plugin) handlePlatgaCallback(w http.ResponseWriter, req *http.Request) {
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
		p.log.Warn("platega callback missing X-Secret header")
		writeError(w, http.StatusUnauthorized, "missing secret")
		return
	}

	if p.cfg.PlategaSecret == "" {
		p.log.Error("platega webhook received but platega_secret is not configured")
		writeError(w, http.StatusServiceUnavailable, "webhook not configured")
		return
	}

	// Platega sends the secret in plain text
	if subtle.ConstantTimeCompare([]byte(secretHeader), []byte(p.cfg.PlategaSecret)) != 1 {
		p.log.Warn("platega callback secret mismatch", "ip", getClientIP(req), "expected_len", len(p.cfg.PlategaSecret))
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
	p.log.Info("platega callback received", "external_id", extID, "status", status)

	// Always dispatch the raw Platega callback event
	p.dispatcher.Dispatch("platega.callback", body, nil)

	if extID != "" && status != "" {
		if err := p.paymentSvc.ProcessExternalPaymentStatus(req.Context(), extID, status); err != nil {
			p.log.Error("failed to process external payment status", "err", err, "extID", extID)
			// Don't fail the webhook — Platega expects 200 OK regardless.
		}
		// Only unban if the payment was actually completed successfully.
		if status == "success" || status == "SUCCESS" || status == "CONFIRMED" || status == "COMPLETED" || status == "completed" {
			payment, err := p.paymentSvc.FindPaymentByExternalID(req.Context(), extID)
			if err == nil && payment != nil && payment.Status == "completed" {
				updatedSub, err := p.userSvc.GetSubscriptionByUserID(req.Context(), payment.UserID)
				if err == nil && updatedSub != nil {
					p.userSvc.DeleteNotificationsBySubID(req.Context(), updatedSub.ID) //nolint:errcheck
					p.unbanUserInXrayAsync(*updatedSub)
				}
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// GET /api/v1/admin/payments/stats
func (p *Plugin) handleAdminPaymentsStats(w http.ResponseWriter, req *http.Request) {
	payments, err := p.paymentSvc.FindAll(req.Context())
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
