package server_test

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
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
