package server_test

import (
	"xraytool/internal/domain"
	"xraytool/internal/plugins/core/payment"
	"xraytool/internal/pluginapi"
	"xraytool/internal/plugins/core/user"

	"gorm.io/gorm"

	"bytes"
	json "github.com/goccy/go-json"
	"log/slog"
	"net/http/httptest"
	"os"
	"testing"

	"xraytool/internal/appconfig"
	"xraytool/internal/database"
	"xraytool/internal/events"
	"xraytool/internal/plugins/core/server"
	"xraytool/internal/plugins/core/subscription"
	vpn "xraytool/internal/plugins/engine_xray"
)

var (
	testDB  *gorm.DB
	testReg domain.Registry
)

func TestMain(m *testing.M) {
	db, _ := database.NewConnection(database.Config{Driver: "sqlite", SQLitePath: ":memory:", Silent: true, AutoMigrate: true})
	testDB = db
	testReg = database.NewRegistry(db)
	os.Exit(m.Run())
}

func newTestRouter(t *testing.T) *server.Router {
	t.Helper()

	f, _ := os.CreateTemp("", "xrayconfig_*.json")
	f.WriteString(`{"inbounds":[]}`) //nolint:errcheck
	f.Close()
	t.Cleanup(func() { os.Remove(f.Name()) })

	cfg := &appconfig.Config{
		Server:            appconfig.ServerConf{Domain: "test.example.com"},

		PlategaMerchantID: "test-platega-merchant-id",
		PlategaSecret:     "test-platega-secret",
		Paths: appconfig.PathsConf{
			XrayConfig: f.Name(),
		},
	}
	engine := &vpn.NoopEngine{}
	cm := subscription.NewCacheManager(cfg, engine)
	dispatcher := events.NewDispatcher(&events.Config{})
	userSvc := user.NewService(testReg, user.Config{IsMaster: true, Domain: cfg.Server.Domain}, engine, nil, slog.Default())
	paymentSvc := payment.NewService(testReg, dispatcher, slog.Default())

	return server.New(cfg, "test-api-key", cm, engine, userSvc, paymentSvc, dispatcher, slog.Default()).WithPaymentProviders(map[string]pluginapi.PaymentProvider{
		"platega": newTestPaymentProvider(),
	})
}

func do(r *server.Router, method, path, body string, apiKey string) *httptest.ResponseRecorder {
	var bodyReader *bytes.Reader
	if body != "" {
		bodyReader = bytes.NewReader([]byte(body))
	} else {
		bodyReader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, bodyReader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func doAuth(r *server.Router, method, path, body string) *httptest.ResponseRecorder {
	return do(r, method, path, body, "test-api-key")
}

func jsonBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("invalid JSON response: %s", rec.Body.String())
	}
	return m
}
