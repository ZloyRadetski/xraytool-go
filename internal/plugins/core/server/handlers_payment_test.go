package server_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"xraytool/internal/database"
)

func TestCreatePayment_Success(t *testing.T) {
	r := newTestRouter(t)
	doAuth(r, "POST", "/api/v1/users/register", `{"telegram_id":2001,"username":"J"}`)

	w := doAuth(r, "POST", "/api/v1/payments/create", `{"telegram_id":2001,"amount":159,"payment_type":"subscription","method":"platega"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d", w.Code)
	}

	pid := int(jsonBody(t, w)["payment_id"].(float64))
	if pid <= 0 {
		t.Errorf("expected valid payment_id")
	}
}

func TestCreatePayment_WebEmail_Success(t *testing.T) {
	r := newTestRouter(t)
	// 1. Request code for email (which automatically registers the web user)
	doAuth(r, "POST", "/api/v1/users/request_code", `{"platform":"web","email":"guest_user@example.com"}`)

	// 2. Create payment using email instead of telegram_id
	w := doAuth(r, "POST", "/api/v1/payments/create", `{"email":"guest_user@example.com","amount":159,"payment_type":"subscription","method":"platega"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d. body: %s", w.Code, w.Body.String())
	}

	pid := int(jsonBody(t, w)["payment_id"].(float64))
	if pid <= 0 {
		t.Errorf("expected valid payment_id")
	}
}

func TestCreatePayment_MissingTelegramIdAndEmail(t *testing.T) {
	r := newTestRouter(t)
	w := doAuth(r, "POST", "/api/v1/payments/create", `{"amount":159,"payment_type":"subscription","method":"platega"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request, got %d", w.Code)
	}
}


func TestUpdatePaymentStatus_Atomic_Success(t *testing.T) {
	r := newTestRouter(t)
	doAuth(r, "POST", "/api/v1/users/register", `{"telegram_id":2002,"username":"K"}`)
	w := doAuth(r, "POST", "/api/v1/payments/create", `{"telegram_id":2002,"amount":159,"payment_type":"subscription","method":"platega"}`)

	pid := int(jsonBody(t, w)["payment_id"].(float64))

	wUpdate := doAuth(r, "POST", fmt.Sprintf("/api/v1/payments/%d/status", pid), `{"status":"completed","expected_statuses":["pending_card"]}`)
	if wUpdate.Code != http.StatusOK {
		t.Fatalf("expected 200 on first update, got %d. body: %s", wUpdate.Code, wUpdate.Body.String())
	}

	wConflict := doAuth(r, "POST", fmt.Sprintf("/api/v1/payments/%d/status", pid), `{"status":"completed","expected_statuses":["pending_card"]}`)
	if wConflict.Code != http.StatusConflict {
		t.Fatalf("expected 409 on second update, got %d", wConflict.Code)
	}
}

func TestPlategaWebhook_MissingSignature(t *testing.T) {
	r := newTestRouter(t)
	// Do not use doAuth because this endpoint doesn't need X-API-Key, but we can use normal do
	w := do(r, "POST", "/api/v1/payments/platega/callback", `{"external_id":"123","status":"completed"}`, "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for missing signature, got %d", w.Code)
	}
}

func TestPlategaWebhook_InvalidSignature(t *testing.T) {
	r := newTestRouter(t)
	req := httptest.NewRequest("POST", "/api/v1/payments/platega/callback", bytes.NewReader([]byte(`{"external_id":"123","status":"completed"}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Platega-Signature", "invalid_hex")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for invalid signature, got %d", w.Code)
	}
}

func TestPlategaWebhook_Success(t *testing.T) {
	r := newTestRouter(t)

	payload := []byte(`{"external_id":"pay_123","status":"completed"}`)

	req := httptest.NewRequest("POST", "/api/v1/payments/platega/callback", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	// Platega sends the secret as plain text in X-Secret header
	req.Header.Set("X-Secret", "test-platega-secret")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d. body: %s", w.Code, w.Body.String())
	}
}

func TestUpdatePaymentStatus_ExtendsSubscription(t *testing.T) {
	r := newTestRouter(t)

	// Clean tables
	testDB.Exec("DELETE FROM plans")
	testDB.Exec("DELETE FROM users")
	testDB.Exec("DELETE FROM payments")
	testDB.Exec("DELETE FROM subscriptions")

	// 1. Create a user and an active plan (e.g. 3 months)
	plan := database.Plan{Months: 3, BasePrice: 300, IsActive: true}
	testDB.Create(&plan)

	// 2. Register user via API (initial subscription ends in 1 month by default)
	doAuth(r, "POST", "/api/v1/users/register", `{"telegram_id":5001,"username":"payment_extender_user"}`)

	// Get subscription to check its initial expiry (which is nil)
	sub, err := testReg.Subscriptions().FindByEmail(context.TODO(), "bot_client_5001")
	if err != nil {
		t.Fatalf("failed to find subscription: %v", err)
	}
	if sub.EndsAt != nil {
		t.Fatalf("expected initial EndsAt to be nil, got %v", sub.EndsAt)
	}

	// 3. Create a pending payment for that plan
	body := fmt.Sprintf(`{"telegram_id":5001,"amount":300,"payment_type":"subscription","method":"platega","plan_id":%d}`, plan.ID)
	wCreate := doAuth(r, "POST", "/api/v1/payments/create", body)
	if wCreate.Code != http.StatusCreated {
		t.Fatalf("failed to create payment: %s", wCreate.Body.String())
	}
	pid := int(jsonBody(t, wCreate)["payment_id"].(float64))

	// 4. Mark payment as completed
	wUpdate := doAuth(r, "POST", fmt.Sprintf("/api/v1/payments/%d/status", pid), `{"status":"completed","expected_statuses":["pending_card"]}`)
	if wUpdate.Code != http.StatusOK {
		t.Fatalf("failed to complete payment: %s", wUpdate.Body.String())
	}

	// 5. Verify the subscription is extended by 3 months (endsAt should be roughly time.Now() + 3 months)
	updatedSub, err := testReg.Subscriptions().FindByEmail(context.TODO(), "bot_client_5001")
	if err != nil {
		t.Fatalf("failed to load updated subscription: %v", err)
	}

	if updatedSub.EndsAt == nil {
		t.Fatal("expected updated subscription EndsAt to be non-nil")
	}

	// Check if EndsAt is roughly in 3 months
	expectedYear, expectedMonth, _ := time.Now().AddDate(0, 3, 0).Date()
	actualYear, actualMonth, _ := updatedSub.EndsAt.Date()
	if expectedYear != actualYear || expectedMonth != actualMonth {
		t.Errorf("expected subscription extended to %d-%d, got %d-%d", expectedYear, expectedMonth, actualYear, actualMonth)
	}
}
