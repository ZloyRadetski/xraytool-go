package mailer

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestBuildHTML verifies buildHTML returns a non-empty string containing the OTP code.
func TestBuildHTML(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		code string
	}{
		{"six digit code", "123456"},
		{"all zeros", "000000"},
		{"mixed digits", "987654"},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			html := buildHTML(tc.code)

			if html == "" {
				t.Fatal("buildHTML returned empty string")
			}
			if !strings.Contains(html, tc.code) {
				t.Errorf("buildHTML output does not contain code %q", tc.code)
			}
			if !strings.Contains(html, "TORVALDS VPN") {
				t.Error("buildHTML output does not contain brand name")
			}
			if !strings.Contains(html, "5 минут") {
				t.Error("buildHTML output does not contain expiry notice")
			}
		})
	}
}

// TestSendCode exercises the HTTP layer using an httptest.Server.
func TestSendCode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		statusCode int
		wantErr    bool
	}{
		{
			name:       "success 200",
			statusCode: http.StatusOK,
			wantErr:    false,
		},
		{
			name:       "success 201",
			statusCode: http.StatusCreated,
			wantErr:    false,
		},
		{
			name:       "unauthorized 401",
			statusCode: http.StatusUnauthorized,
			wantErr:    true,
		},
		{
			name:       "server error 500",
			statusCode: http.StatusInternalServerError,
			wantErr:    true,
		},
		{
			name:       "boundary 300",
			statusCode: http.StatusMultipleChoices,
			wantErr:    true,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Verify mandatory headers are present.
				if r.Header.Get("Authorization") == "" {
					t.Error("Authorization header missing")
				}
				if r.Header.Get("Content-Type") != "application/json" {
					t.Errorf("unexpected Content-Type: %s", r.Header.Get("Content-Type"))
				}
				w.WriteHeader(tc.statusCode)
			}))
			defer srv.Close()

			m := New("test-key", "noreply@torvalds.vpn")
			m.baseURL = srv.URL // redirect to test server

			err := m.SendCode("user@example.com", "123456")
			if tc.wantErr && err == nil {
				t.Errorf("expected error for status %d, got nil", tc.statusCode)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("expected no error for status %d, got: %v", tc.statusCode, err)
			}
		})
	}
}

// TestSendCode_NetworkError checks that a connection failure returns a wrapped error.
func TestSendCode_NetworkError(t *testing.T) {
	t.Parallel()

	// Point to a server that immediately closes (simulate unavailable API).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close() // close immediately so connection is refused

	m := New("test-key", "noreply@torvalds.vpn")
	m.baseURL = srv.URL

	err := m.SendCode("user@example.com", "654321")
	if err == nil {
		t.Fatal("expected error when API is unavailable, got nil")
	}
	if !strings.Contains(err.Error(), "mailer:") {
		t.Errorf("error should be prefixed with 'mailer:', got: %v", err)
	}
}
