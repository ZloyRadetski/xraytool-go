package convert

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"github.com/xtls/libxray/share"
)

var (
	tagWordsOnce   sync.Once
	cachedTagWords []string
)

const systemDictPath = "/usr/share/dict"

// XrayJSONToShareText converts an Xray JSON config (single config, outbound array, or config array) to share links
func XrayJSONToShareText(xrayJSON string) (string, error) {
	var root any
	if err := json.Unmarshal([]byte(xrayJSON), &root); err != nil {
		return "", fmt.Errorf("failed to parse Xray JSON: %w", err)
	}

	// Case 1: JSON array
	if array, ok := root.([]any); ok && len(array) > 0 {
		first := array[0]
		firstMap, isMap := first.(map[string]any)

		if isMap {
			if _, hasProtocol := firstMap["protocol"]; hasProtocol {
				// It's a raw array of outbounds. Wrap it and convert.
				wrapped := map[string]any{
					"outbounds": array,
				}
				encoded, err := json.Marshal(wrapped)
				if err != nil {
					return "", err
				}
				return convertSingleConfigToShareLinks(string(encoded), "")
			}

			if _, hasOutbounds := firstMap["outbounds"]; hasOutbounds {
				// It's an array of configuration profiles (e.g. from subscription configs.txt)
				var links []string
				for _, item := range array {
					itemMap, ok := item.(map[string]any)
					if !ok {
						continue
					}
					// Extract user-friendly display name (remarks)
					remarks, _ := itemMap["remarks"].(string)

					// Filter out only proxy outbounds (ignore routing block like freedom, blackhole, dns)
					rawOutbounds, _ := itemMap["outbounds"].([]any)
					var proxyOutbounds []any
					for _, rawOb := range rawOutbounds {
						ob, ok := rawOb.(map[string]any)
						if !ok {
							continue
						}
						proto, ok := ob["protocol"].(string)
						if !ok {
							continue
						}
						protoLower := strings.ToLower(proto)
						if protoLower == "freedom" || protoLower == "blackhole" || protoLower == "dns" {
							continue
						}
						proxyOutbounds = append(proxyOutbounds, ob)
					}

					if len(proxyOutbounds) > 0 {
						singleConfig := map[string]any{
							"outbounds": proxyOutbounds,
						}
						encoded, err := json.Marshal(singleConfig)
						if err != nil {
							continue
						}
						linkText, err := convertSingleConfigToShareLinks(string(encoded), remarks)
						if err == nil && strings.TrimSpace(linkText) != "" {
							links = append(links, strings.TrimSpace(linkText))
						}
					}
				}
				return strings.Join(links, "\n"), nil
			}
		}
	}

	// Case 2: Single Xray configuration object (or fallback)
	return convertSingleConfigToShareLinks(xrayJSON, "")
}

func convertSingleConfigToShareLinks(xrayJSON string, remarksOverride string) (string, error) {
	normalizedJSON, err := normalizeXrayJSONForShareLinks(xrayJSON)
	if err != nil {
		return "", err
	}
	links, err := share.ConvertXrayJsonToShareLinks([]byte(normalizedJSON))
	if err != nil {
		return "", err
	}

	if remarksOverride != "" {
		// Replace tag/fragment at the end of generated links with URL-escaped remarks
		lines := strings.Split(links, "\n")
		for i, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			if idx := strings.IndexByte(line, '#'); idx >= 0 {
				line = line[:idx]
			}
			// Use path escaping as required by share links specification
			escapedRemarks := url.PathEscape(remarksOverride)
			lines[i] = line + "#" + escapedRemarks
		}
		links = strings.Join(lines, "\n")
	}

	return links, nil
}

