package server_test

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"xraytool/internal/database"
)

// Tests for handleCreatePayment
func TestCreatePayment_Comprehensive(t *testing.T) {
	r := newTestRouter(t)

	// Register user
	doAuth(r, "POST", "/api/v1/users/register", `{"telegram_id":555001,"username":"TestPay"}`)

	// 1. Missing telegram_id
	w1 := doAuth(r, "POST", "/api/v1/payments/create", `{"amount":100,"payment_type":"subscription"}`)
	if w1.Code != http.StatusBadRequest {
		t.Errorf("Expected 400 for missing telegram_id, got %d", w1.Code)
	}

	// 2. Missing amount and no plan_id
	w2 := doAuth(r, "POST", "/api/v1/payments/create", `{"telegram_id":555001,"payment_type":"subscription"}`)
	if w2.Code != http.StatusBadRequest {
		t.Errorf("Expected 400 for missing amount, got %d", w2.Code)
	}

	// 3. User not found
	w3 := doAuth(r, "POST", "/api/v1/payments/create", `{"telegram_id":999999,"amount":100,"payment_type":"subscription"}`)
	if w3.Code != http.StatusNotFound {
		t.Errorf("Expected 404 for unknown user, got %d", w3.Code)
	}

	// Create a plan with unique months
	plan := database.Plan{Months: 99, BasePrice: 1000, GlobalDiscountPercent: 10, IsActive: true}
	testDB.Create(&plan)

	// Create a promo code
	promo := database.PromoCode{Code: "TESTPROMO20", DiscountPercent: 20, MaxUses: 1, TargetPlatform: "all", IsActive: true}
	testDB.Create(&promo)

	// 4. Create payment with plan_id (should apply promo over global discount because 20 > 10)
	w4 := doAuth(r, "POST", "/api/v1/payments/create", fmt.Sprintf(`{"telegram_id":555001,"payment_type":"subscription","plan_id":%d,"promo_code":"TESTPROMO20","platform":"all"}`, plan.ID))
	if w4.Code != http.StatusCreated {
		t.Fatalf("Expected 201 for plan+promo, got %d. body: %s", w4.Code, w4.Body.String())
	}

	pid := int(jsonBody(t, w4)["payment_id"].(float64))
	var p database.Payment
	testDB.First(&p, pid)
	if p.Amount != 800 {
		t.Errorf("Expected amount 800 (20%% off 1000), got %d", p.Amount)
	}
	if p.PromoCodeID == nil || *p.PromoCodeID != promo.ID {
		t.Errorf("Expected promo code to be applied")
	}

	// Complete the payment to trigger usage increment
	doAuth(r, "POST", fmt.Sprintf("/api/v1/payments/%d/status", pid), `{"status":"completed","expected_statuses":["pending_card"]}`)

	// 5. Promo code limit reached (max uses was 1)
	w5 := doAuth(r, "POST", "/api/v1/payments/create", `{"telegram_id":555001,"amount":100,"payment_type":"subscription","promo_code":"TESTPROMO20","platform":"all"}`)
	if w5.Code != http.StatusBadRequest {
		t.Errorf("Expected 400 for max uses reached, got %d", w5.Code)
	}
}

