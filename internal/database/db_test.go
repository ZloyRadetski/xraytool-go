package database_test

import (
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"xraytool/internal/database"
)

func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	if err := db.AutoMigrate(
		&database.User{},
		&database.Subscription{},
		&database.Device{},
		&database.Payment{},
		&database.ReferralReward{},
		&database.SubscriptionNotification{}, // kept in sync with database.Init()
		&database.Plan{},
		&database.PromoCode{},
	); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	return db
}

func TestUser_CreateAndRead(t *testing.T) {
	db := newTestDB(t)
	user := database.User{
		ID:       "uuid-123",
		Username: "alice",
		Metadata: database.Metadata{"telegram_id": float64(12345), "source": "telegram_bot"},
	}
	if err := db.Create(&user).Error; err != nil {
		t.Fatalf("failed to create user: %v", err)
	}

	var fetched database.User
	if err := db.First(&fetched, "id = ?", "uuid-123").Error; err != nil {
		t.Fatalf("failed to find user: %v", err)
	}
	if fetched.Username != "alice" {
		t.Errorf("expected username alice, got %s", fetched.Username)
	}

	tgID := fetched.Metadata["telegram_id"]
	if tgID.(float64) != 12345 {
		t.Errorf("expected telegram_id 12345, got %v", tgID)
	}
}

func TestUser_RefCodeUnique(t *testing.T) {
	db := newTestDB(t)
	u1 := database.User{ID: "1", Username: "u1", RefCode: "ref_1"}
	u2 := database.User{ID: "2", Username: "u2", RefCode: "ref_1"}

	if err := db.Create(&u1).Error; err != nil {
		t.Fatalf("failed to create u1: %v", err)
	}
	if err := db.Create(&u2).Error; err == nil {
		t.Fatal("expected unique constraint error for ref_code, got nil")
	}
}

func TestUser_MetadataLIKESearch(t *testing.T) {
	db := newTestDB(t)
	user := database.User{
		ID:       "1",
		Username: "alice",
		Metadata: database.Metadata{"telegram_id": float64(99999)},
	}
	db.Create(&user)

	var found database.User
	if err := db.Where("metadata LIKE ?", "%\"telegram_id\":99999%").First(&found).Error; err != nil {
		t.Fatalf("failed to find user by metadata LIKE: %v", err)
	}

	var notFound database.User
	if err := db.Where("metadata LIKE ?", "%\"telegram_id\":00001%").First(&notFound).Error; err == nil {
		t.Fatal("expected not to find user, but did")
	}
}

func TestSubscription_CreateAndLink(t *testing.T) {
	db := newTestDB(t)
	user := database.User{ID: "u1", Username: "bob"}
	db.Create(&user)

	sub := database.Subscription{
		ID:       "s1",
		UserID:   "u1",
		Email:    "bot_client_12345",
		XrayUUID: "uuid",
	}
	if err := db.Create(&sub).Error; err != nil {
		t.Fatalf("failed to create subscription: %v", err)
	}

	var fetched database.Subscription
	db.First(&fetched, "id = ?", "s1")
	if fetched.Email != "bot_client_12345" {
		t.Errorf("expected email bot_client_12345, got %s", fetched.Email)
	}
	if fetched.Status != "inactive" {
		t.Errorf("expected status inactive, got %s", fetched.Status)
	}
	if fetched.MaxDevices != 3 {
		t.Errorf("expected max devices 3, got %d", fetched.MaxDevices)
	}
}

func TestSubscription_StatusUpdate(t *testing.T) {
	db := newTestDB(t)
	sub := database.Subscription{ID: "s1", UserID: "u1", Email: "e", XrayUUID: "x"}
	db.Create(&sub)

	db.Model(&database.Subscription{}).Where("id = ?", "s1").Update("status", "active")

	var fetched database.Subscription
	db.First(&fetched, "id = ?", "s1")
	if fetched.Status != "active" {
		t.Errorf("expected active, got %s", fetched.Status)
	}
}

func TestDevice_TrackingAndCount(t *testing.T) {
	db := newTestDB(t)
	sub := database.Subscription{ID: "s1", UserID: "u1", Email: "e", XrayUUID: "x"}
	db.Create(&sub)

	db.Create(&database.Device{SubscriptionID: "s1", HWID: "h1"})
	db.Create(&database.Device{SubscriptionID: "s1", HWID: "h2"})
	db.Create(&database.Device{SubscriptionID: "s1", HWID: "h3"})

	var count int64
	db.Model(&database.Device{}).Where("subscription_id = ?", "s1").Count(&count)
	if count != 3 {
		t.Errorf("expected 3 devices, got %d", count)
	}
}