// ShareLinkToXrayJSON converts a share link (VLESS, VMESS, etc.) to an Xray JSON config
func ShareLinkToXrayJSON(shareLink string) (string, error) {
	data, err := ShareLinkToXrayJSONData(shareLink)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// ShareLinkToXrayJSONData converts a share link to raw JSON bytes
func ShareLinkToXrayJSONData(shareLink string) (json.RawMessage, error) {
	if proxyJSON, ok, err := convertBareHTTPProxyURLToXrayJSON(shareLink); ok || err != nil {
		return proxyJSON, err
	}

	normalizedShareLink, err := normalizeShareTextForLibXray(shareLink)
	if err != nil {
		return nil, err
	}
	config, err := share.ConvertShareLinksToXrayJson(normalizedShareLink)
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(config)
	if err != nil {
		return nil, fmt.Errorf("failed to encode Xray JSON output: %w", err)
	}
	data, _, err = fillEmptyOutboundTags(data, randomTwoWordTag)
	if err != nil {
		return nil, err
	}
	return stripJSONNullFields(data)
}

func normalizeXrayJSONForShareLinks(xrayJSON string) (string, error) {
	var root any
	if err := json.Unmarshal([]byte(xrayJSON), &root); err != nil {
		return "", fmt.Errorf("failed to parse Xray JSON for share-link normalization: %w", err)
	}

	// If the JSON is an array of outbounds, automatically wrap it in a root object: {"outbounds": [...]}
	if array, ok := root.([]any); ok {
		root = map[string]any{
			"outbounds": array,
		}
	}

	rootObject, ok := root.(map[string]any)
	if !ok {
		return xrayJSON, nil
	}
	rawOutbounds, ok := rootObject["outbounds"].([]any)
	if !ok {
		return xrayJSON, nil
	}

	for _, rawOutbound := range rawOutbounds {
		outbound, ok := rawOutbound.(map[string]any)
		if !ok {
			continue
		}
		settings, ok := outbound["settings"].(map[string]any)
		if ok {
			protocol, _ := outbound["protocol"].(string)
			normalizeOutboundSettingsForShareLink(protocol, settings)
		}

		// Workaround for libxray bug: copy publicKey to password under realitySettings
		streamSettings, ok := outbound["streamSettings"].(map[string]any)
		if ok {
			realitySettings, ok := streamSettings["realitySettings"].(map[string]any)
			if ok {
				if pubKey, hasPubKey := realitySettings["publicKey"]; hasPubKey && pubKey != nil {
					realitySettings["password"] = pubKey
				}
			}
		}
	}

	encoded, err := json.Marshal(root)
	if err != nil {
		return "", fmt.Errorf("failed to encode Xray JSON after share-link normalization: %w", err)
	}
	return string(encoded), nil
}

func normalizeOutboundSettingsForShareLink(protocol string, settings map[string]any) {
	switch strings.ToLower(protocol) {
	case "vless", "vmess":
		normalizeVNextSettingsForShareLink(protocol, settings)
	case "trojan", "shadowsocks", "socks", "http":
		normalizeServerSettingsForShareLink(protocol, settings)
	}
}

func normalizeVNextSettingsForShareLink(protocol string, settings map[string]any) {
	server := firstObjectFromArray(settings["vnext"])
	if server == nil {
		return
	}
	copySettingIfMissing(settings, server, "address")
	copySettingIfMissing(settings, server, "port")

	user := firstObjectFromArray(server["users"])
	if user == nil {
		return
	}
	copySettingIfMissing(settings, user, "id")
	copySettingIfMissing(settings, user, "level")
	copySettingIfMissing(settings, user, "email")
	switch strings.ToLower(protocol) {
	case "vless":
		copySettingIfMissing(settings, user, "flow")
		copySettingIfMissing(settings, user, "encryption")
	case "vmess":
		if settings["security"] == nil {
			if security, ok := user["security"]; ok {
				settings["security"] = security
			} else if security, ok := user["encryption"]; ok {
				settings["security"] = security
			}
		}
	}
}

func normalizeServerSettingsForShareLink(protocol string, settings map[string]any) {
	server := firstObjectFromArray(settings["servers"])
	if server == nil {
		return
	}
	copySettingIfMissing(settings, server, "address")
	copySettingIfMissing(settings, server, "port")
	copySettingIfMissing(settings, server, "level")
	copySettingIfMissing(settings, server, "email")

	switch strings.ToLower(protocol) {
	case "trojan":
		copySettingIfMissing(settings, server, "password")
		copySettingIfMissing(settings, server, "flow")
	case "shadowsocks":
		copySettingIfMissing(settings, server, "method")
		copySettingIfMissing(settings, server, "password")
	case "socks", "http":
		user := firstObjectFromArray(server["users"])
		if user != nil {
			copySettingIfMissing(settings, user, "user")
			copySettingIfMissing(settings, user, "pass")
		}
	}
}

func firstObjectFromArray(value any) map[string]any {
	items, ok := value.([]any)
	if !ok || len(items) == 0 {
		return nil
	}
	object, ok := items[0].(map[string]any)
	if !ok {
		return nil
	}
	return object
}

func copySettingIfMissing(target, source map[string]any, key string) {
	if target[key] != nil {
		return
	}
	if value, ok := source[key]; ok && value != nil {
		target[key] = value
	}
}

func fillEmptyOutboundTags(data json.RawMessage, tagGenerator func() (string, error)) (json.RawMessage, int, error) {
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, 0, fmt.Errorf("failed to parse JSON output for tag generation: %w", err)
	}

	rawOutbounds, ok := root["outbounds"]
	if !ok || rawOutbounds == nil {
		return data, 0, nil
	}

	outbounds, ok := rawOutbounds.([]any)
	if !ok {
		return nil, 0, fmt.Errorf("outbounds is not a JSON array")
	}

	filled := 0
	for i, rawOutbound := range outbounds {
		outbound, ok := rawOutbound.(map[string]any)
		if !ok {
			return nil, 0, fmt.Errorf("outbound %d is not a JSON object", i)
		}

		tag, ok := outbound["tag"].(string)
		if ok && tag != "" {
			continue
		}
		if outboundHasName(outbound) {
			continue
		}

		generatedTag, err := tagGenerator()
		if err != nil {
			return nil, 0, fmt.Errorf("failed to generate tag for outbound %d: %w", i, err)
		}
		outbound["tag"] = generatedTag
		filled++
	}

	if filled == 0 {
		return data, 0, nil
	}

	encoded, err := json.Marshal(root)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to encode JSON output after tag generation: %w", err)
	}

	return encoded, filled, nil
}

