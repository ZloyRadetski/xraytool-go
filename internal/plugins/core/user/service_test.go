package user_test

import (
	"context"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"xraytool/internal/database"
	"xraytool/internal/domain"
	"xraytool/internal/plugins/core/user"
	vpn "xraytool/internal/plugins/engine_xray"
)

func newTestService(t *testing.T) (*user.Service, domain.Registry) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := gorm.Open(sqlite.Open(dbPath+"?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("get test sql db: %v", err)
	}
	t.Cleanup(func() {
		_ = sqlDB.Close()
	})
	if err := db.AutoMigrate(
		&database.User{},
		&database.Subscription{},
		&database.Device{},
		&database.Payment{},
		&database.ReferralReward{},
		&database.SubscriptionNotification{},
		&database.Plan{},
		&database.PromoCode{},
	); err != nil {
		t.Fatalf("automigrate: %v", err)
	}

	reg := database.NewRegistry(db)
	svc := user.NewService(reg, user.Config{IsMaster: true}, &vpn.NoopEngine{}, nil, slog.Default())
	return svc, reg
}

func TestSanitizeRefCode(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"valid standard", "ref_12345678", "ref_12345678"},
		{"valid with spaces", "  ref_abc123  ", "ref_abc123"},
		{"empty", "", ""},
		{"whitespace only", "   ", ""},
		{"sql injection attempt", "' OR '1'='1", ""},
		{"script tag attempt", "<script>alert(1)</script>", ""},
		{"too long", "ref_12345678901234567890123456789012345678901234567890123456789012345", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := user.SanitizeRefCode(tt.input)
			if got != tt.expected {
				t.Errorf("SanitizeRefCode(%q) = %q; want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestFindOrCreateWebUser_RefCodeEdgeCases(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	// 1. Create a referrer user
	referrer, err := svc.FindOrCreateWebUser(ctx, "referrer@example.com")
	if err != nil {
		t.Fatalf("failed to create referrer: %v", err)
	}
	refCode := referrer.RefCode
	if refCode == "" {
		t.Fatalf("referrer refCode is empty")
	}

	// 2. Non-existent ref_code
	u1, err := svc.FindOrCreateWebUser(ctx, "user1@example.com", "ref_non_existent")
	if err != nil {
		t.Fatalf("FindOrCreateWebUser failed for non-existent ref code: %v", err)
	}
	if u1.ReferredBy != nil {
		t.Errorf("expected ReferredBy to be nil for non-existent ref code, got %v", *u1.ReferredBy)
	}

	// 3. Malicious / special characters in ref_code
	u2, err := svc.FindOrCreateWebUser(ctx, "user2@example.com", "<script>alert('xss')</script>")
	if err != nil {
		t.Fatalf("FindOrCreateWebUser failed for malicious ref code: %v", err)
	}
	if u2.ReferredBy != nil {
		t.Errorf("expected ReferredBy to be nil for malicious ref code, got %v", *u2.ReferredBy)
	}

	// 4. Self-referral attempt (passing own email's ref code)
	uSelf, err := svc.FindOrCreateWebUser(ctx, "referrer@example.com", refCode)
	if err != nil {
		t.Fatalf("FindOrCreateWebUser failed on existing user lookup: %v", err)
	}
	if uSelf.ID != referrer.ID {
		t.Errorf("expected existing referrer user ID %s, got %s", referrer.ID, uSelf.ID)
	}

	// 5. Valid referral
	u3, err := svc.FindOrCreateWebUser(ctx, "user3@example.com", refCode)
	if err != nil {
		t.Fatalf("FindOrCreateWebUser failed for valid referral: %v", err)
	}
	if u3.ReferredBy == nil || *u3.ReferredBy != referrer.ID {
		t.Errorf("expected ReferredBy = %s, got %v", referrer.ID, u3.ReferredBy)
	}

	// 6. Concurrent requests for same user
	const concurrency = 10
	var wg sync.WaitGroup
	errs := make(chan error, concurrency)
	users := make(chan *domain.User, concurrency)

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			u, err := svc.FindOrCreateWebUser(ctx, "concurrent@example.com", refCode)
			if err != nil {
				errs <- err
				return
			}
			users <- u
		}()
	}
	wg.Wait()
	close(errs)
	close(users)

	for err := range errs {
		t.Errorf("concurrent creation failed: %v", err)
	}
	var firstID string
	for u := range users {
		if firstID == "" {
			firstID = u.ID
		} else if u.ID != firstID {
			t.Errorf("concurrent creation produced different user IDs: %s vs %s", firstID, u.ID)
		}
	}

	// 7. Verify referral count on referrer
	count, err := svc.CountReferrals(ctx, referrer.ID)
	if err != nil {
		t.Fatalf("CountReferrals failed: %v", err)
	}
	// user3 and concurrent@example.com were referred by referrer (2 total)
	if count != 2 {
		t.Errorf("expected referral count 2, got %d", count)
	}
}