func TestPayment_CreateAndFilter(t *testing.T) {
	db := newTestDB(t)
	db.Create(&database.Payment{UserID: "u1", Amount: 100, Status: "pending_card", PaymentType: "t", ExternalID: strPtr("e1")})
	db.Create(&database.Payment{UserID: "u1", Amount: 200, Status: "completed", PaymentType: "t", ExternalID: strPtr("e2")})

	var count int64
	db.Model(&database.Payment{}).Where("status = ?", "pending_card").Count(&count)
	if count != 1 {
		t.Errorf("expected 1 pending_card, got %d", count)
	}

	db.Model(&database.Payment{}).Where("status = ?", "nonexistent").Count(&count)
	if count != 0 {
		t.Errorf("expected 0, got %d", count)
	}
}

func TestPayment_AtomicStatusUpdate(t *testing.T) {
	db := newTestDB(t)
	db.Create(&database.Payment{ID: 1, UserID: "u1", Amount: 100, Status: "pending_card", PaymentType: "t", ExternalID: strPtr("e1")})

	res := db.Model(&database.Payment{}).Where("id = ? AND status IN ?", 1, []string{"pending_card"}).Update("status", "completed")
	if res.RowsAffected != 1 {
		t.Errorf("expected 1 row affected, got %d", res.RowsAffected)
	}

	res = db.Model(&database.Payment{}).Where("id = ? AND status IN ?", 1, []string{"pending_card"}).Update("status", "completed")
	if res.RowsAffected != 0 {
		t.Errorf("expected 0 row affected on second attempt, got %d", res.RowsAffected)
	}
}

func strPtr(s string) *string { return &s }

func TestPayment_ExternalIDUnique(t *testing.T) {
	db := newTestDB(t)
	p1 := database.Payment{UserID: "u1", Amount: 100, Status: "p", PaymentType: "t", ExternalID: strPtr("ext1")}
	p2 := database.Payment{UserID: "u2", Amount: 100, Status: "p", PaymentType: "t", ExternalID: strPtr("ext1")}

	if err := db.Create(&p1).Error; err != nil {
		t.Fatalf("failed to create p1: %v", err)
	}
	if err := db.Create(&p2).Error; err == nil {
		t.Fatal("expected unique constraint error for external_id, got nil")
	}
}

func TestPayment_NullExternalIDAllowsMultiple(t *testing.T) {
	db := newTestDB(t)
	// Multiple manual payments (nil ExternalID) must NOT violate the unique index.
	p1 := database.Payment{UserID: "u1", Amount: 100, Status: "p", PaymentType: "t", ExternalID: nil}
	p2 := database.Payment{UserID: "u2", Amount: 200, Status: "p", PaymentType: "t", ExternalID: nil}

	if err := db.Create(&p1).Error; err != nil {
		t.Fatalf("failed to create p1 with nil ExternalID: %v", err)
	}
	if err := db.Create(&p2).Error; err != nil {
		t.Fatalf("failed to create p2 with nil ExternalID (NULL uniqueness violation): %v", err)
	}
}

func TestReferralReward_CreateAndSum(t *testing.T) {
	db := newTestDB(t)
	db.Create(&database.ReferralReward{ReferrerID: "r1", ReferredID: "r2", PaymentID: 1, Amount: 40})
	db.Create(&database.ReferralReward{ReferrerID: "r1", ReferredID: "r3", PaymentID: 2, Amount: 40})
	db.Create(&database.ReferralReward{ReferrerID: "r1", ReferredID: "r4", PaymentID: 3, Amount: 40})

	var total int64
	db.Model(&database.ReferralReward{}).Where("referrer_id = ?", "r1").Select("COALESCE(SUM(amount),0)").Scan(&total)
	if total != 120 {
		t.Errorf("expected total 120, got %d", total)
	}

	var count int64
	db.Model(&database.ReferralReward{}).Where("referrer_id = ?", "r1").Count(&count)
	if count != 3 {
		t.Errorf("expected 3 rows, got %d", count)
	}
}

