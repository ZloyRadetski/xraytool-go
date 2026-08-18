package promo

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

	"xraytool/internal/database"
	"xraytool/internal/domain"
	"xraytool/internal/pluginapi"
	"xraytool/internal/plugins/engine_xray"
	usersvc "xraytool/internal/plugins/user_management/service"
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

func setupTestPlugin(t *testing.T) (*Plugin, domain.Registry, *usersvc.Service) {
	t.Helper()
	db, err := database.NewConnection(database.Config{
		Driver:      "sqlite",
		SQLitePath:  ":memory:",
		Silent:      true,
		AutoMigrate: true,
	})
	require.NoError(t, err)
	registry := database.NewRegistry(db)
	userSvc := usersvc.NewService(registry, usersvc.Config{IsMaster: true}, &engine_xray.NoopEngine{}, nil, slog.Default())

	p := NewPlugin()
	p.log = slog.Default()
	p.registry = registry
	p.userSvc = userSvc
	p.authMiddleware = func(next http.Handler) http.Handler { return next }

	return p, registry, userSvc
}

func TestPlugin_Metadata(t *testing.T) {
	p := NewPlugin()
	meta := p.Metadata()
	require.Equal(t, "promo", meta.Name)
	require.Equal(t, "api", meta.Kind)
	require.Len(t, meta.Requires, 3)
}

func TestPlugin_LifecycleAndHealth(t *testing.T) {
	p := NewPlugin()
	require.Error(t, p.Health(context.Background()))

	_, reg, uSvc := setupTestPlugin(t)
	authMw := func(next http.Handler) http.Handler { return next }

	resolver := fakeResolver{
		services: map[string]any{
			pluginapi.ServiceDomainRegistry:        reg,
			pluginapi.ServiceUserManagement:        uSvc,
			pluginapi.ServiceProtectedMiddleware: authMw,
		},
	}

	require.NoError(t, p.Init(context.Background(), nil, resolver))
	require.NoError(t, p.Health(context.Background()))
	require.NoError(t, p.Stop(context.Background()))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	require.NoError(t, p.Start(ctx))
}

func TestPlugin_InitErrors(t *testing.T) {
	p := NewPlugin()
	_, reg, uSvc := setupTestPlugin(t)
	authMw := func(next http.Handler) http.Handler { return next }

	// Missing dependency
	err := p.Init(context.Background(), nil, fakeResolver{services: map[string]any{}})
	require.Error(t, err)

	// Resolver error
	err = p.Init(context.Background(), nil, fakeResolver{err: fmt.Errorf("fail")})
	require.Error(t, err)

	// Wrong user management type
	err = p.Init(context.Background(), nil, fakeResolver{
		services: map[string]any{
			pluginapi.ServiceDomainRegistry:        reg,
			pluginapi.ServiceUserManagement:        "wrong",
			pluginapi.ServiceProtectedMiddleware: authMw,
		},
	})
	require.Error(t, err)

	// Wrong middleware type
	err = p.Init(context.Background(), nil, fakeResolver{
		services: map[string]any{
			pluginapi.ServiceDomainRegistry:        reg,
			pluginapi.ServiceUserManagement:        uSvc,
			pluginapi.ServiceProtectedMiddleware: "wrong",
		},
	})
	require.Error(t, err)
}

func TestCreatePromo_Success(t *testing.T) {
	p, _, _ := setupTestPlugin(t)
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	body := []byte(`{
		"code": "summer20",
		"discount_percent": 20,
		"max_uses": 100,
		"target_platform": "telegram"
	}`)
	req := httptest.NewRequest("POST", "/api/v1/admin/promocodes", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code)

	var created domain.PromoCode
	err := json.Unmarshal(rec.Body.Bytes(), &created)
	require.NoError(t, err)
	require.Equal(t, "SUMMER20", created.Code)
	require.Equal(t, 20, created.DiscountPercent)
	require.Equal(t, "telegram", created.TargetPlatform)
	require.True(t, created.IsActive)
}

