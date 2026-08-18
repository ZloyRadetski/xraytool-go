package referral

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"xraytool/internal/database"
	"xraytool/internal/domain"
	"xraytool/internal/pluginapi"
)

type fakeResolver struct {
	services map[string]any
	err      error
}

func (r fakeResolver) Resolve(name string) (any, error) {
	if r.err != nil {
		return nil, r.err
	}
	svc, ok := r.services[name]
	if !ok {
		return nil, fmt.Errorf("service not found: %s", name)
	}
	return svc, nil
}

func (r fakeResolver) Logger() pluginapi.Logger {
	return slog.Default()
}

func (r fakeResolver) EmitEvent(eventType string, data map[string]any, userMeta map[string]any) {}

func (r fakeResolver) DB() pluginapi.PluginDBHandle {
	return nil
}

func setupTestRegistry(t *testing.T) domain.Registry {
	t.Helper()
	db, err := database.NewConnection(database.Config{
		Driver:      "sqlite",
		SQLitePath:  ":memory:",
		Silent:      true,
		AutoMigrate: true,
	})
	require.NoError(t, err)
	return database.NewRegistry(db)
}

func TestPlugin_Metadata(t *testing.T) {
	p := NewPlugin()
	meta := p.Metadata()
	require.Equal(t, "referral", meta.Name)
	require.Equal(t, "event_sink", meta.Kind)
	require.NotEmpty(t, meta.Requires)
	require.Equal(t, pluginapi.ServiceDomainRegistry, meta.Requires[0].Name)
}

func TestPlugin_LifecycleAndHealth(t *testing.T) {
	p := NewPlugin()
	require.Error(t, p.Health(context.Background()), "Health should fail before Init")

	reg := setupTestRegistry(t)
	resolver := fakeResolver{
		services: map[string]any{
			pluginapi.ServiceDomainRegistry: reg,
		},
	}

	err := p.Init(context.Background(), nil, resolver)
	require.NoError(t, err)
	require.NoError(t, p.Health(context.Background()))
	require.NoError(t, p.Stop(context.Background()))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	require.NoError(t, p.Start(ctx))
}

func TestPlugin_InitErrors(t *testing.T) {
	p := NewPlugin()

	// Missing dependency
	err := p.Init(context.Background(), nil, fakeResolver{services: map[string]any{}})
	require.Error(t, err)

	// Resolver error
	err = p.Init(context.Background(), nil, fakeResolver{err: fmt.Errorf("resolver error")})
	require.Error(t, err)

	// Wrong type
	err = p.Init(context.Background(), nil, fakeResolver{
		services: map[string]any{
			pluginapi.ServiceDomainRegistry: "not-a-registry",
		},
	})
	require.Error(t, err)
}

func TestPlugin_Handle_NonPaymentEvent(t *testing.T) {
	p := NewPlugin()
	p.registry = setupTestRegistry(t)
	p.log = slog.Default()

	err := p.Handle(context.Background(), pluginapi.Event{
		Type: "subscription.created",
		Data: map[string]any{"payment_id": 123},
	})
	require.NoError(t, err)
}

func TestPlugin_Handle_InvalidPaymentID(t *testing.T) {
	p := NewPlugin()
	p.registry = setupTestRegistry(t)
	p.log = slog.Default()

	// Missing payment_id
	err := p.Handle(context.Background(), pluginapi.Event{
		Type: "payment.completed",
		Data: map[string]any{},
	})
	require.NoError(t, err)

	// String payment_id
	err = p.Handle(context.Background(), pluginapi.Event{
		Type: "payment.completed",
		Data: map[string]any{"payment_id": "invalid"},
	})
	require.NoError(t, err)
}

func TestPlugin_Handle_PaymentNotFound(t *testing.T) {
	p := NewPlugin()
	p.registry = setupTestRegistry(t)
	p.log = slog.Default()

	err := p.Handle(context.Background(), pluginapi.Event{
		Type: "payment.completed",
		Data: map[string]any{"payment_id": 999999},
	})
	require.Error(t, err, "non-existent payment should return error")
}

