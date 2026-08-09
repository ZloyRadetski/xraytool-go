package mailer_resend

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	json "github.com/goccy/go-json"
)

// Mailer describes any email sender.
type Mailer interface {
	SendCode(toEmail, code string) error
}

// ResendMailer sends transactional emails via the Resend API.
type ResendMailer struct {
	apiKey    string
	fromEmail string
	baseURL   string // overridable for tests
	client    *http.Client
	log       *slog.Logger
}

// New creates a ResendMailer ready to send emails.
func NewResendMailer(apiKey, fromEmail string) *ResendMailer {
	return &ResendMailer{
		apiKey:    apiKey,
		fromEmail: fromEmail,
		baseURL:   "https://api.resend.com",
		client:    &http.Client{Timeout: 10 * time.Second},
		log:       slog.Default(),
	}
}

// SendCode delivers a six-digit verification code. The HTML and plain-text
// variants carry the same security information so the message remains useful
// with remote images, external fonts, and CSS disabled.
func (m *ResendMailer) SendCode(toEmail, code string) error {
	body := map[string]interface{}{
		"from":    m.fromEmail,
		"to":      []string{toEmail},
		"subject": "Код для входа — Torvalds VPN",
		"html":    buildHTML(code),
		"text":    buildText(code),
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("mailer: marshal payload: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, m.baseURL+"/emails", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("mailer: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+m.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := m.client.Do(req)
	if err != nil {
		return fmt.Errorf("mailer: send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("mailer: resend returned status %d", resp.StatusCode)
	}

	m.log.Info("mailer: code sent", "to", toEmail)
	return nil
}
