package server

import (
	"bytes"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIntruderLogDoesNotContainRequestSecrets(t *testing.T) {
	var output bytes.Buffer
	router := &Router{
		log: slog.New(slog.NewTextHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	req := httptest.NewRequest("GET", "https://example.test/private?token=subscription-secret&uuid=00000000-0000-0000-0000-000000000001", nil)
	req.Header.Set("Authorization", "Bearer authorization-secret")
	req.Header.Set("Cookie", "session=cookie-secret")
	req.Header.Set("X-API-Key", "api-key-secret")
	req.Header.Set("X-Secret", "x-secret-value")

	router.logIntruder(req, "invalid credential")

	got := output.String()
	for _, secret := range []string{
		"subscription-secret",
		"00000000-0000-0000-0000-000000000001",
		"authorization-secret",
		"cookie-secret",
		"api-key-secret",
		"x-secret-value",
	} {
		if strings.Contains(got, secret) {
			t.Fatalf("intruder log leaked %q: %s", secret, got)
		}
	}
	if !strings.Contains(got, "method=GET") || !strings.Contains(got, "path=/private") {
		t.Fatalf("intruder log lost non-sensitive diagnostic fields: %s", got)
	}
}
