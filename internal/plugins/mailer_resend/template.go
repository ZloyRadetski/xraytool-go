package mailer_resend

import html "html"

// buildHTML returns an email-client-safe rendition of the website's
// neo-brutalist design: pale indigo background, sharp black borders, and
// yellow, cyan, and pink accents. Tables and inline styles deliberately take
// precedence over modern layout primitives for Outlook and Gmail support.
func buildHTML(code string) string {
	escapedCode := html.EscapeString(code)
	return `<!doctype html>
<html lang="ru">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta name="x-apple-disable-message-reformatting">
  <meta name="color-scheme" content="light">
  <title>Код для входа — Torvalds VPN</title>
  <style>
    @media screen and (max-width: 620px) {
      .email-shell { width: 100% !important; }
      .email-padding { padding: 20px 14px 30px !important; }
      .content-padding { padding: 28px 22px 24px !important; }
      .headline { font-size: 28px !important; line-height: 32px !important; }
      .code { font-size: 40px !important; letter-spacing: 8px !important; }
    }
  </style>
</head>
<body style="margin:0; padding:0; background:#e0e7ff; color:#000000; font-family:Arial, Helvetica, sans-serif;">
  <div style="display:none; max-height:0; overflow:hidden; opacity:0; mso-hide:all;">Ваш код для входа в Torvalds VPN: ` + escapedCode + `</div>
  <table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="width:100%; background:#e0e7ff;">
    <tr>
      <td class="email-padding" align="center" style="padding:40px 16px 48px;">
        <table class="email-shell" role="presentation" width="600" cellpadding="0" cellspacing="0" border="0" style="width:600px; max-width:600px;">
          <tr>
            <td style="padding:0 0 16px;">
              <table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="border:4px solid #000000; background:#ffffff;">
                <tr>
                  <td width="68" align="center" valign="middle" style="width:68px; background:#ffde00; border-right:4px solid #000000; padding:15px 0; font-size:30px; font-weight:900; line-height:30px;">T</td>
                  <td valign="middle" style="padding:13px 18px 12px;">
                    <div style="font-size:21px; font-weight:900; letter-spacing:-0.6px; line-height:24px; text-transform:uppercase;">TORVALDS VPN</div>
                    <div style="padding-top:3px; font-size:10px; font-weight:800; letter-spacing:1.8px; line-height:14px; text-transform:uppercase;">Secure access / личный кабинет</div>
                  </td>
                  <td width="126" align="center" valign="middle" style="width:126px; background:#00f0ff; border-left:4px solid #000000; padding:12px 8px; font-size:10px; font-weight:900; letter-spacing:1px; line-height:14px; text-transform:uppercase;">Код<br>доступа</td>
                </tr>
              </table>
            </td>
          </tr>
          <tr>
            <td style="border:4px solid #000000; border-bottom:10px solid #000000; background:#ffffff; box-shadow:8px 8px 0 #000000;">
              <table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0">
                <tr>
                  <td class="content-padding" style="padding:38px 38px 30px;">
                    <div style="display:inline-block; margin:0 0 18px; padding:7px 10px 6px; border:3px solid #000000; background:#ff4982; color:#ffffff; font-size:11px; font-weight:900; letter-spacing:1.2px; line-height:13px; text-transform:uppercase;">Подтверждение входа</div>
                    <div class="headline" style="margin:0; font-size:34px; font-weight:900; letter-spacing:-1.3px; line-height:37px; text-transform:uppercase;">Введите код<br>на сайте</div>
                    <p style="margin:16px 0 25px; font-size:16px; line-height:24px;">Мы получили запрос на вход в ваш аккаунт. Введите этот код в форме авторизации для получения доступа к аккаунту на сайте.</p>

                    <table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="margin:0 0 22px; border:4px solid #000000; background:#ffde00;">
                      <tr>
                        <td align="center" style="padding:10px 12px 0; font-size:10px; font-weight:900; letter-spacing:1.6px; line-height:14px; text-transform:uppercase;">Ваш одноразовый код</td>
                      </tr>
                      <tr>
                        <td class="code" align="center" style="padding:9px 14px 21px; font-family:'Courier New', Courier, monospace; font-size:52px; font-weight:700; letter-spacing:13px; line-height:56px; white-space:nowrap;">` + escapedCode + `</td>
                      </tr>
                    </table>

                    <table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="margin:0 0 18px; border:3px solid #000000; background:#00f0ff;">
                      <tr>
                        <td width="42" align="center" valign="top" style="width:42px; padding:12px 0 10px; font-size:18px; line-height:18px;">↗</td>
                        <td style="padding:10px 14px 11px 0; font-size:14px; font-weight:800; line-height:20px;">Код действует <strong>5 минут</strong>. Если он истечёт, запросите новый на странице входа.</td>
                      </tr>
                    </table>

                    <table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="border-top:3px solid #000000;">
                      <tr>
                        <td style="padding:18px 0 0; font-size:13px; line-height:20px;">
                          <strong>Не вы запрашивали код?</strong><br>
                          Просто проигнорируйте письмо. Никому не сообщайте код — поддержка Torvalds VPN никогда не просит его назвать.
                        </td>
                      </tr>
                    </table>
                  </td>
                </tr>
              </table>
            </td>
          </tr>
          <tr>
            <td align="center" style="padding:27px 20px 0; color:#303030; font-size:11px; font-weight:700; line-height:17px;">
              <div style="font-weight:900; letter-spacing:1.3px; text-transform:uppercase;">TORVALDS VPN · secure access</div>
              <div style="padding-top:4px;">Автоматическое письмо — отвечать на него не нужно.</div>
            </td>
          </tr>
        </table>
      </td>
    </tr>
  </table>
</body>
</html>`
}

func buildText(code string) string {
	return "TORVALDS VPN\n\n" +
		"Код для входа: " + code + "\n\n" +
		"Введите его на странице входа. Код действует 5 минут.\n\n" +
		"Не сообщайте код никому: поддержка Torvalds VPN никогда его не запрашивает.\n"
}
