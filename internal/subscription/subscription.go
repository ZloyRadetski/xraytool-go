package subscription

import (
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"xraytool/internal/vpn"
)

// Package-level compiled regexes (avoids recompilation on every call).
var (
	validClientIDRe = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)
	validDateRe     = regexp.MustCompile(`^([0-9]{1,2})[-.]([0-9]{1,2})[-.]([0-9]{4})$`)
)

// Request is the input metadata received from PHP proxy.
type Request struct {
	RemoteAddr string            `json:"remote_addr"`
	UserAgent  string            `json:"user_agent"`
	Query      map[string]string `json:"query"`
	Headers    map[string]string `json:"headers"`
}

// Response is the output containing HTTP headers and body sent back to PHP.
type Response struct {
	StatusCode  int               `json:"status_code"`
	Headers     map[string]string `json:"headers"`
	Body        string            `json:"body"`
	ErrorReason string            `json:"-"` // Internal use for logging
	SubID       string            `json:"-"` // Extracted subfile/ID
}

// ActiveUser represents merged client data from active config.
type ActiveUser struct {
	Email    string
	ID       string
	Subfile  string
	Password string
	Expire   string
	Hy2Auth  string
	Hy2Obfs  string
	Limit    int
}

// generateDummyVless converts an array of custom strings into VLESS dummy links.
func generateDummyVless(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	var out []string
	port := 443
	for _, l := range lines {
		if l == "" {
			out = append(out, "")
			continue
		}
		// The `#` fragment in VLESS links is the name/remark of the proxy node.
		// It MUST be url-encoded, otherwise Xray clients will drop the string at the first space or corrupt it.
		// Note: url.PathEscape uses %20 for spaces, which is preferred over %2B (+) by most clients.
		encodedName := strings.ReplaceAll(url.QueryEscape(l), "+", "%20")
		// Use unique ports so the client doesn't deduplicate or overwrite nodes with the same address/UUID
		out = append(out, fmt.Sprintf("vless://00000000-0000-0000-0000-000000000000@127.0.0.1:%d?type=tcp&security=none#%s", port, encodedName))
		port++
	}
	return strings.Join(out, "\n")
}

// ---------------------------------------------------------------------------
// Helper functions
// ---------------------------------------------------------------------------

func failResponse(code int, msg string) *Response {
	return &Response{
		StatusCode: code,
		Headers: map[string]string{
			"Content-Type":    "text/plain; charset=utf-8",
			"X-Reject-Reason": msg,
		},
		Body:        msg,
		ErrorReason: msg,
	}
}

func normalizeSubfileToID(s string) string {
	s = strings.TrimSpace(s)
	wsReplacer := strings.NewReplacer(" ", "", "\t", "", "\n", "", "\r", "")
	s = wsReplacer.Replace(s)
	if strings.HasSuffix(strings.ToLower(s), ".txt") {
		s = s[:len(s)-4]
	}
	var sb strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			sb.WriteRune(r)
		}
	}
	return strings.ToLower(sb.String())
}

func ResolveClientID(req *Request) (string, string) {
	if id, ok := req.Query["id"]; ok && id != "" {
		id = normalizeClientIDValue(id)
		if id != "" {
			return id, id + ".txt"
		}
	}
	if file, ok := req.Query["file"]; ok && file != "" {
		file = normalizeClientIDValue(file)
		if file != "" {
			return file, file + ".txt"
		}
	}

	// Check path formats from request headers if available or URI
	// Since we pass Request URI or path from sub.php
	uriPath := req.Headers["x-request-path"]
	if uriPath == "" {
		uriPath = req.Query["request_path"]
	}
	if uriPath != "" {
		if strings.HasPrefix(uriPath, "/client&id=") {
			id := normalizeClientIDValue(uriPath[11:])
			if id != "" {
				return id, id + ".txt"
			}
		}
		if strings.HasPrefix(uriPath, "/client/") {
			id := normalizeClientIDValue(uriPath[8:])
			if id != "" {
				return id, id + ".txt"
			}
		}
		candidate := filepath.Base(uriPath)
		id := normalizeClientIDValue(candidate)
		if id != "" {
			return id, id + ".txt"
		}
	}

	return "", ""
}

