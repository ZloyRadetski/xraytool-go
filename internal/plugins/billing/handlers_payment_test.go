package billing

import (
	"bytes"
	"context"
	"fmt"
	json "github.com/goccy/go-json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"xraytool/internal/appconfig"
	"xraytool/internal/database"
	"xraytool/internal/domain"
	"xraytool/internal/events"
	"xraytool/internal/pluginapi"
	"xraytool/internal/plugins/engine_xray"
	usersvc "xraytool/internal/plugins/user_management/service"
)

type billingFakeResolver struct {
	services map[string]any
	err      error
}

func (r billingFakeResolver) Resolve(name string) (any, error) {
	if r.err != nil {
		return nil, r.err
	}
	svc, ok := r.services[name]
	if !ok {
		return nil, fmt.Errorf("service not found: %s", name)
	}
	return svc, nil
}

func (r billingFakeResolver) Logger() pluginapi.Logger {
	return slog.Default()
}

func (r billingFakeResolver) EmitEvent(eventType string, data map[string]any, userMeta map[string]any) {}

func (r billingFakeResolver) DB() pluginapi.PluginDBHandle {
	return nil
}

func setupBillingPluginTest(t *testing.T) (*Plugin, domain.Registry, *usersvc.Service) {
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
	uSvc := usersvc.NewService(reg, usersvc.Config{IsMaster: true}, &engine_xray.NoopEngine{}, nil, slog.Default())

	cfg := &appconfig.Config{}
	p := NewPlugin(cfg)
	p.log = slog.Default()
	p.registry = reg
	p.paymentSvc = NewService(reg, disp, slog.Default())
	p.userSvc = uSvc
	p.dispatcher = disp
	p.engine = &engine_xray.NoopEngine{}
	p.authMiddleware = func(next http.Handler) http.Handler { return next }

	return p, reg, uSvc
}

func TestPlugin_Metadata(t *testing.T) {
	p := NewPlugin(&appconfig.Config{})
	meta := p.Metadata()
	require.Equal(t, "billing", meta.Name)
	require.Equal(t, "payment", meta.Kind)
	require.NotEmpty(t, meta.Publishes)
}

func TestPlugin_Lifecycle(t *testing.T) {
	cfg := &appconfig.Config{}
	p := NewPlugin(cfg)
	require.Error(t, p.Health(context.Background()))

	_, reg, uSvc := setupBillingPluginTest(t)
	authMw := func(next http.Handler) http.Handler { return next }

	resolver := billingFakeResolver{
		services: map[string]any{
			pluginapi.ServiceDomainRegistry:        reg,
			pluginapi.ServiceDomainEngine:          &engine_xray.NoopEngine{},
			pluginapi.ServiceEventDispatcher:       events.NewDispatcher(&events.Config{}),
			pluginapi.ServiceUserManagement:        uSvc,
			pluginapi.ServiceProtectedMiddleware: authMw,
		},
	}

	require.NoError(t, p.Init(context.Background(), nil, resolver))
	require.NoError(t, p.Health(context.Background()))
	require.NotNil(t, p.PublishedServices())
	require.NoError(t, p.Stop(context.Background()))
}

func TestHandler_CreatePayment_Success(t *testing.T) {
	p, reg, _ := setupBillingPluginTest(t)
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	ctx := context.Background()
	user := &domain.User{ID: "h-user-1", Username: "huser", RefCode: "H_REF_1", Metadata: domain.Metadata{"email": "huser@test.com"}}
	require.NoError(t, reg.Users().Create(ctx, user))

	plan := &domain.Plan{Months: 1, BasePrice: 300, IsActive: true}
	require.NoError(t, reg.Plans().Create(ctx, plan))

	body := []byte(fmt.Sprintf(`{
		"email": "huser@test.com",
		"payment_type": "subscription",
		"method": "card",
		"plan_id": %d,
		"max_devices": 3
	}`, plan.ID))

	req := httptest.NewRequest("POST", "/api/v1/payments/create", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code)

	var resp map[string]any
	err := json.Unmarshal(rec.Body.Bytes(), &resp)
	require.NoError(t, err)
	require.NotNil(t, resp["payment_id"])
}

func TestHandler_GetPayment(t *testing.T) {
	p, reg, _ := setupBillingPluginTest(t)
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	ctx := context.Background()
	user := &domain.User{ID: "h-user-2", Username: "huser2", RefCode: "H_REF_2"}
	require.NoError(t, reg.Users().Create(ctx, user))

	pay := &domain.Payment{
		UserID:      user.ID,
		Amount:      600,
		Status:      "pending_card",
		PaymentType: "topup",
	}
	require.NoError(t, reg.Payments().Create(ctx, pay))

	// 1. Success
	req := httptest.NewRequest("GET", fmt.Sprintf("/api/v1/payments/%d", pay.ID), nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var fetched map[string]any
	err := json.Unmarshal(rec.Body.Bytes(), &fetched)
	require.NoError(t, err)
	require.Equal(t, float64(pay.ID), fetched["id"])
	require.Equal(t, float64(600), fetched["amount"])

	// 2. Not found
	req = httptest.NewRequest("GET", "/api/v1/payments/99999", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code)

	// 3. Invalid ID
	req = httptest.NewRequest("GET", "/api/v1/payments/invalid", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestHandler_ListPayments(t *testing.T) {
	p, reg, _ := setupBillingPluginTest(t)
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	ctx := context.Background()
	user := &domain.User{ID: "h-user-3", Username: "huser3", RefCode: "H_REF_3"}
	require.NoError(t, reg.Users().Create(ctx, user))

	_ = reg.Payments().Create(ctx, &domain.Payment{UserID: user.ID, Amount: 100, Status: "completed", PaymentType: "topup"})
	_ = reg.Payments().Create(ctx, &domain.Payment{UserID: user.ID, Amount: 200, Status: "pending_card", PaymentType: "topup"})

	req := httptest.NewRequest("GET", "/api/v1/payments", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var list []map[string]any
	err := json.Unmarshal(rec.Body.Bytes(), &list)
	require.NoError(t, err)
	require.Len(t, list, 2)
}

func TestHandler_UpdatePaymentStatus(t *testing.T) {
	p, reg, _ := setupBillingPluginTest(t)
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	ctx := context.Background()
	user := &domain.User{ID: "h-user-4", Username: "huser4", RefCode: "H_REF_4"}
	require.NoError(t, reg.Users().Create(ctx, user))

	pay := &domain.Payment{
		UserID:      user.ID,
		Amount:      500,
		Status:      "pending_card",
		PaymentType: "topup",
	}
	require.NoError(t, reg.Payments().Create(ctx, pay))

	// 1. Success update
	body := []byte(`{"status":"completed","expected_statuses":["pending_card"]}`)
	req := httptest.NewRequest("POST", fmt.Sprintf("/api/v1/payments/%d/status", pay.ID), bytes.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	// 2. Conflict update (mismatch expected status)
	bodyConflict := []byte(`{"status":"failed","expected_statuses":["pending_card"]}`)
	req = httptest.NewRequest("POST", fmt.Sprintf("/api/v1/payments/%d/status", pay.ID), bytes.NewReader(bodyConflict))
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusConflict, rec.Code)
}

func TestScrubberWorker(t *testing.T) {
	p, _, _ := setupBillingPluginTest(t)
	worker := NewScrubberWorker(p.paymentSvc, slog.Default())
	worker.interval = 10 * time.Millisecond
	worker.retention = 1 * time.Hour

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()

	// Run should execute startup scrub and terminate cleanly on ctx.Done()
	worker.Run(ctx)
}