func TestPlugin_Handle_RewardApplied(t *testing.T) {
	reg := setupTestRegistry(t)
	ctx := context.Background()

	// Create referrer user
	referrer := &domain.User{
		ID:       "ref-user-id",
		Username: "referrer",
		RefCode:  "REF_CODE_1",
		Balance:  0,
	}
	require.NoError(t, reg.Users().Create(ctx, referrer))

	// Create referred user
	referred := &domain.User{
		ID:         "payer-user-id",
		Username:   "payer",
		RefCode:    "REF_CODE_2",
		ReferredBy: &referrer.ID,
		Balance:    0,
	}
	require.NoError(t, reg.Users().Create(ctx, referred))

	// Create completed payment
	pay := &domain.Payment{
		UserID:      referred.ID,
		Amount:      400,
		Status:      "completed",
		PaymentType: "subscription",
		Method:      "platega",
	}
	require.NoError(t, reg.Payments().Create(ctx, pay))

	p := NewPlugin()
	p.registry = reg
	p.log = slog.Default()

	// Handle event
	err := p.Handle(ctx, pluginapi.Event{
		Type: "payment.completed",
		Data: map[string]any{"payment_id": pay.ID},
	})
	require.NoError(t, err)

	// Verify referrer balance got 25% of 400 = 100
	updatedReferrer, err := reg.Users().FindByID(ctx, referrer.ID)
	require.NoError(t, err)
	require.Equal(t, 100, updatedReferrer.Balance)
}

func TestPlugin_Handle_TypeCoercion(t *testing.T) {
	reg := setupTestRegistry(t)
	ctx := context.Background()

	referrer := &domain.User{ID: "ref-user-2", Username: "ref2", RefCode: "REF_CODE_3"}
	require.NoError(t, reg.Users().Create(ctx, referrer))

	referred := &domain.User{ID: "payer-user-2", Username: "payer2", RefCode: "REF_CODE_4", ReferredBy: &referrer.ID}
	require.NoError(t, reg.Users().Create(ctx, referred))

	pay := &domain.Payment{
		UserID:      referred.ID,
		Amount:      200,
		Status:      "completed",
		PaymentType: "subscription",
		Method:      "platega",
	}
	require.NoError(t, reg.Payments().Create(ctx, pay))

	p := NewPlugin()
	p.registry = reg
	p.log = slog.Default()

	// 1. float64 payment_id (standard JSON unmarshaling)
	err := p.Handle(ctx, pluginapi.Event{
		Type: "payment.completed",
		Data: map[string]any{"payment_id": float64(pay.ID)},
	})
	require.NoError(t, err)

	// 2. int payment_id
	err = p.Handle(ctx, pluginapi.Event{
		Type: "payment.completed",
		Data: map[string]any{"payment_id": int(pay.ID)},
	})
	require.NoError(t, err)

	// 3. int64 payment_id
	err = p.Handle(ctx, pluginapi.Event{
		Type: "payment.completed",
		Data: map[string]any{"payment_id": int64(pay.ID)},
	})
	require.NoError(t, err)
}

func TestPlugin_Handle_NoReferrer(t *testing.T) {
	reg := setupTestRegistry(t)
	ctx := context.Background()

	user := &domain.User{ID: "standalone-user", Username: "standalone", RefCode: "REF_CODE_5"}
	require.NoError(t, reg.Users().Create(ctx, user))

	pay := &domain.Payment{
		UserID:      user.ID,
		Amount:      500,
		Status:      "completed",
		PaymentType: "subscription",
	}
	require.NoError(t, reg.Payments().Create(ctx, pay))

	p := NewPlugin()
	p.registry = reg
	p.log = slog.Default()

	err := p.Handle(ctx, pluginapi.Event{
		Type: "payment.completed",
		Data: map[string]any{"payment_id": pay.ID},
	})
	require.NoError(t, err)
}

func TestPlugin_Handle_SmallAmount(t *testing.T) {
	reg := setupTestRegistry(t)
	ctx := context.Background()

	referrer := &domain.User{ID: "ref-small", Username: "refsmall", RefCode: "REF_CODE_6"}
	require.NoError(t, reg.Users().Create(ctx, referrer))

	referred := &domain.User{ID: "payer-small", Username: "payersmall", RefCode: "REF_CODE_7", ReferredBy: &referrer.ID}
	require.NoError(t, reg.Users().Create(ctx, referred))

	// Amount 3 / 4 = 0 reward
	pay := &domain.Payment{
		UserID:      referred.ID,
		Amount:      3,
		Status:      "completed",
		PaymentType: "subscription",
	}
	require.NoError(t, reg.Payments().Create(ctx, pay))

	p := NewPlugin()
	p.registry = reg
	p.log = slog.Default()

	err := p.Handle(ctx, pluginapi.Event{
		Type: "payment.completed",
		Data: map[string]any{"payment_id": pay.ID},
	})
	require.NoError(t, err)

	updatedRef, err := reg.Users().FindByID(ctx, referrer.ID)
	require.NoError(t, err)
	require.Equal(t, 0, updatedRef.Balance)
}
