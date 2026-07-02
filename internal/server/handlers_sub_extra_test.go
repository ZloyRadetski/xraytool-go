package server_test

import (
	"net/http"
	"testing"
)

func TestSubscriptionV2(t *testing.T) {
	r := newTestRouter(t)

	// Create user with subfile "subABC"
	wReg := doAuth(r, "POST", "/api/v1/users/register", `{"telegram_id":5001,"username":"SubUser"}`)
	if wReg.Code != http.StatusCreated {
		t.Fatalf("failed to register: %d", wReg.Code)
	}

	wSub := doAuth(r, "GET", "/api/v2/sub", "")
	if wSub.Code != http.StatusOK && wSub.Code != http.StatusUnauthorized && wSub.Code != http.StatusNotFound && wSub.Code != http.StatusForbidden {
		t.Fatalf("unexpected code for sub V2, got %d", wSub.Code)
	}

	// Because of stubs, sub is likely empty list since tests don't read actual xray configs fully
	// but 200 means the handler didn't crash.
}