func normalizeClientIDValue(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// Strip URL decode logic
	if strings.HasPrefix(strings.ToLower(s), "id=") {
		s = s[3:]
	}
	if strings.HasSuffix(strings.ToLower(s), ".txt") {
		s = s[:len(s)-4]
	}
	// Verify characters
	if !validClientIDRe.MatchString(s) {
		return ""
	}
	return s
}

func userAgentHasToken(uaLower, token string) bool {
	if uaLower == "" || token == "" {
		return false
	}
	idx := strings.Index(uaLower, token)
	if idx < 0 {
		return false
	}
	// Make sure it is isolated (not part of another word)
	before := true
	if idx > 0 {
		r := uaLower[idx-1]
		before = (r < 'a' || r > 'z') && (r < '0' || r > '9')
	}
	after := true
	if idx+len(token) < len(uaLower) {
		r := uaLower[idx+len(token)]
		after = (r < 'a' || r > 'z') && (r < '0' || r > '9')
	}
	return before && after
}

func findActiveUserBySubfile(cfg vpn.RawConfig, filename string, defaultExpire string) *ActiveUser {
	targetNorm := normalizeSubfileToID(filename)
	if targetNorm == "" {
		return nil
	}

	inbounds, err := cfg.GetInbounds()
	if err != nil {
		return nil
	}

	var best *ActiveUser
	for _, ib := range inbounds {
		clients, err := ib.GetClients()
		if err != nil {
			continue
		}
		for _, c := range clients {
			sub := c.GetString("subfile")
			if sub == "" {
				continue
			}
			if normalizeSubfileToID(sub) == targetNorm {
				email := c.Email()
				if email == "" {
					continue
				}

				limitVal := 3
				if lv, ok := c.GetNumber("limit"); ok && lv > 0 {
					limitVal = int(lv)
				}

				hy2Auth := c.GetString("auth")

				row := &ActiveUser{
					Email:    email,
					ID:       c.GetString("id"),
					Subfile:  sub,
					Password: c.GetString("password"),
					Expire:   c.GetString("expire"),
					Hy2Auth:  hy2Auth,
					Hy2Obfs:  c.GetString("hy2_obfs"),
					Limit:    limitVal,
				}
				if row.Expire == "" {
					row.Expire = defaultExpire
				}

				if best == nil {
					best = row
					continue
				}

				// Merge
				if best.Hy2Auth == "" && row.Hy2Auth != "" {
					best.Hy2Auth = row.Hy2Auth
				}
				if best.Password == "" && row.Password != "" {
					best.Password = row.Password
				}
				if best.ID == "" && row.ID != "" {
					best.ID = row.ID
				}
				if best.Hy2Obfs == "" && row.Hy2Obfs != "" {
					best.Hy2Obfs = row.Hy2Obfs
				}
				if best.Expire == "" && row.Expire != "" {
					best.Expire = row.Expire
				}
			}
		}
	}
	return best
}

func pickRequestValue(req *Request, queryKeys, headerKeys []string) string {
	for _, qk := range queryKeys {
		if v, ok := req.Query[qk]; ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	for _, hk := range headerKeys {
		// Try case-insensitive header matches
		for k, v := range req.Headers {
			if strings.EqualFold(k, hk) && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		}
	}
	return ""
}

func normalizeHwid(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return ""
	}
	// Strip separators and canonicalize hex
	var sb strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'f') || (r >= '0' && r <= '9') {
			sb.WriteRune(r)
		}
	}
	hexOnly := sb.String()
	l := len(hexOnly)
	if l == 16 || l == 32 || l == 40 || l == 64 {
		return hexOnly
	}
	return s
}

