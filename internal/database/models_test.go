package database_test

import (
	json "github.com/goccy/go-json"
	"reflect"
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
	db.Create(&database.Subscription{ID: "s1", UserID: "u1", Email: "e", UUID: "x"})

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

func TestPlan_DefaultValues(t *testing.T) {
	db := newTestDB(t)

	plan := database.Plan{Months: 24, BasePrice: 3000, EngineIDs: []string{"xray", "singbox"}}
	err := db.Create(&plan).Error
	if err != nil {
		t.Fatalf("failed to create plan: %v", err)
	}

	var p database.Plan
	db.First(&p, "id = ?", plan.ID)

	if p.GlobalDiscountPercent != 0 {
		t.Errorf("expected GlobalDiscountPercent=0, got %d", p.GlobalDiscountPercent)
	}
	if !p.IsActive {
		t.Errorf("expected IsActive=true")
	}
	if got, want := p.EngineIDs, []string{"xray", "singbox"}; !reflect.DeepEqual(got, want) {
		t.Errorf("expected EngineIDs=%v, got %v", want, got)
	}
}

func TestPromoCode_DefaultValues(t *testing.T) {
	db := newTestDB(t)

	promo := database.PromoCode{Code: "TESTCODE", DiscountPercent: 10}
	err := db.Create(&promo).Error
	if err != nil {
		t.Fatalf("failed to create promo code: %v", err)
	}

	var pc database.PromoCode
	db.First(&pc, "id = ?", promo.ID)

	if pc.MaxUses != 0 {
		t.Errorf("expected MaxUses=0, got %d", pc.MaxUses)
	}
	if pc.TargetPlatform != "all" {
		t.Errorf("expected TargetPlatform='all', got %q", pc.TargetPlatform)
	}
	if !pc.IsActive {
		t.Errorf("expected IsActive=true")
	}
}

func TestPayment_WithPlanAndPromoCode(t *testing.T) {
	db := newTestDB(t)

	// Create user
	user := database.User{ID: "u_pay_test", Username: "pay_test"}
	db.Create(&user)

	planID := int64(1)
	promoID := int64(1)

	payment := database.Payment{
		UserID:      user.ID,
		Amount:      100,
		Status:      "completed",
		PaymentType: "subscription",
		PlanID:      &planID,
		PromoCodeID: &promoID,
	}

	err := db.Create(&payment).Error
	if err != nil {
		t.Fatalf("failed to create payment: %v", err)
	}

	var p database.Payment
	db.First(&p, "id = ?", payment.ID)

	if p.PlanID == nil || *p.PlanID != planID {
		t.Errorf("expected PlanID=%d", planID)
	}
	if p.PromoCodeID == nil || *p.PromoCodeID != promoID {
		t.Errorf("expected PromoCodeID=%d", promoID)
	}
}
