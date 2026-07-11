package server

import (
	"bytes"
	"compress/gzip"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newGzipTestRouter builds a minimal Router for middleware unit-testing.
func newGzipTestRouter() *Router {
	return &Router{
		mux:    http.NewServeMux(),
		apiKey: "test-key",
		log:    slog.Default(),
	}
}

// echoJSONHandler returns a handler that writes a fixed JSON body.
func echoJSONHandler(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
	}
}

// TestGzipMiddleware_CompressesResponse verifies that when the client sends
// Accept-Encoding: gzip, the server returns a gzip-compressed body.
func TestGzipMiddleware_CompressesResponse(t *testing.T) {
	r := newGzipTestRouter()
	payload := `{"users":["alice","bob","charlie"]}`

	handler := r.gzipMiddleware(echoJSONHandler(payload))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("expected Content-Encoding: gzip, got %q", rec.Header().Get("Content-Encoding"))
	}
	if rec.Header().Get("Vary") != "Accept-Encoding" {
		t.Fatalf("expected Vary: Accept-Encoding, got %q", rec.Header().Get("Vary"))
	}

	// Decompress and verify body matches original.
	gr, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("failed to create gzip reader: %v", err)
	}
	defer gr.Close()
	got, err := io.ReadAll(gr)
	if err != nil {
		t.Fatalf("failed to read decompressed body: %v", err)
	}
	if strings.TrimSpace(string(got)) != payload {
		t.Fatalf("body mismatch: got %q, want %q", got, payload)
	}
}

// TestGzipMiddleware_NoCompressionWithoutHeader verifies that when the client
// does NOT send Accept-Encoding: gzip, the response is NOT compressed.
func TestGzipMiddleware_NoCompressionWithoutHeader(t *testing.T) {
	r := newGzipTestRouter()
	payload := `{"ok":true}`

	handler := r.gzipMiddleware(echoJSONHandler(payload))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	// No Accept-Encoding header.
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if enc := rec.Header().Get("Content-Encoding"); enc != "" {
		t.Fatalf("expected no Content-Encoding, got %q", enc)
	}
	if strings.TrimSpace(rec.Body.String()) != payload {
		t.Fatalf("body mismatch: got %q, want %q", rec.Body.String(), payload)
	}
}

// TestGzipMiddleware_DecompressesRequest verifies that when the client sends a
// gzip-compressed request body, the middleware decompresses it before the
// handler reads it.
func TestGzipMiddleware_DecompressesRequest(t *testing.T) {
	r := newGzipTestRouter()
	expectedBody := `{"action":"sync-delta","payload":"hello"}`

	// Gzip-compress the request body.
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	_, _ = gw.Write([]byte(expectedBody))
	_ = gw.Close()

	// Handler that reads the (decompressed) body and echoes it back.
	readBodyHandler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		got, err := io.ReadAll(req.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(got)
	})

	handler := r.gzipMiddleware(readBodyHandler)

	req := httptest.NewRequest(http.MethodPost, "/test", &buf)
	req.Header.Set("Content-Encoding", "gzip")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if strings.TrimSpace(rec.Body.String()) != expectedBody {
		t.Fatalf("body mismatch: got %q, want %q", rec.Body.String(), expectedBody)
	}
	// The Content-Encoding header must be stripped so handlers don't see it.
	if enc := req.Header.Get("Content-Encoding"); enc != "" {
		t.Fatalf("expected Content-Encoding to be stripped from request, still has %q", enc)
	}
}

// TestGzipMiddleware_BadGzipBody verifies that a malformed gzip request body
// returns 400 Bad Request.
func TestGzipMiddleware_BadGzipBody(t *testing.T) {
	r := newGzipTestRouter()

	handler := r.gzipMiddleware(echoJSONHandler("should not reach"))

	req := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader("not-gzip-data"))
	req.Header.Set("Content-Encoding", "gzip")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad gzip body, got %d", rec.Code)
	}
}