func firstRealityPrivateKey(cfg vpn.RawConfig) string {
	inbounds, err := cfg.GetInbounds()
	if err != nil {
		return ""
	}
	for _, ib := range inbounds {
		if stream, ok := ib["streamSettings"]; ok {
			var ss map[string]json.RawMessage
			if json.Unmarshal(stream, &ss) == nil {
				if reality, ok := ss["realitySettings"]; ok {
					var rs map[string]interface{}
					if json.Unmarshal(reality, &rs) == nil {
						if pkey, ok := rs["privateKey"]; ok {
							if s, ok := pkey.(string); ok {
								return strings.TrimSpace(s)
							}
						}
					}
				}
			}
		}
	}
	return ""
}

func firstRealityPublicKey(cfg vpn.RawConfig) string {
	inbounds, err := cfg.GetInbounds()
	if err != nil {
		return ""
	}
	for _, ib := range inbounds {
		if stream, ok := ib["streamSettings"]; ok {
			var ss map[string]json.RawMessage
			if json.Unmarshal(stream, &ss) == nil {
				if reality, ok := ss["realitySettings"]; ok {
					var rs map[string]interface{}
					if json.Unmarshal(reality, &rs) == nil {
						if pkey, ok := rs["publicKey"]; ok {
							if s, ok := pkey.(string); ok {
								return strings.TrimSpace(s)
							}
						}
					}
				}
			}
		}
	}
	return ""
}

func randomRealityShortID(cfg vpn.RawConfig) string {
	inbounds, err := cfg.GetInbounds()
	if err != nil {
		return ""
	}
	for _, ib := range inbounds {
		if stream, ok := ib["streamSettings"]; ok {
			var ss map[string]json.RawMessage
			if json.Unmarshal(stream, &ss) == nil {
				if reality, ok := ss["realitySettings"]; ok {
					var rs map[string]interface{}
					if json.Unmarshal(reality, &rs) == nil {
						if sids, ok := rs["shortIds"]; ok {
							if arr, ok := sids.([]interface{}); ok {
								var validSIDs []string
								for _, item := range arr {
									if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
										validSIDs = append(validSIDs, strings.TrimSpace(s))
									}
								}
								if len(validSIDs) > 0 {
									n, err := rand.Int(rand.Reader, big.NewInt(int64(len(validSIDs))))
									if err == nil {
										return validSIDs[n.Int64()]
									}
									return validSIDs[0] // fallback
								}
							}
						}
					}
				}
			}
		}
	}
	return ""
}

func firstRealitySNI(cfg vpn.RawConfig) string {
	inbounds, err := cfg.GetInbounds()
	if err != nil {
		return "google.com"
	}
	for _, ib := range inbounds {
		if stream, ok := ib["streamSettings"]; ok {
			var ss map[string]json.RawMessage
			if json.Unmarshal(stream, &ss) == nil {
				if reality, ok := ss["realitySettings"]; ok {
					var rs map[string]interface{}
					if json.Unmarshal(reality, &rs) == nil {
						if snis, ok := rs["serverNames"]; ok {
							if arr, ok := snis.([]interface{}); ok {
								for _, item := range arr {
									if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
										return strings.TrimSpace(s)
									}
								}
							}
						}
					}
				}
			}
		}
	}
	return "google.com"
}

var (
	pubKeyCache   = make(map[string]string)
	pubKeyCacheMu sync.RWMutex
)

func derivePublicKey(privateKey string) string {
	privateKey = strings.TrimSpace(privateKey)
	if privateKey == "" {
		return ""
	}

	pubKeyCacheMu.RLock()
	pub, ok := pubKeyCache[privateKey]
	pubKeyCacheMu.RUnlock()
	if ok {
		return pub
	}

	pubKeyCacheMu.Lock()
	defer pubKeyCacheMu.Unlock()

	if pub, ok := pubKeyCache[privateKey]; ok {
		return pub
	}

	// Validate to prevent any command injection vectors
	if !regexp.MustCompile(`^[A-Za-z0-9\-_=]+$`).MatchString(privateKey) {
		pubKeyCache[privateKey] = ""
		return ""
	}
	// Decode private key
	privBytes, err := base64.RawURLEncoding.DecodeString(privateKey)
	if err != nil {
		privBytes, err = base64.StdEncoding.DecodeString(privateKey)
		if err != nil {
			pubKeyCache[privateKey] = ""
			return ""
		}
	}

	if len(privBytes) != 32 {
		pubKeyCache[privateKey] = ""
		return ""
	}

	// Apply Curve25519 clamping (Xray compatibility)
	privBytes[0] &= 248
	privBytes[31] &= 127
	privBytes[31] |= 64

	key, err := ecdh.X25519().NewPrivateKey(privBytes)
	if err != nil {
		pubKeyCache[privateKey] = ""
		return ""
	}

	pubBytes := key.PublicKey().Bytes()
	pub = base64.RawURLEncoding.EncodeToString(pubBytes)

	pubKeyCache[privateKey] = pub
	return pub
}

