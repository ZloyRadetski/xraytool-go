package mailer_resend

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBuildHTML(t *testing.T) {
	t.Parallel()

	for _, code := range []string{"123456", "000000", "987654"} {
		code := code
		t.Run(code, func(t *testing.T) {
			t.Parallel()

			html := buildHTML(code)
			if html == "" {
				t.Fatal("buildHTML returned an empty message")
			}
			if !strings.Contains(html, code) {
				t.Errorf("buildHTML does not contain code %q", code)
			}
			for _, expected := range []string{
				"TORVALDS VPN",
				"5 минут",
				"Подтверждение входа",
				"#e0e7ff",
				"#ffde00",
				"#00f0ff",
			} {
				if !strings.Contains(html, expected) {
					t.Errorf("buildHTML does not contain %q", expected)
				}
			}
		})
	}
}

func TestBuildText(t *testing.T) {
	text := buildText("123456")
	if !strings.Contains(text, "123456") || !strings.Contains(text, "5 минут") {
		t.Fatalf("plain-text message is incomplete: %q", text)
	}
}

func TestSendCode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		statusCode int
		wantErr    bool
	}{
		{name: "success 200", statusCode: http.StatusOK},
		{name: "success 201", statusCode: http.StatusCreated},
		{name: "unauthorized 401", statusCode: http.StatusUnauthorized, wantErr: true},
		{name: "server error 500", statusCode: http.StatusInternalServerError, wantErr: true},
		{name: "boundary 300", statusCode: http.StatusMultipleChoices, wantErr: true},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer test-key" {
					t.Errorf("unexpected Authorization header: %q", r.Header.Get("Authorization"))
				}
				if r.Header.Get("Content-Type") != "application/json" {
					t.Errorf("unexpected Content-Type: %s", r.Header.Get("Content-Type"))
				}

				var payload struct {
					Subject string `json:"subject"`
					HTML    string `json:"html"`
					Text    string `json:"text"`
				}
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Errorf("decode email payload: %v", err)
				}
				if payload.Subject != "Код для входа — Torvalds VPN" {
					t.Errorf("unexpected subject: %q", payload.Subject)
				}
				if !strings.Contains(payload.HTML, "123456") || !strings.Contains(payload.Text, "123456") {
					t.Error("email payload must carry the verification code in HTML and plain text")
				}
				w.WriteHeader(tc.statusCode)
			}))
			defer srv.Close()

			mailer := NewResendMailer("test-key", "noreply@torvalds.vpn")
			mailer.baseURL = srv.URL
			err := mailer.SendCode("user@example.com", "123456")
			if (err != nil) != tc.wantErr {
				t.Errorf("SendCode() error = %v, wantErr %t", err, tc.wantErr)
			}
		})
	}
}

func TestSendCodeNetworkError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close()

	mailer := NewResendMailer("test-key", "noreply@torvalds.vpn")
	mailer.baseURL = srv.URL
	if err := mailer.SendCode("user@example.com", "654321"); err == nil || !strings.Contains(err.Error(), "mailer:") {
		t.Errorf("SendCode() error = %v, want a wrapped transport error", err)
	}
}