func TestUser_BalanceUpdate_Atomic(t *testing.T) {
	db := newTestDB(t)
	db.Create(&database.User{ID: "u1", Username: "u1", Balance: 100})

	res := db.Model(&database.User{}).Where("id = ? AND balance >= ?", "u1", 50).Update("balance", gorm.Expr("balance - ?", 50))
	if res.RowsAffected != 1 {
		t.Errorf("expected 1 row affected, got %d", res.RowsAffected)
	}

	var u database.User
	db.First(&u, "id = ?", "u1")
	if u.Balance != 50 {
		t.Errorf("expected balance 50, got %d", u.Balance)
	}

	res = db.Model(&database.User{}).Where("id = ? AND balance >= ?", "u1", 100).Update("balance", gorm.Expr("balance - ?", 100))
	if res.RowsAffected != 0 {
		t.Errorf("expected 0 row affected, got %d", res.RowsAffected)
	}
}

func TestMetadata_NullHandling(t *testing.T) {
	db := newTestDB(t)
	db.Create(&database.User{ID: "u1", Username: "u1", Metadata: nil})

	var u database.User
	db.First(&u, "id = ?", "u1")
	if len(u.Metadata) > 0 {
		t.Errorf("expected nil or empty metadata, got %v", u.Metadata)
	}
}

func TestSubscription_NullableTimes(t *testing.T) {
	db := newTestDB(t)
	sub := database.Subscription{ID: "s1", UserID: "u1", Email: "e", XrayUUID: "x", StartsAt: nil, EndsAt: nil}
	db.Create(&sub)

	var fetched database.Subscription
	db.First(&fetched, "id = ?", "s1")
	if fetched.StartsAt != nil || fetched.EndsAt != nil {
		t.Errorf("expected nil times, got starts_at=%v ends_at=%v", fetched.StartsAt, fetched.EndsAt)
	}

	now := time.Now()
	db.Model(&database.Subscription{}).Where("id = ?", "s1").Update("ends_at", now)

	db.First(&fetched, "id = ?", "s1")
	if fetched.EndsAt == nil || fetched.EndsAt.Unix() != now.Unix() {
		t.Errorf("expected ends_at %v, got %v", now, fetched.EndsAt)
	}
}

func TestUser_CRUD(t *testing.T) {
	db := newTestDB(t)

	// Create
	user := database.User{
		ID:       "crud-u1",
		Username: "crud_user",
		Balance:  100,
	}
	if err := db.Create(&user).Error; err != nil {
		t.Fatalf("failed to create user: %v", err)
	}

	// Read
	var readUser database.User
	if err := db.First(&readUser, "id = ?", "crud-u1").Error; err != nil {
		t.Fatalf("failed to read user: %v", err)
	}
	if readUser.Username != "crud_user" || readUser.Balance != 100 {
		t.Errorf("unexpected user data: %+v", readUser)
	}

	// Update
	if err := db.Model(&database.User{}).Where("id = ?", "crud-u1").Updates(map[string]interface{}{"username": "updated_user", "balance": 200}).Error; err != nil {
		t.Fatalf("failed to update user: %v", err)
	}

	var updatedUser database.User
	db.First(&updatedUser, "id = ?", "crud-u1")
	if updatedUser.Username != "updated_user" || updatedUser.Balance != 200 {
		t.Errorf("expected updated user data, got: %+v", updatedUser)
	}

	// Delete
	if err := db.Delete(&database.User{}, "id = ?", "crud-u1").Error; err != nil {
		t.Fatalf("failed to delete user: %v", err)
	}

	// Ensure deleted
	if err := db.First(&database.User{}, "id = ?", "crud-u1").Error; err == nil {
		t.Fatal("expected error reading deleted user, got nil")
	}
}