func outboundHasName(outbound map[string]any) bool {
	for _, key := range []string{"sendThrough", "name"} {
		if value, ok := outbound[key].(string); ok && strings.TrimSpace(value) != "" {
			return true
		}
	}
	return false
}

func stripJSONNullFields(data json.RawMessage) (json.RawMessage, error) {
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, fmt.Errorf("failed to parse JSON output for null cleanup: %w", err)
	}
	cleaned := stripNullFields(value)
	encoded, err := json.Marshal(cleaned)
	if err != nil {
		return nil, fmt.Errorf("failed to encode JSON output after null cleanup: %w", err)
	}
	return encoded, nil
}

func stripNullFields(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if child == nil {
				delete(typed, key)
				continue
			}
			typed[key] = stripNullFields(child)
		}
		return typed
	case []any:
		for i, child := range typed {
			typed[i] = stripNullFields(child)
		}
		return typed
	default:
		return value
	}
}

func randomTwoWordTag() (string, error) {
	words := tagWords()
	if len(words) == 0 {
		words = fallbackTagWords()
	}

	first, err := randomWord(words)
	if err != nil {
		return "", err
	}
	second, err := randomWord(words)
	if err != nil {
		return "", err
	}

	return first + "-" + second, nil
}

func tagWords() []string {
	tagWordsOnce.Do(func() {
		words, err := loadDictionaryWords(systemDictPath)
		if err != nil {
			words = fallbackTagWords()
		}
		cachedTagWords = words
	})
	return cachedTagWords
}

func loadDictionaryWords(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	seen := make(map[string]struct{})
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if err := readDictionaryFile(filepath.Join(dir, entry.Name()), seen); err != nil {
			return nil, err
		}
	}

	words := make([]string, 0, len(seen))
	for word := range seen {
		words = append(words, word)
	}
	return words, nil
}

func readDictionaryFile(path string, seen map[string]struct{}) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		if word, ok := normalizeTagWord(scanner.Text()); ok {
			seen[word] = struct{}{}
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func normalizeTagWord(raw string) (string, bool) {
	word := strings.ToLower(strings.TrimSpace(raw))
	if len(word) < 3 || len(word) > 12 {
		return "", false
	}

	for _, r := range word {
		if !unicode.IsLetter(r) {
			return "", false
		}
	}

	return word, true
}

func randomWord(words []string) (string, error) {
	if len(words) == 0 {
		return "", fmt.Errorf("word list is empty")
	}

	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(words))))
	if err != nil {
		return "", err
	}
	return words[n.Int64()], nil
}

func fallbackTagWords() []string {
	return []string{
		"amber", "anchor", "breeze", "brook", "cedar", "cloud",
		"copper", "ember", "falcon", "field", "harbor", "lantern",
		"meadow", "orbit", "pebble", "river", "silver", "summit",
	}
}

// StripJSONComments removes comments from a JSON-with-comments string
func StripJSONComments(input string) (string, error) {
	var out strings.Builder
	out.Grow(len(input))

	inString := false
	escaped := false

	for i := 0; i < len(input); i++ {
		ch := input[i]

		if inString {
			out.WriteByte(ch)
			if escaped {
				escaped = false
				continue
			}
			if ch == '\\' {
				escaped = true
				continue
			}
			if ch == '"' {
				inString = false
			}
			continue
		}

		if ch == '"' {
			inString = true
			out.WriteByte(ch)
			continue
		}

		if ch == '/' && i+1 < len(input) {
			next := input[i+1]
			if next == '/' {
				i += 2
				for ; i < len(input) && input[i] != '\n'; i++ {
				}
				if i < len(input) {
					out.WriteByte(input[i])
				}
				continue
			}
			if next == '*' {
				i += 2
				closed := false
				for ; i < len(input); i++ {
					if input[i] == '\n' {
						out.WriteByte('\n')
					}
					if input[i] == '*' && i+1 < len(input) && input[i+1] == '/' {
						i++
						closed = true
						break
					}
				}
				if !closed {
					return "", fmt.Errorf("unterminated block comment")
				}
				continue
			}
		}

		out.WriteByte(ch)
	}

	if inString {
		return "", fmt.Errorf("unterminated string")
	}

	return out.String(), nil
}

