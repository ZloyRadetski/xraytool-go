package server_test

import (
	"net/http"
	"testing"
)

func TestRouter_CatchAll_404(t *testing.T) {
	r := newTestRouter(t)
	w := do(r, "GET", "/nonexistent/route", "", "test-api-key")
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestRouter_AuthMiddleware_MissingKey(t *testing.T) {
	r := newTestRouter(t)
	w := do(r, "POST", "/api/v1/users/register", "", "")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestRouter_AuthMiddleware_WrongKey(t *testing.T) {
	r := newTestRouter(t)
	w := do(r, "POST", "/api/v1/users/register", "", "wrong-key")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestRouter_PublicRoute_NoAuth(t *testing.T) {
	r := newTestRouter(t)
	w := do(r, "GET", "/client", "", "")
	// Sub is stubbed as 501, not 404. Meaning it passed auth middleware.
	if w.Code == http.StatusNotFound {
		t.Errorf("expected not 404 for public route")
	}
}

func TestRouter_ConfigStorageRoutesAreNotOwnedByAPI(t *testing.T) {
	r := newTestRouter(t)
	w := do(r, http.MethodGet, "/api/rest/download", "", "test-api-key")
	if w.Code != http.StatusNotFound {
		t.Errorf("expected config-storage route to be absent from api_server, got %d", w.Code)
	}
}