func ssServerPassword(cfg vpn.RawConfig) string {
	inbounds, err := cfg.GetInbounds()
	if err != nil {
		return ""
	}
	for _, ib := range inbounds {
		if ib.Tag() == "ss2022-in" {
			if settings, ok := ib["settings"]; ok {
				var s map[string]interface{}
				if json.Unmarshal(settings, &s) == nil {
					if pass, ok := s["password"]; ok {
						if p, ok := pass.(string); ok {
							return strings.TrimSpace(p)
						}
					}
				}
			}
		}
	}
	return ""
}

func extractHy2Pass(rawAuth string) string {
	rawAuth = strings.TrimSpace(rawAuth)
	if rawAuth == "" || strings.ToLower(rawAuth) == "null" {
		return ""
	}
	return rawAuth
}

func buildDeterministicHy2Pass(uuidHint, email string) string {
	h := sha256.New()
	h.Write([]byte(uuidHint + ":" + email + ":hy2"))
	return hex.EncodeToString(h.Sum(nil))
}

func getOrCreateHy2ObfsPassword(yamlPath string, cfg vpn.RawConfig) string {
	// 1. Try parsing directly from Xray Hysteria2 inbound settings (settings.obfs.password)
	inbounds, err := cfg.GetInbounds()
	if err == nil {
		for _, ib := range inbounds {
			p := ib.Protocol()
			if p == "hysteria2" || p == "hysteria" || p == "hy2" {
				if settings, ok := ib["settings"]; ok {
					var s map[string]interface{}
					if json.Unmarshal(settings, &s) == nil {
						if obfs, ok := s["obfs"].(map[string]interface{}); ok {
							if pass, ok := obfs["password"].(string); ok && strings.TrimSpace(pass) != "" {
								return strings.TrimSpace(pass)
							}
						}
					}
				}
			}
		}
	}

	// 2. Try legacy standalone hysteria yaml config fallback
	val := getHy2ObfsPasswordFromYAML(yamlPath)
	if val != "" {
		return val
	}

	// 3. Try legacy client metadata fallback
	if err == nil {
		for _, ib := range inbounds {
			clients, err := ib.GetClients()
			if err == nil {
				for _, c := range clients {
					if obfs := c.GetString("hy2_obfs"); obfs != "" {
						return obfs
					}
				}
			}
		}
	}

	// 4. Fallback to empty string (values must be taken from config, no random generation)
	return ""
}

func getHy2ObfsPasswordFromYAML(yamlPath string) string {
	if yamlPath == "" {
		return ""
	}
	data, err := os.ReadFile(yamlPath)
	if err != nil {
		return ""
	}
	lines := strings.Split(string(data), "\n")
	inObfs := false
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "obfs:") {
			inObfs = true
			continue
		}
		if inObfs && strings.Contains(line, ":") && !strings.HasPrefix(line, "password:") {
			inObfs = false
		}
		if inObfs && strings.HasPrefix(line, "password:") {
			val := strings.TrimPrefix(line, "password:")
			if idx := strings.Index(val, "#"); idx >= 0 {
				val = val[:idx]
			}
			val = strings.TrimSpace(val)
			val = strings.Trim(val, `"'`)
			return val
		}
	}
	return ""
}

