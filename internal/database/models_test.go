package database_test

import (
	"encoding/json"
	"testing"

	"xraytool/internal/database"
)

func TestMetadata_Serialization(t *testing.T) {
	m := database.Metadata{"key": "value", "num": float64(42)}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("failed to marshal metadata: %v", err)
	}

	var m2 database.Metadata
	if err := json.Unmarshal(data, &m2); err != nil {
		t.Fatalf("failed to unmarshal metadata: %v", err)
	}

	if m2["key"] != "value" || m2["num"].(float64) != 42 {
		t.Errorf("expected key=value, num=42, got %v", m2)
	}
}

func TestUser_DefaultValues(t *testing.T) {
	db := newTestDB(t)
	db.Create(&database.User{ID: "u1", Username: "user1"})

	var u database.User
	db.First(&u, "id = ?", "u1")

	if u.Balance != 0 {
		t.Errorf("expected Balance=0, got %d", u.Balance)
	}
	if u.IsAdmin {
		t.Errorf("expected IsAdmin=false")
	}
}

func TestSubscription_DefaultMaxDevices(t *testing.T) {
	db := newTestDB(t)
	db.Create(&database.Subscription{ID: "s1", UserID: "u1", Email: "e", XrayUUID: "x"})

	var s database.Subscription
	db.First(&s, "id = ?", "s1")

	if s.MaxDevices != 3 {
		t.Errorf("expected MaxDevices=3, got %d", s.MaxDevices)
	}
	if s.AutoRenew {
		t.Errorf("expected AutoRenew=false")
	}
	if s.Status != "inactive" {
		t.Errorf("expected Status=inactive, got %s", s.Status)
	}
}

func TestPayment_DefaultTimestamp(t *testing.T) {
	db := newTestDB(t)
	db.Create(&database.Payment{UserID: "u1", Amount: 100, Status: "p", PaymentType: "t", ExternalID: strPtr("e")})

	var p database.Payment
	db.First(&p, "user_id = ?", "u1")

	if p.CreatedAt.IsZero() {
		t.Error("expected CreatedAt to be set")
	}
}
