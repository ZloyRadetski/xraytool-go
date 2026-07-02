package server_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

func TestListPayments(t *testing.T) {
	r := newTestRouter(t)
	doAuth(r, "POST", "/api/v1/users/register", `{"telegram_id":4001,"username":"PaymentUser"}`)

	wCreate := doAuth(r, "POST", "/api/v1/payments/create", `{"telegram_id":4001,"amount":100,"payment_type":"subscription","method":"cryptobot"}`)
	if wCreate.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d", wCreate.Code)
	}

	wList := doAuth(r, "GET", "/api/v1/payments?telegram_id=4001", "")
	if wList.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", wList.Code)
	}

	var res []map[string]interface{}
	if err := json.Unmarshal(wList.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}

	if len(res) != 1 {
		t.Errorf("expected 1 payment, got %d", len(res))
	}
	if int(res[0]["amount"].(float64)) != 100 {
		t.Errorf("expected amount 100")
	}
}

func TestGetPayment(t *testing.T) {
	r := newTestRouter(t)
	doAuth(r, "POST", "/api/v1/users/register", `{"telegram_id":4002,"username":"PaymentUser2"}`)

	wCreate := doAuth(r, "POST", "/api/v1/payments/create", `{"telegram_id":4002,"amount":250,"payment_type":"balance","method":"stars"}`)
	pid := int(jsonBody(t, wCreate)["payment_id"].(float64))

	wGet := doAuth(r, "GET", fmt.Sprintf("/api/v1/payments/%d", pid), "")
	if wGet.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", wGet.Code)
	}

	j := jsonBody(t, wGet)
	if int(j["amount"].(float64)) != 250 {
		t.Errorf("expected amount 250")
	}
	if j["payment_type"] != "balance" {
		t.Errorf("expected type balance")
	}

	// Not found payment
	wNotFound := doAuth(r, "GET", "/api/v1/payments/99999", "")
	if wNotFound.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", wNotFound.Code)
	}
}

func TestCreatePayment_EdgeCases(t *testing.T) {
	r := newTestRouter(t)

	// Bad JSON
	wBadJSON := doAuth(r, "POST", "/api/v1/payments/create", `{"telegram_id":4003,"amount":`)
	if wBadJSON.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for bad json, got %d", wBadJSON.Code)
	}

	// Missing amount
	wNoAmount := doAuth(r, "POST", "/api/v1/payments/create", `{"telegram_id":4003,"amount":0}`)
	if wNoAmount.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for zero amount, got %d", wNoAmount.Code)
	}

	// Missing user
	wNoUser := doAuth(r, "POST", "/api/v1/payments/create", `{"telegram_id":99999,"amount":100,"payment_type":"subscription"}`)
	if wNoUser.Code != http.StatusNotFound {
		t.Errorf("expected 404 for missing user, got %d", wNoUser.Code)
	}
}