func TestCreatePromo_ValidationErrors(t *testing.T) {
	p, _, _ := setupTestPlugin(t)
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	// 1. Invalid JSON
	req := httptest.NewRequest("POST", "/api/v1/admin/promocodes", bytes.NewReader([]byte(`{bad json`)))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	// 2. Empty code
	req = httptest.NewRequest("POST", "/api/v1/admin/promocodes", bytes.NewReader([]byte(`{"code":"","discount_percent":20}`)))
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	// 3. Discount 0 or > 100
	req = httptest.NewRequest("POST", "/api/v1/admin/promocodes", bytes.NewReader([]byte(`{"code":"PROMO","discount_percent":0}`)))
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	req = httptest.NewRequest("POST", "/api/v1/admin/promocodes", bytes.NewReader([]byte(`{"code":"PROMO","discount_percent":105}`)))
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	// 4. Default platform is "all"
	req = httptest.NewRequest("POST", "/api/v1/admin/promocodes", bytes.NewReader([]byte(`{"code":"ALLPLATFORM","discount_percent":10}`)))
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code)
	var promo domain.PromoCode
	_ = json.Unmarshal(rec.Body.Bytes(), &promo)
	require.Equal(t, "all", promo.TargetPlatform)

	// 5. Duplicate code returns 409
	req = httptest.NewRequest("POST", "/api/v1/admin/promocodes", bytes.NewReader([]byte(`{"code":"ALLPLATFORM","discount_percent":10}`)))
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusConflict, rec.Code)
}

