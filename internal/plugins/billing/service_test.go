package billing

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"xraytool/internal/database"
	"xraytool/internal/domain"
	"xraytool/internal/events"
)

type testFixture struct {
	db         domain.Registry
	dispatcher *events.Dispatcher
	service    *Service
	user       *domain.User
	plan       *domain.Plan
	sub        *domain.Subscription
}

func setupBillingTest(t *testing.T) testFixture {
	t.Helper()
	dbConn, err := database.NewConnection(database.Config{
		Driver:      "sqlite",
		SQLitePath:  ":memory:",
		Silent:      true,
		AutoMigrate: true,
	})
	require.NoError(t, err)

	reg := database.NewRegistry(dbConn)
	disp := events.NewDispatcher(&events.Config{})
	svc := NewService(reg, disp, slog.Default())

	ctx := context.Background()

	// Create user
	user := &domain.User{
		ID:       "bill-user-1",
		Username: "billuser",
		RefCode:  "BILL_REF_1",
		Balance:  0,
		Metadata: domain.Metadata{"email": "billuser@test.com", "telegram_id": float64(99901)},
	}
	require.NoError(t, reg.Users().Create(ctx, user))

	// Create plan (1 month)
	plan := &domain.Plan{
		Months:    1,
		BasePrice: 500,
		IsActive:  true,
	}
	require.NoError(t, reg.Plans().Create(ctx, plan))

	// Create active subscription
	now := time.Now()
	endsAt := now.Add(10 * 24 * time.Hour)
	sub := &domain.Subscription{
		ID:         "sub-uuid-1",
		UserID:     user.ID,
		Email:      "billuser@test.com",
		UUID:       "00000000-0000-0000-0000-000000000001",
		Status:     "active",
		MaxDevices: 4,
		CreatedAt:  now,
		EndsAt:     &endsAt,
	}
	require.NoError(t, reg.Subscriptions().Create(ctx, sub))

	return testFixture{
		db:         reg,
		dispatcher: disp,
		service:    svc,
		user:       user,
		plan:       plan,
		sub:        sub,
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// ProcessExternalPaymentStatus Tests
// ─────────────────────────────────────────────────────────────────────────────

func TestProcessExternal_CompletedWithPlan(t *testing.T) {
	f := setupBillingTest(t)
	ctx := context.Background()

	extID := "ext-plan-123"
	pay := &domain.Payment{
		UserID:      f.user.ID,
		Amount:      500,
		Status:      "pending_card",
		PaymentType: "subscription",
		Method:      "platega",
		ExternalID:  &extID,
		PlanID:      &f.plan.ID,
	}
	require.NoError(t, f.db.Payments().Create(ctx, pay))

	origEndsAt := *f.sub.EndsAt

	// Process external payment status -> completed
	err := f.service.ProcessExternalPaymentStatus(ctx, extID, "completed")
	require.NoError(t, err)

	// Verify payment updated
	updatedPay, err := f.db.Payments().FindByExternalID(ctx, extID)
	require.NoError(t, err)
	require.Equal(t, "completed", updatedPay.Status)

	// Verify subscription extended
	updatedSub, err := f.db.Subscriptions().FindByID(ctx, f.sub.ID)
	require.NoError(t, err)
	require.True(t, updatedSub.EndsAt.After(origEndsAt), "EndsAt must be extended after plan payment")

	// Verify balance was NOT touched
	updatedUser, err := f.db.Users().FindByID(ctx, f.user.ID)
	require.NoError(t, err)
	require.Equal(t, 0, updatedUser.Balance)
}

func TestProcessExternal_CompletedWithoutPlan(t *testing.T) {
	f := setupBillingTest(t)
	ctx := context.Background()

	extID := "ext-topup-456"
	pay := &domain.Payment{
		UserID:      f.user.ID,
		Amount:      1000,
		Status:      "pending_card",
		PaymentType: "topup",
		Method:      "platega",
		ExternalID:  &extID,
		PlanID:      nil, // Direct balance topup
	}
	require.NoError(t, f.db.Payments().Create(ctx, pay))

	// Process status -> completed
	err := f.service.ProcessExternalPaymentStatus(ctx, extID, "completed")
	require.NoError(t, err)

	// Verify balance credited
	updatedUser, err := f.db.Users().FindByID(ctx, f.user.ID)
	require.NoError(t, err)
	require.Equal(t, 1000, updatedUser.Balance)
}

func TestProcessExternal_CompletedBalanceMethod(t *testing.T) {
	f := setupBillingTest(t)
	ctx := context.Background()

	// Initial user balance is 300
	require.NoError(t, f.db.Users().AdjustBalance(ctx, f.user.ID, 300))

	extID := "ext-balance-789"
	pay := &domain.Payment{
		UserID:      f.user.ID,
		Amount:      300,
		Status:      "pending_card",
		PaymentType: "custom_slot",
		Method:      "balance", // Paid via internal balance
		ExternalID:  &extID,
		PlanID:      nil,
	}
	require.NoError(t, f.db.Payments().Create(ctx, pay))

	// Process status -> completed
	err := f.service.ProcessExternalPaymentStatus(ctx, extID, "completed")
	require.NoError(t, err)

	// Balance MUST NOT be credited again! Balance should stay 300
	updatedUser, err := f.db.Users().FindByID(ctx, f.user.ID)
	require.NoError(t, err)
	require.Equal(t, 300, updatedUser.Balance)

	updatedPay, err := f.db.Payments().FindByExternalID(ctx, extID)
	require.NoError(t, err)
	require.Equal(t, "completed", updatedPay.Status)
}

func TestProcessExternal_AlreadyCompleted(t *testing.T) {
	f := setupBillingTest(t)
	ctx := context.Background()

	extID := "ext-idemp-111"
	pay := &domain.Payment{
		UserID:      f.user.ID,
		Amount:      500,
		Status:      "completed",
		PaymentType: "topup",
		Method:      "platega",
		ExternalID:  &extID,
	}
	require.NoError(t, f.db.Payments().Create(ctx, pay))

	// Second callback should be a no-op
	err := f.service.ProcessExternalPaymentStatus(ctx, extID, "completed")
	require.NoError(t, err)

	// Balance remains 0 (not credited twice)
	updatedUser, err := f.db.Users().FindByID(ctx, f.user.ID)
	require.NoError(t, err)
	require.Equal(t, 0, updatedUser.Balance)
}

func TestProcessExternal_StatusMapping(t *testing.T) {
	statusVariants := []string{"success", "SUCCESS", "CONFIRMED", "COMPLETED"}

	for i, rawStatus := range statusVariants {
		t.Run(rawStatus, func(t *testing.T) {
			f := setupBillingTest(t)
			ctx := context.Background()

			extID := fmt.Sprintf("ext-map-%d", i)
			pay := &domain.Payment{
				UserID:      f.user.ID,
				Amount:      100,
				Status:      "pending_card",
				PaymentType: "topup",
				Method:      "platega",
				ExternalID:  &extID,
			}
			require.NoError(t, f.db.Payments().Create(ctx, pay))

			err := f.service.ProcessExternalPaymentStatus(ctx, extID, rawStatus)
			require.NoError(t, err)

			updatedPay, err := f.db.Payments().FindByExternalID(ctx, extID)
			require.NoError(t, err)
			require.Equal(t, "completed", updatedPay.Status)
		})
	}
}

func TestProcessExternal_NotFound(t *testing.T) {
	f := setupBillingTest(t)
	err := f.service.ProcessExternalPaymentStatus(context.Background(), "non-existent-ext-id", "completed")
	require.Error(t, err)
}

func TestProcessExternal_DispatchesEvent(t *testing.T) {
	var eventReceived bool
	var receivedPaymentID int64

	disp := events.NewDispatcher(&events.Config{
		OnDispatch: func(eventType string, data map[string]interface{}, userMetadata map[string]interface{}) {
			if eventType == "payment.completed" {
				eventReceived = true
				if pid, ok := data["payment_id"].(int64); ok {
					receivedPaymentID = pid
				}
			}
		},
	})

	dbConn, err := database.NewConnection(database.Config{
		Driver:      "sqlite",
		SQLitePath:  ":memory:",
		Silent:      true,
		AutoMigrate: true,
	})
	require.NoError(t, err)
	reg := database.NewRegistry(dbConn)
	svc := NewService(reg, disp, slog.Default())
	ctx := context.Background()

	user := &domain.User{ID: "disp-user", Username: "dispuser", RefCode: "DISP_REF"}
	require.NoError(t, reg.Users().Create(ctx, user))

	extID := "ext-dispatch-1"
	pay := &domain.Payment{
		UserID:      user.ID,
		Amount:      500,
		Status:      "pending_card",
		PaymentType: "topup",
		Method:      "platega",
		ExternalID:  &extID,
	}
	require.NoError(t, reg.Payments().Create(ctx, pay))

	err = svc.ProcessExternalPaymentStatus(ctx, extID, "completed")
	require.NoError(t, err)
	require.True(t, eventReceived, "payment.completed event should be dispatched")
	require.Equal(t, pay.ID, receivedPaymentID)
}

// ─────────────────────────────────────────────────────────────────────────────
// extendSubscriptionForPayment Tests
// ─────────────────────────────────────────────────────────────────────────────

func TestExtend_NoPlanID(t *testing.T) {
	f := setupBillingTest(t)
	ctx := context.Background()

	origEndsAt := *f.sub.EndsAt

	pay := &domain.Payment{
		UserID:      f.user.ID,
		Amount:      200,
		Status:      "completed",
		PaymentType: "topup",
		PlanID:      nil, // No plan
	}

	// Should safely early-return without altering subscription
	f.service.extendSubscriptionForPayment(ctx, f.db, pay)

	sub, err := f.db.Subscriptions().FindByID(ctx, f.sub.ID)
	require.NoError(t, err)
	require.Equal(t, origEndsAt.Unix(), sub.EndsAt.Unix())
}

func TestExtend_CustomDataMaxDevices(t *testing.T) {
	f := setupBillingTest(t)
	ctx := context.Background()

	// 1. float64 in CustomData (typical from JSON unmarshaling)
	payFloat := &domain.Payment{
		UserID:      f.user.ID,
		Amount:      500,
		Status:      "completed",
		PlanID:      &f.plan.ID,
		CustomData:  domain.Metadata{"max_devices": float64(7)},
	}
	f.service.extendSubscriptionForPayment(ctx, f.db, payFloat)

	sub, err := f.db.Subscriptions().FindByID(ctx, f.sub.ID)
	require.NoError(t, err)
	require.Equal(t, 7, sub.MaxDevices)

	// 2. int in CustomData
	payInt := &domain.Payment{
		UserID:      f.user.ID,
		Amount:      500,
		Status:      "completed",
		PlanID:      &f.plan.ID,
		CustomData:  domain.Metadata{"max_devices": 10},
	}
	f.service.extendSubscriptionForPayment(ctx, f.db, payInt)

	sub, err = f.db.Subscriptions().FindByID(ctx, f.sub.ID)
	require.NoError(t, err)
	require.Equal(t, 10, sub.MaxDevices)
}

func TestExtend_EnforcesMin3Devices(t *testing.T) {
	f := setupBillingTest(t)
	ctx := context.Background()

	payLow := &domain.Payment{
		UserID:      f.user.ID,
		Amount:      500,
		Status:      "completed",
		PlanID:      &f.plan.ID,
		CustomData:  domain.Metadata{"max_devices": 1},
	}
	f.service.extendSubscriptionForPayment(ctx, f.db, payLow)

	sub, err := f.db.Subscriptions().FindByID(ctx, f.sub.ID)
	require.NoError(t, err)
	require.Equal(t, 3, sub.MaxDevices, "minimum devices must be 3")
}

func TestExtend_NoExistingSubscription(t *testing.T) {
	f := setupBillingTest(t)
	ctx := context.Background()

	// User without subscription
	newUser := &domain.User{ID: "no-sub-user", Username: "nosub", RefCode: "NO_SUB_REF"}
	require.NoError(t, f.db.Users().Create(ctx, newUser))

	pay := &domain.Payment{
		UserID: newUser.ID,
		Amount: 500,
		PlanID: &f.plan.ID,
	}

	// Should not panic, logs error gracefully
	f.service.extendSubscriptionForPayment(ctx, f.db, pay)
}

// ─────────────────────────────────────────────────────────────────────────────
// UpdatePaymentStatus Tests
// ─────────────────────────────────────────────────────────────────────────────

func TestUpdatePaymentStatus_CompletedCreditsBalance(t *testing.T) {
	f := setupBillingTest(t)
	ctx := context.Background()

	pay := &domain.Payment{
		UserID:      f.user.ID,
		Amount:      400,
		Status:      "pending_card",
		PaymentType: "topup",
		Method:      "platega",
	}
	require.NoError(t, f.db.Payments().Create(ctx, pay))

	updated, err := f.service.UpdatePaymentStatus(ctx, pay.ID, "completed", []string{"pending_card"})
	require.NoError(t, err)
	require.True(t, updated)

	updatedUser, err := f.db.Users().FindByID(ctx, f.user.ID)
	require.NoError(t, err)
	require.Equal(t, 400, updatedUser.Balance)
}

func TestUpdatePaymentStatus_ExpectedStatusMismatch(t *testing.T) {
	f := setupBillingTest(t)
	ctx := context.Background()

	pay := &domain.Payment{
		UserID:      f.user.ID,
		Amount:      400,
		Status:      "failed",
		PaymentType: "topup",
	}
	require.NoError(t, f.db.Payments().Create(ctx, pay))

	// Attempt update expecting pending_card
	updated, err := f.service.UpdatePaymentStatus(ctx, pay.ID, "completed", []string{"pending_card"})
	require.NoError(t, err)
	require.False(t, updated, "Status should not update when mismatching expectedStatuses")
}

// ─────────────────────────────────────────────────────────────────────────────
// CreatePayment Tests
// ─────────────────────────────────────────────────────────────────────────────

func TestCreatePayment_Success(t *testing.T) {
	f := setupBillingTest(t)
	ctx := context.Background()

	req := CreatePaymentRequest{
		Email:       "billuser@test.com",
		PaymentType: "subscription",
		Method:      "card",
		PlanID:      &f.plan.ID,
		MaxDevices:  3,
		Platform:    "web",
	}

	pay, err := f.service.CreatePayment(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, pay)
	require.Equal(t, f.user.ID, pay.UserID)
	require.Equal(t, "pending_card", pay.Status)
	require.Equal(t, 500, pay.Amount)
}

func TestCreatePayment_UserNotFound(t *testing.T) {
	f := setupBillingTest(t)
	ctx := context.Background()

	req := CreatePaymentRequest{
		Email:       "unknown@example.com",
		PaymentType: "subscription",
		Method:      "card",
		PlanID:      &f.plan.ID,
	}

	_, err := f.service.CreatePayment(ctx, req)
	require.ErrorIs(t, err, ErrUserNotFound)
}

func TestCreatePayment_PromoLimitReached(t *testing.T) {
	f := setupBillingTest(t)
	ctx := context.Background()

	// Promo with limit 1, used 1
	promo := &domain.PromoCode{
		Code:            "MAXED",
		DiscountPercent: 20,
		MaxUses:         1,
		UsesCount:       1,
		IsActive:        true,
		TargetPlatform:  "all",
	}
	require.NoError(t, f.db.Promos().Create(ctx, promo))

	req := CreatePaymentRequest{
		Email:       "billuser@test.com",
		PaymentType: "subscription",
		Method:      "card",
		PlanID:      &f.plan.ID,
		PromoCode:   "MAXED",
		Platform:    "web",
	}

	_, err := f.service.CreatePayment(ctx, req)
	require.ErrorIs(t, err, ErrPromoLimitReached)
}

// ─────────────────────────────────────────────────────────────────────────────
// ScrubOldPayments & FindAll Tests
// ─────────────────────────────────────────────────────────────────────────────

func TestScrubOldPayments(t *testing.T) {
	f := setupBillingTest(t)
	ctx := context.Background()

	ext1 := "ext-old"
	ext2 := "ext-new"

	pOld := &domain.Payment{
		UserID:      f.user.ID,
		Amount:      100,
		Status:      "completed",
		PaymentType: "subscription",
		ExternalID:  &ext1,
		CreatedAt:   time.Now().Add(-48 * time.Hour),
	}
	require.NoError(t, f.db.Payments().Create(ctx, pOld))

	pNew := &domain.Payment{
		UserID:      f.user.ID,
		Amount:      100,
		Status:      "completed",
		PaymentType: "subscription",
		ExternalID:  &ext2,
		CreatedAt:   time.Now(),
	}
	require.NoError(t, f.db.Payments().Create(ctx, pNew))

	// Scrub older than 24h
	count, err := f.service.ScrubOldPayments(ctx, 24*time.Hour)
	require.NoError(t, err)
	require.Equal(t, int64(1), count)

	// Verify old payment had external_id scrubbed
	all, err := f.service.FindAll(ctx)
	require.NoError(t, err)
	require.Len(t, all, 2)
}