func TestPayment_CRUD(t *testing.T) {
	db := newTestDB(t)

	// Create User for FK constraint (even though SQLite memory might not enforce by default, good practice)
	db.Create(&database.User{ID: "crud-u2", Username: "user2"})

	// Create
	payment := database.Payment{
		UserID:      "crud-u2",
		Amount:      500,
		Status:      "pending",
		PaymentType: "sub",
	}
	if err := db.Create(&payment).Error; err != nil {
		t.Fatalf("failed to create payment: %v", err)
	}

	// Read
	var readPayment database.Payment
	if err := db.First(&readPayment, "id = ?", payment.ID).Error; err != nil {
		t.Fatalf("failed to read payment: %v", err)
	}
	if readPayment.Amount != 500 || readPayment.Status != "pending" {
		t.Errorf("unexpected payment data: %+v", readPayment)
	}

	// Update
	if err := db.Model(&database.Payment{}).Where("id = ?", payment.ID).Update("status", "completed").Error; err != nil {
		t.Fatalf("failed to update payment: %v", err)
	}

	var updatedPayment database.Payment
	db.First(&updatedPayment, "id = ?", payment.ID)
	if updatedPayment.Status != "completed" {
		t.Errorf("expected updated status, got: %s", updatedPayment.Status)
	}

	// Delete
	if err := db.Delete(&database.Payment{}, "id = ?", payment.ID).Error; err != nil {
		t.Fatalf("failed to delete payment: %v", err)
	}

	// Ensure deleted
	if err := db.First(&database.Payment{}, "id = ?", payment.ID).Error; err == nil {
		t.Fatal("expected error reading deleted payment, got nil")
	}
}

func TestPlan_CRUD(t *testing.T) {
	db := newTestDB(t)

	// Create
	plan := database.Plan{
		Months:    1,
		BasePrice: 100,
		IsActive:  true,
	}
	if err := db.Create(&plan).Error; err != nil {
		t.Fatalf("failed to create plan: %v", err)
	}

	// Read
	var readPlan database.Plan
	if err := db.First(&readPlan, "id = ?", plan.ID).Error; err != nil {
		t.Fatalf("failed to read plan: %v", err)
	}
	if readPlan.Months != 1 || readPlan.BasePrice != 100 {
		t.Errorf("unexpected plan data: %+v", readPlan)
	}

	// Update
	if err := db.Model(&database.Plan{}).Where("id = ?", plan.ID).Updates(map[string]interface{}{"months": 2, "base_price": 150}).Error; err != nil {
		t.Fatalf("failed to update plan: %v", err)
	}

	var updatedPlan database.Plan
	db.First(&updatedPlan, "id = ?", plan.ID)
	if updatedPlan.Months != 2 || updatedPlan.BasePrice != 150 {
		t.Errorf("expected updated plan data, got: %+v", updatedPlan)
	}

	// Delete
	if err := db.Delete(&database.Plan{}, "id = ?", plan.ID).Error; err != nil {
		t.Fatalf("failed to delete plan: %v", err)
	}

	// Ensure deleted
	if err := db.First(&database.Plan{}, "id = ?", plan.ID).Error; err == nil {
		t.Fatal("expected error reading deleted plan, got nil")
	}
}

func TestPromoCode_CRUD(t *testing.T) {
	db := newTestDB(t)

	// Create
	promo := database.PromoCode{
		Code:            "CRUD10",
		DiscountPercent: 10,
		MaxUses:         100,
		TargetPlatform:  "all",
	}
	if err := db.Create(&promo).Error; err != nil {
		t.Fatalf("failed to create promo code: %v", err)
	}

	// Read
	var readPromo database.PromoCode
	if err := db.First(&readPromo, "id = ?", promo.ID).Error; err != nil {
		t.Fatalf("failed to read promo code: %v", err)
	}
	if readPromo.Code != "CRUD10" || readPromo.DiscountPercent != 10 {
		t.Errorf("unexpected promo code data: %+v", readPromo)
	}

	// Update
	if err := db.Model(&database.PromoCode{}).Where("id = ?", promo.ID).Updates(map[string]interface{}{"discount_percent": 20, "max_uses": 50}).Error; err != nil {
		t.Fatalf("failed to update promo code: %v", err)
	}

	var updatedPromo database.PromoCode
	db.First(&updatedPromo, "id = ?", promo.ID)
	if updatedPromo.DiscountPercent != 20 || updatedPromo.MaxUses != 50 {
		t.Errorf("expected updated promo data, got: %+v", updatedPromo)
	}

	// Delete
	if err := db.Delete(&database.PromoCode{}, "id = ?", promo.ID).Error; err != nil {
		t.Fatalf("failed to delete promo code: %v", err)
	}

	// Ensure deleted
	if err := db.First(&database.PromoCode{}, "id = ?", promo.ID).Error; err == nil {
		t.Fatal("expected error reading deleted promo code, got nil")
	}
}