func getTrafficBytes(statsPath string, email string) (up int64, down int64) {
	if statsPath == "" {
		return 0, 0
	}
	data, err := os.ReadFile(statsPath)
	if err != nil {
		return 0, 0
	}

	var parsed struct {
		Users map[string]struct {
			CumulativeUp   float64 `json:"cumulative_up"`
			CumulativeDown float64 `json:"cumulative_down"`
		} `json:"users"`
	}

	if json.Unmarshal(data, &parsed) != nil || parsed.Users == nil {
		return 0, 0
	}

	userObj, ok := parsed.Users[email]
	if !ok {
		return 0, 0
	}

	return int64(userObj.CumulativeUp), int64(userObj.CumulativeDown)
}

func defaultExpireDate() string {
	return time.Now().AddDate(0, 0, 30).Format("02-01-2006")
}

func parseDateToTimestamp(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	// Try parsing as numeric Unix timestamp directly (to handle already-parsed values passed to generateHeader)
	if ts, err := strconv.ParseInt(s, 10, 64); err == nil && ts > 0 {
		return ts
	}
	// Try ISO layouts (DD-MM-YYYY or DD.MM.YYYY)
	if m := validDateRe.FindStringSubmatch(s); len(m) == 4 {
		iso := fmt.Sprintf("%s-%s-%s 00:00:00 +0000 UTC", m[3], m[2], m[1])
		t, err := time.Parse("2006-01-02 15:04:05 -0700 MST", iso)
		if err == nil {
			return t.Unix()
		}
	}
	// Fallback formats
	layouts := []string{
		"2006-01-02 15:04:05",
		"2006-01-02",
		"2006.01.02 15:04:05",
		"2006.01.02",
		time.RFC3339,
	}
	for _, l := range layouts {
		if t, err := time.Parse(l, s); err == nil {
			return t.Unix()
		}
	}
	return 0
}

func getRequestHost(req *Request, fallbackDomain string) string {
	host := req.Headers["host"]
	if host == "" {
		host = req.Headers["x-forwarded-host"]
	}
	if host == "" {
		host = fallbackDomain
	}
	// Sanitize
	var sb strings.Builder
	for _, r := range host {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '-' || r == ':' {
			sb.WriteRune(r)
		}
	}
	res := sb.String()
	if res == "" {
		return "example.com"
	}
	return res
}

// parseSubscriptionTemplate parses a subscription template file.
// Format:
//
//	# Profile-Title: ...
//	# Profile-Update-Interval: ...
//
//	{  <-- first JSON template block
//	  "remarks": "Config 1",
//	  ...
//	}
//	# ---          <-- optional separator (or just blank line between blocks)
//	{  <-- second JSON template block
//	  "remarks": "Config 2",
//	  ...
//	}
//
// Returns (headerSection, []templateBlocks).
func parseSubscriptionTemplate(content string) (string, []string) {
	lines := strings.Split(content, "\n")
	var header strings.Builder
	var templates []string
	isHeader := true
	var jsonBody strings.Builder
	depth := 0 // brace depth tracker

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		if isHeader {
			if trimmed != "" && (strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[")) {
				isHeader = false
				jsonBody.WriteString(line + "\n")
				for _, ch := range trimmed {
					if ch == '{' || ch == '[' {
						depth++
					} else if ch == '}' || ch == ']' {
						depth--
					}
				}
			} else {
				header.WriteString(line + "\n")
			}
			continue
		}

		// We are inside a JSON block.
		// Check for block separator: a line that is "# ---" or starts a new JSON block
		// after a completed block (depth == 0).
		if depth == 0 {
			if trimmed == "# ---" || trimmed == "---" {
				// Explicit separator: save current block, start next
				if block := strings.TrimSpace(jsonBody.String()); block != "" {
					templates = append(templates, block)
				}
				jsonBody.Reset()
				continue
			}
			if trimmed != "" && (strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[")) {
				// New JSON block starting — save the previous one
				if block := strings.TrimSpace(jsonBody.String()); block != "" {
					templates = append(templates, block)
				}
				jsonBody.Reset()
			}
		}

		jsonBody.WriteString(line + "\n")
		for _, ch := range trimmed {
			if ch == '{' || ch == '[' {
				depth++
			} else if ch == '}' || ch == ']' {
				depth--
			}
		}
	}

	if block := strings.TrimSpace(jsonBody.String()); block != "" {
		templates = append(templates, block)
	}

	if len(templates) > 0 {
		return header.String(), templates
	}

	// Fallback to line-by-line link parsing if no JSON start was found
	isHeader = true
	header.Reset()
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			if isHeader {
				header.WriteString("\n")
			}
			continue
		}
		if strings.Contains(line, "://") {
			isHeader = false
			templates = append(templates, line)
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			if isHeader {
				header.WriteString(line + "\n")
			}
			continue
		}
		if isHeader {
			header.WriteString(line + "\n")
		}
	}

	return header.String(), templates
}