// NormalizeJSONInput validates and prepares JSON input
func NormalizeJSONInput(input string) (string, bool, error) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return "", false, nil
	}

	stripped, err := StripJSONComments(trimmed)
	if err != nil {
		return "", false, fmt.Errorf("failed to parse JSON comments: %w", err)
	}

	candidate := strings.TrimSpace(stripped)
	if !strings.HasPrefix(candidate, "{") && !strings.HasPrefix(candidate, "[") {
		return "", false, nil
	}

	var raw json.RawMessage
	if err := json.Unmarshal([]byte(candidate), &raw); err != nil {
		return "", false, fmt.Errorf("invalid JSON input: %w", err)
	}

	return candidate, true, nil
}

func normalizeShareTextForLibXray(text string) (string, error) {
	lines := strings.Split(text, "\n")
	shareLinks := make([]string, 0, len(lines))
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		lines[i] = trimmed
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if normalized, ok, err := normalizeSubscriptionURILine(trimmed); err != nil {
			return "", err
		} else if ok {
			shareLinks = append(shareLinks, normalized)
		}
	}
	if len(shareLinks) > 0 {
		return strings.Join(shareLinks, "\n"), nil
	}
	return strings.Join(lines, "\n"), nil
}

func normalizeSubscriptionURILine(line string) (string, bool, error) {
	if !strings.Contains(line, "://") {
		return "", false, nil
	}

	parsed, err := url.Parse(line)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", false, nil
	}

	if strings.EqualFold(parsed.Scheme, "vmess") {
		normalized := normalizeVMessQRCodeLink(line)
		if isVMessQRCodeLink(normalized) || hasURLPort(normalized) {
			return normalized, true, nil
		}
		return "", false, nil
	}

	if strings.EqualFold(parsed.Scheme, "http") {
		if (parsed.Path == "" || parsed.Path == "/") && parsed.RawQuery == "" && parsed.Port() != "" {
			return line, true, nil
		}
		return "", false, nil
	}
	if strings.EqualFold(parsed.Scheme, "https") {
		return "", false, nil
	}
	if strings.EqualFold(parsed.Scheme, "mixed") {
		return "", false, fmt.Errorf("mixed proxy URLs are unsupported because Xray mixed is inbound-only")
	}
	if isSocks5ProxyScheme(parsed.Scheme) {
		normalized, err := normalizeSocks5URLForLibXray(parsed)
		if err != nil {
			return "", false, err
		}
		return normalized, true, nil
	}
	if requiresURLPort(parsed.Scheme) && parsed.Port() == "" {
		return "", false, nil
	}
	return line, true, nil
}

