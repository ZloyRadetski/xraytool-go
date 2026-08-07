package server_test

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"xraytool/internal/database"
)

func TestReferralRewardForPayment(t *testing.T) {
	r := newTestRouter(t)

	// Referrer
	doAuth(r, "POST", "/api/v1/users/register", `{"telegram_id":6001,"username":"Referrer"}`)

	// Referee
	doAuth(r, "POST", "/api/v1/users/register", `{"telegram_id":6002,"username":"Referee"}`)

	// Setup referral in DB

	var referrer, referee database.User
	testDB.Where("username = ?", "Referrer").First(&referrer)
	testDB.Where("username = ?", "Referee").First(&referee)

	testDB.Model(&referee).Update("referred_by", referrer.ID)

	var check database.User
	testDB.First(&check, "id = ?", referee.ID)
	if check.ReferredBy == nil || *check.ReferredBy != referrer.ID {
		t.Fatalf("referred_by not set correctly: %v", check.ReferredBy)
	}

	// Create payment
	wCreate := doAuth(r, "POST", "/api/v1/payments/create", `{"telegram_id":6002,"amount":1000,"payment_type":"subscription","method":"cryptobot"}`)
	pid := int(jsonBody(t, wCreate)["payment_id"].(float64))

	// Complete payment -> applies referral reward (should be 1000 * 25% = 250)
	wStatus := doAuth(r, "POST", fmt.Sprintf("/api/v1/payments/%d/status", pid), `{"status":"completed","expected_statuses":["pending_card"]}`)
	if wStatus.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", wStatus.Code)
	}

	time.Sleep(100 * time.Millisecond) // wait for background goroutine to finish

	wRef := doAuth(r, "GET", "/api/v1/users/telegram/6001", "")
	bal := int(jsonBody(t, wRef)["balance"].(float64))
	if bal != 250 {
		t.Errorf("expected 250 reward, got %d", bal)
	}
}
