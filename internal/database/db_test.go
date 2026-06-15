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
	if u.Metadata != nil && len(u.Metadata) > 0 {
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