func normalizeSocks5URLForLibXray(parsed *url.URL) (string, error) {
	if parsed.Hostname() == "" {
		return "", fmt.Errorf("%s proxy link is missing a host", parsed.Scheme)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil || port < 1 || port > 65535 {
		return "", fmt.Errorf("invalid %s proxy port: %q", parsed.Scheme, parsed.Port())
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return "", fmt.Errorf("%s proxy links must not include a path", parsed.Scheme)
	}
	if parsed.RawQuery != "" {
		return "", fmt.Errorf("%s proxy links must not include a query string", parsed.Scheme)
	}

	normalized := &url.URL{
		Scheme:   "socks",
		Host:     parsed.Host,
		Fragment: parsed.Fragment,
	}
	if parsed.User != nil {
		username := parsed.User.Username()
		password, hasPassword := parsed.User.Password()
		if username != "" || hasPassword {
			encoded := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
			normalized.User = url.User(encoded)
		}
	}
	return normalized.String(), nil
}

func isSocks5ProxyScheme(scheme string) bool {
	return strings.EqualFold(scheme, "socks5") || strings.EqualFold(scheme, "socks5h")
}

func hasURLPort(line string) bool {
	parsed, err := url.Parse(line)
	return err == nil && parsed.Port() != ""
}

func requiresURLPort(scheme string) bool {
	switch strings.ToLower(scheme) {
	case "vless", "trojan", "socks", "socks5", "socks5h", "ss":
		return true
	default:
		return false
	}
}

func normalizeVMessQRCodeLink(link string) string {
	const prefix = "vmess://"
	if !strings.HasPrefix(strings.ToLower(link), prefix) {
		return link
	}

	payload := link[len(prefix):]
	if isVMessQRCodePayload(payload) {
		return link
	}

	if normalized, ok := trimVMessQRCodePayload(payload); ok {
		return prefix + normalized
	}
	return link
}

func isVMessQRCodeLink(link string) bool {
	const prefix = "vmess://"
	return strings.HasPrefix(strings.ToLower(link), prefix) && isVMessQRCodePayload(link[len(prefix):])
}

func trimVMessQRCodePayload(payload string) (string, bool) {
	if padding := strings.IndexByte(payload, '='); padding >= 0 {
		end := padding + 1
		if end < len(payload) && payload[end] == '=' {
			end++
		}
		candidate := payload[:end]
		if isVMessQRCodePayload(candidate) {
			return candidate, true
		}
	}

	for end := len(payload) - 1; end > 0; end-- {
		candidate := payload[:end]
		if isVMessQRCodePayload(candidate) {
			return candidate, true
		}
	}
	return "", false
}

func isVMessQRCodePayload(payload string) bool {
	decoded, err := decodeShareBase64(payload)
	if err != nil {
		return false
	}

	var qr map[string]json.RawMessage
	if err := json.Unmarshal(decoded, &qr); err != nil {
		return false
	}

	_, hasAddress := qr["add"]
	_, hasID := qr["id"]
	_, hasPort := qr["port"]
	return hasAddress && hasID && hasPort
}

func decodeShareBase64(text string) ([]byte, error) {
	normalized := strings.NewReplacer("-", "+", "_", "/").Replace(text)
	if missingPadding := len(normalized) % 4; missingPadding != 0 {
		normalized += strings.Repeat("=", 4-missingPadding)
	}
	return base64.StdEncoding.DecodeString(normalized)
}

func convertBareHTTPProxyURLToXrayJSON(shareLink string) (json.RawMessage, bool, error) {
	parsed, err := url.Parse(strings.TrimSpace(shareLink))
	if err != nil {
		return nil, false, nil
	}
	if !strings.EqualFold(parsed.Scheme, "http") {
		return nil, false, nil
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return nil, false, nil
	}
	if parsed.RawQuery != "" {
		return nil, false, nil
	}
	if parsed.Hostname() == "" {
		return nil, true, fmt.Errorf("%s proxy link is missing a host", parsed.Scheme)
	}
	if parsed.Port() == "" {
		return nil, true, fmt.Errorf("%s proxy link is missing a port", parsed.Scheme)
	}

	port, err := strconv.Atoi(parsed.Port())
	if err != nil || port < 1 || port > 65535 {
		return nil, true, fmt.Errorf("invalid %s proxy port: %q", parsed.Scheme, parsed.Port())
	}

	server := map[string]any{
		"address": parsed.Hostname(),
		"port":    port,
	}
	if parsed.User != nil {
		username := parsed.User.Username()
		password, hasPassword := parsed.User.Password()
		if username != "" || hasPassword {
			server["users"] = []map[string]string{
				{
					"user": username,
					"pass": password,
				},
			}
		}
	}

	tag := strings.TrimSpace(parsed.Fragment)
	if tag == "" {
		tag = "http"
	}

	root := map[string]any{
		"outbounds": []map[string]any{
			{
				"protocol": "http",
				"tag":      tag,
				"settings": map[string]any{
					"servers": []map[string]any{server},
				},
			},
		},
	}

	data, err := json.Marshal(root)
	if err != nil {
		return nil, true, fmt.Errorf("failed to encode http outbound: %w", err)
	}
	return data, true, nil
}

// ResolveInput reads input from string, file path or stdin
func ResolveInput(arg string) (string, string, error) {
	if arg == "-" {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", "stdin", err
		}
		input := strings.TrimSpace(string(data))
		if input == "" {
			return "", "stdin", fmt.Errorf("stdin is empty")
		}
		return input, "stdin", nil
	}

	if info, err := os.Stat(arg); err == nil && !info.IsDir() {
		data, err := os.ReadFile(arg)
		if err != nil {
			return "", arg, err
		}
		input := strings.TrimSpace(string(data))
		if input == "" {
			return "", arg, fmt.Errorf("file is empty: %s", arg)
		}
		return input, arg, nil
	}

	return strings.TrimSpace(arg), "arg", nil
}