func TestListPromos(t *testing.T) {
	p, reg, _ := setupTestPlugin(t)
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	// Empty list
	req := httptest.NewRequest("GET", "/api/v1/admin/promocodes", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	// With records
	_ = reg.Promos().Create(context.Background(), &domain.PromoCode{Code: "P1", DiscountPercent: 10, TargetPlatform: "all", IsActive: true})
	_ = reg.Promos().Create(context.Background(), &domain.PromoCode{Code: "P2", DiscountPercent: 20, TargetPlatform: "all", IsActive: true})

	req = httptest.NewRequest("GET", "/api/v1/admin/promocodes", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var list []domain.PromoCode
	err := json.Unmarshal(rec.Body.Bytes(), &list)
	require.NoError(t, err)
	require.Len(t, list, 2)
}

func TestDeletePromo(t *testing.T) {
	p, reg, _ := setupTestPlugin(t)
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	promo := domain.PromoCode{Code: "TO_DELETE", DiscountPercent: 10, TargetPlatform: "all", IsActive: true}
	require.NoError(t, reg.Promos().Create(context.Background(), &promo))

	// 1. Success delete
	req := httptest.NewRequest("DELETE", fmt.Sprintf("/api/v1/admin/promocodes/%d", promo.ID), nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code)

	// 2. Not found delete
	req = httptest.NewRequest("DELETE", fmt.Sprintf("/api/v1/admin/promocodes/%d", promo.ID), nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code)

	// 3. Invalid ID
	req = httptest.NewRequest("DELETE", "/api/v1/admin/promocodes/abc", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestEditPromo(t *testing.T) {
	p, reg, _ := setupTestPlugin(t)
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	promo := domain.PromoCode{Code: "ORIGINAL", DiscountPercent: 10, TargetPlatform: "all", IsActive: true}
	require.NoError(t, reg.Promos().Create(context.Background(), &promo))

	// 1. Successful update
	body := []byte(`{"code":"UPDATED","discount_percent":25,"max_uses":50,"target_platform":"web"}`)
	req := httptest.NewRequest("PUT", fmt.Sprintf("/api/v1/admin/promocodes/%d", promo.ID), bytes.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	updated, err := reg.Promos().FindByID(context.Background(), promo.ID)
	require.NoError(t, err)
	require.Equal(t, "UPDATED", updated.Code)
	require.Equal(t, 25, updated.DiscountPercent)
	require.Equal(t, 50, updated.MaxUses)
	require.Equal(t, "web", updated.TargetPlatform)

	// 2. Not found
	req = httptest.NewRequest("PUT", "/api/v1/admin/promocodes/9999", bytes.NewReader(body))
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code)

	// 3. Invalid ID
	req = httptest.NewRequest("PUT", "/api/v1/admin/promocodes/invalid", bytes.NewReader(body))
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	// 4. Invalid body
	req = httptest.NewRequest("PUT", fmt.Sprintf("/api/v1/admin/promocodes/%d", promo.ID), bytes.NewReader([]byte(`{bad json`)))
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestValidatePromo(t *testing.T) {
	p, reg, _ := setupTestPlugin(t)
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	ctx := context.Background()

	// 1. Missing code / platform
	req := httptest.NewRequest("GET", "/api/v1/promocodes/validate?platform=telegram", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	req = httptest.NewRequest("GET", "/api/v1/promocodes/validate?code=TEST", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	// 2. Not found
	req = httptest.NewRequest("GET", "/api/v1/promocodes/validate?code=NOT_FOUND&platform=telegram", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code)

	// 3. Valid promo
	validPromo := domain.PromoCode{Code: "VALID20", DiscountPercent: 20, TargetPlatform: "all", IsActive: true}
	require.NoError(t, reg.Promos().Create(ctx, &validPromo))

	req = httptest.NewRequest("GET", "/api/v1/promocodes/validate?code=valid20&platform=telegram", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var valResp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &valResp)
	require.Equal(t, true, valResp["valid"])
	require.Equal(t, float64(20), valResp["discount_percent"])

	// 4. Inactive promo
	inactivePromo := domain.PromoCode{Code: "INACTIVE", DiscountPercent: 15, TargetPlatform: "all", IsActive: true}
	require.NoError(t, reg.Promos().Create(ctx, &inactivePromo))
	inactivePromo.IsActive = false
	require.NoError(t, reg.Promos().Update(ctx, &inactivePromo))

	req = httptest.NewRequest("GET", "/api/v1/promocodes/validate?code=INACTIVE&platform=telegram", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	// 5. Expired promo
	past := time.Now().Add(-1 * time.Hour)
	expiredPromo := domain.PromoCode{Code: "EXPIRED", DiscountPercent: 15, TargetPlatform: "all", IsActive: true, ExpiresAt: &past}
	require.NoError(t, reg.Promos().Create(ctx, &expiredPromo))

	req = httptest.NewRequest("GET", "/api/v1/promocodes/validate?code=EXPIRED&platform=telegram", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	// 6. Platform mismatch
	webOnly := domain.PromoCode{Code: "WEBONLY", DiscountPercent: 30, TargetPlatform: "web", IsActive: true}
	require.NoError(t, reg.Promos().Create(ctx, &webOnly))

	req = httptest.NewRequest("GET", "/api/v1/promocodes/validate?code=WEBONLY&platform=telegram", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	// 7. Usage limit reached
	limitPromo := domain.PromoCode{Code: "LIMITED", DiscountPercent: 10, MaxUses: 2, UsesCount: 2, TargetPlatform: "all", IsActive: true}
	require.NoError(t, reg.Promos().Create(ctx, &limitPromo))

	req = httptest.NewRequest("GET", "/api/v1/promocodes/validate?code=LIMITED&platform=telegram", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	// 8. User already used check
	user := &domain.User{ID: "promo-user", Username: "promouser", RefCode: "PROMO_REF", Metadata: domain.Metadata{"telegram_id": int64(12345)}}
	require.NoError(t, reg.Users().Create(ctx, user))

	userPromo := domain.PromoCode{Code: "ONCEUSER", DiscountPercent: 50, TargetPlatform: "all", IsActive: true}
	require.NoError(t, reg.Promos().Create(ctx, &userPromo))

	// First: not used yet
	req = httptest.NewRequest("GET", "/api/v1/promocodes/validate?code=ONCEUSER&platform=telegram&telegram_id=12345", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	// User completes a payment with this promo
	pay := &domain.Payment{UserID: user.ID, Amount: 100, Status: "completed", PaymentType: "subscription", PromoCodeID: &userPromo.ID}
	require.NoError(t, reg.Payments().Create(ctx, pay))

	// Second: already used
	req = httptest.NewRequest("GET", "/api/v1/promocodes/validate?code=ONCEUSER&platform=telegram&telegram_id=12345", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}