func generateHeader(email, sublink, headerText, expireVal, uploadVal, downloadVal string, isBlocked bool, limit int) string {
	daysLeft := "0"
	expireTs := parseDateToTimestamp(expireVal)
	if expireTs > 0 {
		nowTs := time.Now().Unix()
		if expireTs > nowTs {
			daysLeft = fmt.Sprintf("%d", (expireTs-nowTs+86399)/86400)
		}
	}

	blockedStr := "0"
	if isBlocked {
		blockedStr = "1"
	}

	upBytes, _ := strconv.ParseInt(uploadVal, 10, 64)
	downBytes, _ := strconv.ParseInt(downloadVal, 10, 64)

	tokens := map[string]string{
		"{EMAIL}":           email,
		"{SUBLINK}":         sublink,
		"{EXPIRE}":          expireVal,
		"{UP}":              uploadVal,
		"{DOWN}":            downloadVal,
		"{TOTAL}":           fmt.Sprintf("%d", upBytes+downBytes),
		"{DOWNLOAD}":        downloadVal,
		"{DAYS_LEFT}":       daysLeft,
		"{EXPIRE_DAYS}":     daysLeft,
		"{IS_USER_BLOCKED}": blockedStr,
		"{DEVICE_LIMIT}":    fmt.Sprintf("%d", limit),
	}

	lines := strings.Split(headerText, "\n")
	var out []string
	hasBlockedLine := false

	for _, line := range lines {
		lineTrimmed := strings.TrimSpace(line)
		if lineTrimmed == "" {
			continue
		}
		if strings.HasPrefix(lineTrimmed, "#announce:") {
			announceVal := strings.TrimPrefix(lineTrimmed, "#announce:")
			for k, v := range tokens {
				announceVal = strings.ReplaceAll(announceVal, k, v)
			}
			// Replace literal \n sequence with actual newlines for multi-line announcements
			announceVal = strings.ReplaceAll(announceVal, `\n`, "\n")
			out = append(out, "#announce: base64:"+base64.StdEncoding.EncodeToString([]byte(announceVal)))
			continue
		}

		lineRendered := line
		for k, v := range tokens {
			lineRendered = strings.ReplaceAll(lineRendered, k, v)
		}

		if strings.HasPrefix(lineTrimmed, "#is-user-blocked:") {
			out = append(out, "#is-user-blocked: "+blockedStr)
			hasBlockedLine = true
			continue
		}

		if strings.HasPrefix(lineTrimmed, "#profile-title:") {
			titleVal := strings.TrimSpace(strings.TrimPrefix(lineTrimmed, "#profile-title:"))
			if !strings.HasPrefix(titleVal, "base64:") {
				out = append(out, "#profile-title: base64:"+base64.StdEncoding.EncodeToString([]byte(titleVal)))
			} else {
				out = append(out, lineRendered)
			}
			continue
		}

		out = append(out, lineRendered)
	}

	if !hasBlockedLine {
		out = append(out, "#is-user-blocked: "+blockedStr)
	}

	return strings.Join(out, "\n")
}

