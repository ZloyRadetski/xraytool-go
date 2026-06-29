package mailer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"
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
func New(apiKey, fromEmail string) *ResendMailer {
	return &ResendMailer{
		apiKey:    apiKey,
		fromEmail: fromEmail,
		baseURL:   "https://api.resend.com",
		client:    &http.Client{Timeout: 10 * time.Second},
		log:       slog.Default(),
	}
}

// SendCode delivers a 6-digit OTP code to toEmail via the Resend API.
// It builds a neo-brutalism styled HTML email matching the Torvalds VPN brand.
func (m *ResendMailer) SendCode(toEmail, code string) error {
	body := map[string]interface{}{
		"from":    m.fromEmail,
		"to":      []string{toEmail},
		"subject": "Ваш код входа — Torvalds VPN",
		"html":    buildHTML(code),
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

// buildHTML returns a neo-brutalism styled HTML email with the OTP code.
// Design: dark background (#0d0d0d), bold yellow (#f5d800) accents, thick black borders,
// monospace code block, Torvalds VPN branding. Kept well under 50 KB.
func buildHTML(code string) string {
	return `<!DOCTYPE html>
<html lang="ru">
<head>
  <meta charset="UTF-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1.0" />
  <title>Ваш код входа — Torvalds VPN</title>
  <style>
    @import url('https://fonts.googleapis.com/css2?family=Space+Grotesk:wght@400;700;900&family=Space+Mono:wght@400;700&display=swap');

    * { margin: 0; padding: 0; box-sizing: border-box; }

    body {
      background-color: #0d0d0d;
      font-family: 'Space Grotesk', Arial, sans-serif;
      color: #f0f0f0;
      padding: 32px 16px;
    }

    .wrapper {
      max-width: 560px;
      margin: 0 auto;
    }

    /* ── Header ── */
    .header {
      background-color: #f5d800;
      border: 4px solid #000;
      box-shadow: 6px 6px 0 #000;
      padding: 20px 28px;
      margin-bottom: 24px;
      display: flex;
      align-items: center;
      gap: 14px;
    }

    .logo-icon {
      width: 44px;
      height: 44px;
      background: #000;
      border: 3px solid #000;
      border-radius: 4px;
      display: flex;
      align-items: center;
      justify-content: center;
      flex-shrink: 0;
    }

    .logo-icon svg { display: block; }

    .logo-text {
      font-size: 22px;
      font-weight: 900;
      color: #000;
      letter-spacing: -0.5px;
      line-height: 1.1;
    }

    .logo-sub {
      font-size: 11px;
      font-weight: 700;
      color: #000;
      letter-spacing: 2px;
      text-transform: uppercase;
      opacity: 0.7;
    }

    /* ── Card ── */
    .card {
      background-color: #1a1a1a;
      border: 4px solid #f5d800;
      box-shadow: 8px 8px 0 #f5d800;
      padding: 36px 32px;
      margin-bottom: 20px;
    }

    .card-title {
      font-size: 13px;
      font-weight: 700;
      letter-spacing: 3px;
      text-transform: uppercase;
      color: #f5d800;
      margin-bottom: 10px;
    }

    .card-headline {
      font-size: 26px;
      font-weight: 900;
      color: #f0f0f0;
      margin-bottom: 28px;
      line-height: 1.25;
    }

    /* ── Code Block ── */
    .code-block {
      background-color: #0d0d0d;
      border: 4px solid #f0f0f0;
      box-shadow: 5px 5px 0 #f5d800;
      padding: 24px 16px;
      text-align: center;
      margin-bottom: 24px;
    }

    .code-label {
      font-family: 'Space Mono', monospace;
      font-size: 11px;
      font-weight: 700;
      letter-spacing: 3px;
      text-transform: uppercase;
      color: #666;
      margin-bottom: 12px;
    }

    .code-value {
      font-family: 'Space Mono', 'Courier New', monospace;
      font-size: 52px;
      font-weight: 700;
      letter-spacing: 14px;
      color: #f5d800;
      line-height: 1;
    }

    /* ── Expiry Notice ── */
    .expiry-bar {
      background-color: #2a1f00;
      border: 3px solid #f5d800;
      padding: 12px 16px;
      display: flex;
      align-items: center;
      gap: 10px;
      margin-bottom: 24px;
    }

    .expiry-icon {
      font-size: 18px;
      flex-shrink: 0;
    }

    .expiry-text {
      font-size: 13px;
      font-weight: 700;
      color: #f5d800;
    }

    .expiry-text span {
      color: #f0c000;
      font-weight: 900;
    }

    /* ── Security Note ── */
    .security-note {
      font-size: 12px;
      color: #555;
      line-height: 1.6;
      border-top: 2px solid #2a2a2a;
      padding-top: 18px;
    }

    /* ── Footer ── */
    .footer {
      text-align: center;
      font-size: 11px;
      color: #3a3a3a;
      font-family: 'Space Mono', monospace;
      letter-spacing: 1px;
      line-height: 1.8;
    }

    .footer a {
      color: #f5d800;
      text-decoration: none;
    }
  </style>
</head>
<body>
  <div class="wrapper">

    <!-- Header / Brand -->
    <div class="header">
      <div class="logo-icon">
        <svg width="26" height="26" viewBox="0 0 26 26" fill="none" xmlns="http://www.w3.org/2000/svg">
          <rect x="1" y="1" width="24" height="24" rx="2" stroke="#f5d800" stroke-width="2"/>
          <path d="M7 13 L13 7 L19 13" stroke="#f5d800" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"/>
          <path d="M13 7 L13 20" stroke="#f5d800" stroke-width="2.5" stroke-linecap="round"/>
        </svg>
      </div>
      <div>
        <div class="logo-text">TORVALDS VPN</div>
        <div class="logo-sub">Secure Access</div>
      </div>
    </div>

    <!-- Main Card -->
    <div class="card">
      <div class="card-title">// Одноразовый код</div>
      <div class="card-headline">Введите этот код<br/>для входа в аккаунт</div>

      <!-- OTP Code Box -->
      <div class="code-block">
        <div class="code-label">// your_otp_code</div>
        <div class="code-value">` + code + `</div>
      </div>

      <!-- Expiry -->
      <div class="expiry-bar">
        <div class="expiry-icon">⏱</div>
        <div class="expiry-text">
          Код действителен <span>5 минут</span>. После истечения запросите новый.
        </div>
      </div>

      <!-- Security Note -->
      <div class="security-note">
        Если вы не запрашивали этот код — просто проигнорируйте письмо.
        Никому не сообщайте код, включая сотрудников поддержки.
        Мы никогда не запрашиваем коды самостоятельно.
      </div>
    </div>

    <!-- Footer -->
    <div class="footer">
      <div>© 2026 TORVALDS VPN — ALL RIGHTS RESERVED</div>
      <div>Это автоматическое письмо, не отвечайте на него.</div>
    </div>

  </div>
</body>
</html>`
}