func TestValidatePromoCode_Comprehensive(t *testing.T) {
	r := newTestRouter(t)

	// Create promo codes
	activePromo := database.PromoCode{Code: "ACTIVE10", DiscountPercent: 10, TargetPlatform: "android", IsActive: true}
	inactivePromo := database.PromoCode{Code: "INACTIVE", DiscountPercent: 50, TargetPlatform: "all", IsActive: true}
	testDB.Create(&inactivePromo)
	testDB.Model(&inactivePromo).Update("is_active", false)

	now := time.Now()
	expiredTime := now.Add(-1 * time.Hour)
	expiredPromo := database.PromoCode{Code: "EXPIRED", DiscountPercent: 20, TargetPlatform: "all", IsActive: true, ExpiresAt: &expiredTime}

	testDB.Create(&activePromo)
	testDB.Create(&expiredPromo)

	tests := []struct {
		name       string
		query      string
		wantStatus int
	}{
		{"missing code", "?platform=all", http.StatusBadRequest},
		{"missing platform", "?code=ACTIVE10", http.StatusBadRequest},
		{"not found", "?code=UNKNOWN&platform=all", http.StatusNotFound},
		{"inactive", "?code=INACTIVE&platform=all", http.StatusBadRequest},
		{"expired", "?code=EXPIRED&platform=all", http.StatusBadRequest},
		{"wrong platform", "?code=ACTIVE10&platform=ios", http.StatusBadRequest},
		{"valid", "?code=ACTIVE10&platform=android", http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := doAuth(r, "GET", "/api/v1/promocodes/validate"+tt.query, "")
			if w.Code != tt.wantStatus {
				t.Errorf("expected %d, got %d. body: %s", tt.wantStatus, w.Code, w.Body.String())
			}
		})
	}

	// Test already used by user
	doAuth(r, "POST", "/api/v1/users/register", `{"telegram_id":666001,"username":"PromoUser"}`)

	var u database.User
	testDB.Where("username = ?", "PromoUser").First(&u)

	// Create a completed payment with ACTIVE10
	testDB.Create(&database.Payment{
		UserID: u.ID, Amount: 100, Status: "completed", PaymentType: "sub", Method: "test", PromoCodeID: &activePromo.ID,
	})

	w := doAuth(r, "GET", "/api/v1/promocodes/validate?code=ACTIVE10&platform=android&telegram_id=666001", "")
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for already used by user, got %d", w.Code)
	}
}

func TestPlatgaCallback_Comprehensive(t *testing.T) {
	r := newTestRouter(t)

	// Register user
	doAuth(r, "POST", "/api/v1/users/register", `{"telegram_id":777001,"username":"PlatgaUser"}`)
	var u database.User
	testDB.Where("username = ?", "PlatgaUser").First(&u)

	extID := "ext_12345"
	testDB.Create(&database.Payment{
		UserID: u.ID, Amount: 500, Status: "pending", PaymentType: "sub", Method: "platega", ExternalID: &extID,
	})

	// 1. Invalid JSON body
	payloadInvalid := []byte(`{invalid}`)
	mac := hmac.New(sha256.New, []byte("test-platega-secret"))
	mac.Write(payloadInvalid)

	reqInvalid := httptest.NewRequest("POST", "/api/v1/payments/platega/callback", bytes.NewReader(payloadInvalid))
	reqInvalid.Header.Set("X-Platega-Signature", hex.EncodeToString(mac.Sum(nil)))
	wInvalid := httptest.NewRecorder()
	r.ServeHTTP(wInvalid, reqInvalid)
	if wInvalid.Code != http.StatusBadRequest {
		t.Errorf("Expected 400 for invalid JSON, got %d", wInvalid.Code)
	}

	// 2. Successful callback -> auto updates status
	// Note: Platega uses "id" field, not "external_id"
	payloadSuccess := []byte(fmt.Sprintf(`{"id":"%s","status":"success"}`, extID))

	reqSuccess := httptest.NewRequest("POST", "/api/v1/payments/platega/callback", bytes.NewReader(payloadSuccess))
	reqSuccess.Header.Set("Content-Type", "application/json")
	// Platega sends the secret as plain text in X-Secret header
	reqSuccess.Header.Set("X-Secret", "test-platega-secret")

	wSuccess := httptest.NewRecorder()
	r.ServeHTTP(wSuccess, reqSuccess)
	if wSuccess.Code != http.StatusOK {
		t.Errorf("Expected 200 for successful callback, got %d. body: %s", wSuccess.Code, wSuccess.Body.String())
	}

	// Check if payment was updated
	var p database.Payment
	testDB.Where("external_id = ?", extID).First(&p)
	if p.Status != "completed" {
		t.Errorf("Expected payment status to be completed, got %s", p.Status)
	}
}