type HeaderMeta struct {
	Title           string
	UserInfo        string
	UpdateInterval  string
	WebUrl          string
	MaxDevicesCount int
	IsUserBlocked   bool
	CustomHeaders   map[string]string
}

func parseHeaderMetadata(headerText string) HeaderMeta {
	meta := HeaderMeta{
		Title:           "Torvalds VPN",
		UpdateInterval:  "12",
		MaxDevicesCount: 3,
		CustomHeaders:   make(map[string]string),
	}

	lines := strings.Split(headerText, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) < 2 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(parts[0]))
		val := strings.TrimSpace(parts[1])

		// Save custom header
		hKey := strings.TrimPrefix(key, "#")
		hKey = strings.ReplaceAll(hKey, " ", "-")
		hKey = http.CanonicalHeaderKey(hKey) // Capitalize first letters
		meta.CustomHeaders[hKey] = val

		switch key {
		case "#max-devices-count":
			fmt.Sscanf(val, "%d", &meta.MaxDevicesCount)
		case "#is-user-blocked":
			var blocked int
			fmt.Sscanf(val, "%d", &blocked)
			meta.IsUserBlocked = blocked == 1
		case "#profile-title":
			if strings.HasPrefix(val, "base64:") {
				if dec, err := base64.StdEncoding.DecodeString(val[7:]); err == nil {
					meta.Title = string(dec)
				}
			} else {
				meta.Title = val
			}
		case "#subscription-userinfo":
			meta.UserInfo = val
		case "#profile-update-interval":
			meta.UpdateInterval = val
		case "#profile-web-page-url":
			meta.WebUrl = val
		}
	}
	return meta
}

func buildErrorJSONResponse(lines []string) string {
	var nodes []map[string]interface{}
	for _, line := range lines {
		nodes = append(nodes, map[string]interface{}{
			"dns": map[string]interface{}{
				"servers": []string{"1.1.1.1", "1.0.0.1"},
			},
			"inbounds": []interface{}{},
			"log": map[string]interface{}{
				"loglevel": "none",
				"dnsLog":   false,
			},
			"outbounds": []map[string]interface{}{
				{
					"protocol": "freedom",
					"tag":      "direct",
				},
			},
			"remarks": line,
		})
	}
	data, _ := json.MarshalIndent(nodes, "", "  ")
	return string(data)
}

func canonicalDeviceClientKey(filename, clientId string) string {
	norm := normalizeSubfileToID(filename)
	if norm == "" {
		norm = normalizeSubfileToID(clientId)
	}
	if norm != "" {
		return norm + ".txt"
	}
	fallback := strings.TrimSpace(filename)
	if fallback == "" {
		fallback = strings.TrimSpace(clientId)
	}
	if fallback == "" {
		return ""
	}
	if !strings.HasSuffix(strings.ToLower(fallback), ".txt") {
		fallback += ".txt"
	}
	return strings.ToLower(fallback)
}

func buildDeviceClientKeyVariants(filename, clientId string) []string {
	var variants []string
	rawFile := strings.TrimSpace(filename)
	rawId := strings.TrimSpace(clientId)

	if rawFile != "" {
		variants = append(variants, rawFile, strings.ToLower(rawFile))
	}
	if rawId != "" {
		variants = append(variants, rawId, strings.ToLower(rawId), rawId+".txt", strings.ToLower(rawId)+".txt")
	}

	fileNorm := normalizeSubfileToID(rawFile)
	if fileNorm != "" {
		variants = append(variants, fileNorm, fileNorm+".txt")
	}

	idNorm := normalizeSubfileToID(rawId)
	if idNorm != "" {
		variants = append(variants, idNorm, idNorm+".txt")
	}

	// Unique filter
	var out []string
	seen := make(map[string]bool)
	for _, v := range variants {
		v = strings.TrimSpace(v)
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

func firstReadablePath(paths ...string) string {
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			if f, err := os.Open(p); err == nil {
				f.Close()
				return p
			}
		}
	}
	// No readable path found; return empty string so callers can handle stat errors gracefully.
	_ = paths
	return ""
}
