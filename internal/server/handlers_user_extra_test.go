package server_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"xraytool/internal/database"
)

func TestListAdmins(t *testing.T) {
	r := newTestRouter(t)

	// Admin
	doAuth(r, "POST", "/api/v1/users/register", `{"telegram_id":3001,"username":"Admin1"}`)

	resUpdate := testDB.Model(&database.User{}).Where("username = ?", "Admin1").Update("is_admin", true)
	if resUpdate.Error != nil {
		t.Fatalf("failed to set admin: %v", resUpdate.Error)
	}
	if resUpdate.RowsAffected == 0 {
		t.Fatalf("no rows updated")
	}

	w := doAuth(r, "GET", "/api/v1/users/admins", "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var res []float64
	// Parse manually to access array
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}

	if len(res) != 1 {
		t.Errorf("expected 1 admin, got %d", len(res))
	} else if int(res[0]) != 3001 {
		t.Errorf("expected admin 3001, got %v", res[0])
	}
}

func TestGetUserByRef(t *testing.T) {
	r := newTestRouter(t)
	wReg := doAuth(r, "POST", "/api/v1/users/register", `{"telegram_id":3003,"username":"RefUser"}`)
	if wReg.Code != http.StatusCreated && wReg.Code != http.StatusOK {
		t.Fatalf("failed to register user: %d %s", wReg.Code, wReg.Body.String())
	}

	refCodeRaw := jsonBody(t, wReg)["ref_code"]
	if refCodeRaw == nil {
		t.Fatalf("ref_code is missing from response: %v", wReg.Body.String())
	}
	refCode := refCodeRaw.(string)

	wGet := doAuth(r, "GET", "/api/v1/users/ref/"+refCode, "")
	if wGet.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", wGet.Code)
	}

	if jsonBody(t, wGet)["username"] != "RefUser" {
		t.Errorf("wrong username")
	}

	wNotFound := doAuth(r, "GET", "/api/v1/users/ref/NON_EXISTENT", "")
	if wNotFound.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", wNotFound.Code)
	}
}

func TestAutoRenewToggle(t *testing.T) {
	r := newTestRouter(t)
	doAuth(r, "POST", "/api/v1/users/register", `{"telegram_id":3004,"username":"ToggleUser"}`)

	w := doAuth(r, "POST", "/api/v1/users/telegram/3004/auto-renew-toggle", `{"auto_renew":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	wg := doAuth(r, "GET", "/api/v1/users/telegram/3004", "")
	if jsonBody(t, wg)["auto_renew"] != true {
		t.Errorf("expected auto_renew to be true")
	}

	// Test direct auto-renew handler (POST /api/v1/users/telegram/{id}/auto-renew)
	wAuto := doAuth(r, "POST", "/api/v1/users/telegram/3004/auto-renew", "")
	if wAuto.Code != http.StatusOK && wAuto.Code != http.StatusBadRequest {
		// Just call it to increase coverage (it might fail due to insufficient balance, returning 400)
		t.Errorf("expected 200 or 400, got %d", wAuto.Code)
	}

	// Test missing user cases
	wMiss := doAuth(r, "POST", "/api/v1/users/telegram/99999/auto-renew-toggle", `{"auto_renew":true}`)
	if wMiss.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", wMiss.Code)
	}
}

func TestSetMetadata(t *testing.T) {
	r := newTestRouter(t)
	doAuth(r, "POST", "/api/v1/users/register", `{"telegram_id":3005,"username":"MetaUser"}`)

	w := doAuth(r, "POST", "/api/v1/users/telegram/3005/metadata", `{"key":"key1", "value":"value1"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	wg := doAuth(r, "GET", "/api/v1/users/telegram/3005", "")
	meta := jsonBody(t, wg)["metadata"].(map[string]interface{})
	if meta["key1"] != "value1" {
		t.Errorf("metadata mismatch")
	}
}

func TestAdminUnblockUser(t *testing.T) {
	r := newTestRouter(t)
	doAuth(r, "POST", "/api/v1/users/register", `{"telegram_id":3006,"username":"BlockUnblockUser"}`)

	doAuth(r, "POST", "/api/v1/admin/users/bot_client_3006/block", "")
	wgBlocked := doAuth(r, "GET", "/api/v1/users/telegram/3006", "")
	if jsonBody(t, wgBlocked)["sub_status"] != "blocked" {
		t.Errorf("expected blocked")
	}

	w := doAuth(r, "POST", "/api/v1/admin/users/bot_client_3006/unblock", "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	wgUnblocked := doAuth(r, "GET", "/api/v1/users/telegram/3006", "")
	if jsonBody(t, wgUnblocked)["sub_status"] != "active" {
		t.Errorf("expected active")
	}

	// Not found user
	wNotFound := doAuth(r, "POST", "/api/v1/admin/users/nonexistent@email.com/block", "")
	if wNotFound.Code != http.StatusNotFound {
		t.Errorf("expected 404 for non-existent user block, got %d", wNotFound.Code)
	}

	wNotFoundUnblock := doAuth(r, "POST", "/api/v1/admin/users/nonexistent@email.com/unblock", "")
	if wNotFoundUnblock.Code != http.StatusNotFound {
		t.Errorf("expected 404 for non-existent user unblock, got %d", wNotFoundUnblock.Code)
	}

	wNotFoundExpire := doAuth(r, "POST", "/api/v1/admin/users/nonexistent@email.com/set-expire", `{"expire":"2026-07-04T00:00:00Z"}`)
	if wNotFoundExpire.Code != http.StatusNotFound {
		t.Errorf("expected 404 for non-existent user expire, got %d", wNotFoundExpire.Code)
	}
}
