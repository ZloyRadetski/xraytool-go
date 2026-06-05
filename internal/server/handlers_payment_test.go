package server_test

import (
	"fmt"
	"net/http"
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