func TestRegisterTelegramUser_RefCodeEdgeCases(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	// 1. Create a referrer Telegram user
	referrer, err := svc.RegisterTelegramUser(ctx, user.RegisterTelegramUserRequest{
		TelegramID: 10001,
		Username:   "referrer_tg",
	})
	if err != nil {
		t.Fatalf("failed to register referrer TG user: %v", err)
	}
	refCode := referrer.RefCode

	// 2. Self-referral attempt (TG user tries to register with their own ref code)
	uSelf, err := svc.RegisterTelegramUser(ctx, user.RegisterTelegramUserRequest{
		TelegramID:     10001,
		Username:       "referrer_tg",
		ReferredByCode: refCode,
	})
	if err != nil {
		t.Fatalf("RegisterTelegramUser failed: %v", err)
	}
	if uSelf.ID != referrer.ID {
		t.Errorf("expected existing user ID %s, got %s", referrer.ID, uSelf.ID)
	}

	// 3. Register user with invalid ref code
	uInvalid, err := svc.RegisterTelegramUser(ctx, user.RegisterTelegramUserRequest{
		TelegramID:     10002,
		Username:       "user_invalid_ref",
		ReferredByCode: "' OR 1=1 --",
	})
	if err != nil {
		t.Fatalf("RegisterTelegramUser failed: %v", err)
	}
	if uInvalid.ReferredBy != nil {
		t.Errorf("expected ReferredBy to be nil for invalid ref code, got %v", *uInvalid.ReferredBy)
	}

	// 4. Valid Telegram referral
	uReferred, err := svc.RegisterTelegramUser(ctx, user.RegisterTelegramUserRequest{
		TelegramID:     10003,
		Username:       "user_referred",
		ReferredByCode: refCode,
	})
	if err != nil {
		t.Fatalf("RegisterTelegramUser failed: %v", err)
	}
	if uReferred.ReferredBy == nil || *uReferred.ReferredBy != referrer.ID {
		t.Errorf("expected ReferredBy = %s, got %v", referrer.ID, uReferred.ReferredBy)
	}

	// 5. Concurrent Telegram registrations
	const concurrency = 10
	var wg sync.WaitGroup
	errs := make(chan error, concurrency)
	users := make(chan *domain.User, concurrency)

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			u, err := svc.RegisterTelegramUser(ctx, user.RegisterTelegramUserRequest{
				TelegramID: 99999,
				Username:   "concurrent_tg",
			})
			if err != nil {
				errs <- err
				return
			}
			users <- u
		}()
	}
	wg.Wait()
	close(errs)
	close(users)

	for err := range errs {
		t.Errorf("concurrent TG registration failed: %v", err)
	}
	var firstID string
	for u := range users {
		if firstID == "" {
			firstID = u.ID
		} else if u.ID != firstID {
			t.Errorf("concurrent TG registration produced different user IDs: %s vs %s", firstID, u.ID)
		}
	}
}

func TestReferralStatsAndRewards(t *testing.T) {
	svc, reg := newTestService(t)
	ctx := context.Background()

	referrer, err := svc.FindOrCreateWebUser(ctx, "ref_boss@example.com")
	if err != nil {
		t.Fatalf("failed to create referrer: %v", err)
	}

	userA, _ := svc.FindOrCreateWebUser(ctx, "ref_a@example.com", referrer.RefCode)
	_, _ = svc.FindOrCreateWebUser(ctx, "ref_b@example.com", referrer.RefCode)

	// Count referrals should equal 2 (both registered)
	count, err := svc.CountReferrals(ctx, referrer.ID)
	if err != nil || count != 2 {
		t.Fatalf("expected 2 referrals, got count=%d, err=%v", count, err)
	}

	// Add referral reward for payment from userA
	if err := reg.Users().AddReferralReward(ctx, referrer.ID, userA.ID, 100, 250); err != nil {
		t.Fatalf("AddReferralReward failed: %v", err)
	}

	totalReward, err := svc.SumReferralRewards(ctx, referrer.ID)
	if err != nil || totalReward != 250 {
		t.Fatalf("expected total reward 250, got %d (err: %v)", totalReward, err)
	}

	stats, err := svc.GetReferralStats(ctx, []string{referrer.ID})
	if err != nil || len(stats) == 0 {
		t.Fatalf("GetReferralStats failed: %v", err)
	}
	if stats[0].Count != 2 || stats[0].Total != 250 {
		t.Errorf("expected stats count=2, total=250; got count=%d, total=%d", stats[0].Count, stats[0].Total)
	}
}
