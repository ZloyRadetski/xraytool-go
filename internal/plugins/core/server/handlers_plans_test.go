package server_test

import (
	"fmt"
	json "github.com/goccy/go-json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"xraytool/internal/database"
)

func TestGetPlans(t *testing.T) {
	r := newTestRouter(t)

	// Add test plans

	testDB.Exec("DELETE FROM plans")
	testDB.Create(&database.Plan{Months: 1, BasePrice: 100, GlobalDiscountPercent: 10, IsActive: true})
	testDB.Create(&database.Plan{Months: 3, BasePrice: 200, GlobalDiscountPercent: 0, IsActive: true})
	testDB.Exec("INSERT INTO plans (months, base_price, is_active) VALUES (6, 300, 0)")

	req := httptest.NewRequest("GET", "/api/v1/plans", nil)
	req.Header.Set("X-API-Key", "test-api-key")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}

	if len(resp) != 2 {
		t.Fatalf("expected 2 active plans, got %d", len(resp))
	}

	// 1 month plan with 10% discount -> 90
	if resp[0]["months"].(float64) == 1 {
		if resp[0]["final_price"].(float64) != 90 {
			t.Errorf("expected final price 90, got %v", resp[0]["final_price"])
		}
	}
}

func TestValidatePromoCode(t *testing.T) {
	r := newTestRouter(t)

	testDB.Exec("DELETE FROM promo_codes")

	now := time.Now()
	expired := now.Add(-1 * time.Hour)

	testDB.Create(&database.PromoCode{Code: "VALID", DiscountPercent: 20, TargetPlatform: "all", IsActive: true})
	testDB.Exec("INSERT INTO promo_codes (code, discount_percent, is_active, target_platform) VALUES ('INACTIVE', 20, 0, 'all')")
	testDB.Create(&database.PromoCode{Code: "EXPIRED", DiscountPercent: 20, ExpiresAt: &expired, IsActive: true})
	testDB.Create(&database.PromoCode{Code: "BOTONLY", DiscountPercent: 20, TargetPlatform: "bot", IsActive: true})

	tests := []struct {
		name       string
		query      string
		wantStatus int
		wantValid  bool
	}{
		{"ValidCode", "?code=VALID&platform=bot", 200, true},
		{"ValidCodePlatformAll", "?code=VALID&platform=web", 200, true},
		{"InactiveCode", "?code=INACTIVE&platform=bot", 400, false},
		{"ExpiredCode", "?code=EXPIRED&platform=bot", 400, false},
		{"BotOnlyCodeWrongPlatform", "?code=BOTONLY&platform=web", 400, false},
		{"BotOnlyCodeRightPlatform", "?code=BOTONLY&platform=bot", 200, true},
		{"MissingCode", "?platform=bot", 400, false},
		{"MissingPlatform", "?code=VALID", 400, false},
		{"NotFound", "?code=NOTFOUND&platform=bot", 404, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/api/v1/promocodes/validate"+tt.query, nil)
			req.Header.Set("X-API-Key", "test-api-key")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != tt.wantStatus {
				t.Fatalf("expected %d, got %d. Body: %s", tt.wantStatus, w.Code, w.Body.String())
			}
			if tt.wantStatus == 200 {
				var resp map[string]interface{}
				json.Unmarshal(w.Body.Bytes(), &resp) //nolint:errcheck
				if resp["valid"] != tt.wantValid {
					t.Errorf("expected valid=%v, got %v", tt.wantValid, resp["valid"])
				}
			}
		})
	}
}

func TestValidatePromoCode_Limits(t *testing.T) {
	r := newTestRouter(t)

	testDB.Exec("DELETE FROM promo_codes")
	testDB.Exec("DELETE FROM payments")

	promo := database.PromoCode{Code: "LIMIT", DiscountPercent: 50, MaxUses: 1, UsesCount: 1, IsActive: true}
	testDB.Create(&promo)

	// Use the promo code once
	testDB.Create(&database.Payment{
		UserID: "u1", Amount: 100, Status: "completed", PaymentType: "sub", PromoCodeID: &promo.ID,
	})

	req := httptest.NewRequest("GET", "/api/v1/promocodes/validate?code=LIMIT&platform=bot", nil)
	req.Header.Set("X-API-Key", "test-api-key")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "usage limit reached") {
		t.Errorf("expected usage limit message, got %s", w.Body.String())
	}
}

func TestCreatePaymentWithPlan(t *testing.T) {
	r := newTestRouter(t)

	testDB.Exec("DELETE FROM plans")
	testDB.Exec("DELETE FROM promo_codes")
	testDB.Exec("DELETE FROM users")
	testDB.Exec("DELETE FROM payments")

	testDB.Create(&database.User{ID: "testu", Username: "tu", Metadata: database.Metadata{"telegram_id": float64(111)}})

	plan := database.Plan{Months: 1, BasePrice: 1000, GlobalDiscountPercent: 10, IsActive: true} // global price = 900
	testDB.Create(&plan)

	promo := database.PromoCode{Code: "PROMO20", DiscountPercent: 20, TargetPlatform: "bot", IsActive: true} // promo price = 800
	testDB.Create(&promo)

	// Valid promo, price should be 800
	body := `{"telegram_id":111, "payment_type":"sub", "method":"card", "plan_id":` + fmt.Sprint(plan.ID) + `, "promo_code":"PROMO20", "platform":"bot"}`
	req := httptest.NewRequest("POST", "/api/v1/payments/create", strings.NewReader(body))
	req.Header.Set("X-API-Key", "test-api-key")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp) //nolint:errcheck
	paymentID := int64(resp["payment_id"].(float64))
	var p database.Payment
	testDB.First(&p, paymentID)
	if p.Amount != 800 {
		t.Errorf("expected 800 (best price), got %v", p.Amount)
	}

	// Create another promo that gives less discount (5%), so global is better
	promoBad := database.PromoCode{Code: "PROMO5", DiscountPercent: 5, TargetPlatform: "bot", IsActive: true} // promo price = 950
	testDB.Create(&promoBad)

	// Clear previous payments to avoid ErrAlreadyPendingPayment
	testDB.Exec("DELETE FROM payments")

	body2 := `{"telegram_id":111, "payment_type":"sub", "method":"card", "plan_id":` + fmt.Sprint(plan.ID) + `, "promo_code":"PROMO5", "platform":"bot"}`
	req2 := httptest.NewRequest("POST", "/api/v1/payments/create", strings.NewReader(body2))
	req2.Header.Set("X-API-Key", "test-api-key")
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)

	if w2.Code != http.StatusCreated {
		t.Fatalf("expected 200, got %d", w2.Code)
	}

	var resp2 map[string]interface{}
	json.Unmarshal(w2.Body.Bytes(), &resp2) //nolint:errcheck
	paymentID2 := int64(resp2["payment_id"].(float64))
	var p2 database.Payment
	testDB.First(&p2, paymentID2)
	if p2.Amount != 900 {
		t.Errorf("expected 900 (global discount is better), got %v", p2.Amount)
	}
}
