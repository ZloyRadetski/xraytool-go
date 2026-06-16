package database_test

import (
	"testing"
	"time"
	"xraytool/internal/database"
)

func TestPromoCode_Expiration(t *testing.T) {
	db := newTestDB(t)

	now := time.Now()
	expiredTime := now.Add(-24 * time.Hour)
	futureTime := now.Add(24 * time.Hour)

	promoExpired := database.PromoCode{
		Code:            "EXPIRED",
		DiscountPercent: 10,
		MaxUses:         100,
		TargetPlatform:  "all",
		ExpiresAt:       &expiredTime,
	}
	if err := db.Create(&promoExpired).Error; err != nil {
		t.Fatalf("failed to create expired promo: %v", err)
	}

	promoValid := database.PromoCode{
		Code:            "VALID",
		DiscountPercent: 20,
		MaxUses:         10,
		TargetPlatform:  "all",
		ExpiresAt:       &futureTime,
	}
	if err := db.Create(&promoValid).Error; err != nil {
		t.Fatalf("failed to create valid promo: %v", err)
	}

	// Read both and check logic
	var readExpired, readValid database.PromoCode
	db.First(&readExpired, "code = ?", "EXPIRED")
	db.First(&readValid, "code = ?", "VALID")

	if readExpired.ExpiresAt == nil || readExpired.ExpiresAt.After(time.Now()) {
		t.Errorf("expected EXPIRED promo to be expired")
	}

	if readValid.ExpiresAt == nil || readValid.ExpiresAt.Before(time.Now()) {
		t.Errorf("expected VALID promo to be valid")
	}
}
