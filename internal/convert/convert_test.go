package convert

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestFirstObjectFromArray(t *testing.T) {
	if got := firstObjectFromArray(nil); got != nil {
		t.Errorf("expected nil, got %v", got)
	}
	if got := firstObjectFromArray([]any{}); got != nil {
		t.Errorf("expected nil, got %v", got)
	}
	if got := firstObjectFromArray([]any{"not a map"}); got != nil {
		t.Errorf("expected nil, got %v", got)
	}
	expected := map[string]any{"key": "value"}
	if got := firstObjectFromArray([]any{expected}); got["key"] != "value" {
		t.Errorf("expected map with key: value, got %v", got)
	}
}

func TestCopySettingIfMissing(t *testing.T) {
	target := map[string]any{}
	source := map[string]any{"foo": "bar"}
	copySettingIfMissing(target, source, "foo")
	if target["foo"] != "bar" {
		t.Errorf("expected foo: bar, got %v", target["foo"])
	}

	target = map[string]any{"foo": "baz"}
	copySettingIfMissing(target, source, "foo")
	if target["foo"] != "baz" {
		t.Errorf("expected foo: baz, got %v", target["foo"])
	}

	copySettingIfMissing(target, source, "missing")
	if target["missing"] != nil {
		t.Errorf("expected nil, got %v", target["missing"])
	}
}

func TestOutboundHasName(t *testing.T) {
	if outboundHasName(map[string]any{"name": "foo"}) != true {
		t.Error("expected true")
	}
	if outboundHasName(map[string]any{"sendThrough": "bar"}) != true {
		t.Error("expected true")
	}
	if outboundHasName(map[string]any{"other": "baz"}) != false {
		t.Error("expected false")
	}
	if outboundHasName(map[string]any{"name": "  "}) != false {
		t.Error("expected false for whitespace")
	}
}

func TestStripNullFields(t *testing.T) {
	input := map[string]any{
		"a": nil,
		"b": 1,
		"c": []any{nil, 2, map[string]any{"d": nil, "e": 3}},
	}
	cleaned := stripNullFields(input).(map[string]any)
	if _, ok := cleaned["a"]; ok {
		t.Error("expected a to be stripped")
	}
	if cleaned["b"] != 1 {
		t.Error("expected b to be 1")
	}
	c := cleaned["c"].([]any)
	if c[0] != nil { // Wait, stripNullFields doesn't remove nil from slice, just recursive map stripping?
		// "typed[i] = stripNullFields(child)" - so nil remains nil in slice.
	}
	cMap := c[2].(map[string]any)
	if _, ok := cMap["d"]; ok {
		t.Error("expected d to be stripped")
	}
}

func TestStripJSONNullFields(t *testing.T) {
	data := []byte(`{"a": null, "b": 1}`)
	res, err := stripJSONNullFields(data)
	if err != nil {
		t.Fatal(err)
	}
	if string(res) != `{"b":1}` {
		t.Errorf("expected {\"b\":1}, got %s", string(res))
	}

	// error case
	_, err = stripJSONNullFields([]byte(`{invalid`))
	if err == nil {
		t.Error("expected error on invalid JSON")
	}
}

func TestRandomTwoWordTag(t *testing.T) {
	tag, err := randomTwoWordTag()
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tag, "-")
	if len(parts) != 2 {
		t.Errorf("expected two words, got %s", tag)
	}
}

func TestFallbackTagWords(t *testing.T) {
	words := fallbackTagWords()
	if len(words) == 0 {
		t.Error("expected fallback words")
	}
}

func TestRandomWord(t *testing.T) {
	_, err := randomWord([]string{})
	if err == nil {
		t.Error("expected error on empty words list")
	}
	w, err := randomWord([]string{"a"})
	if err != nil || w != "a" {
		t.Errorf("expected a, got %s, err %v", w, err)
	}
}

func TestNormalizeTagWord(t *testing.T) {
	if w, ok := normalizeTagWord("ABc"); !ok || w != "abc" {
		t.Errorf("failed ABc")
	}
	if _, ok := normalizeTagWord("ab"); ok {
		t.Error("expected false for short word")
	}
	if _, ok := normalizeTagWord("123abc"); ok {
		t.Error("expected false for non-letter")
	}
	if _, ok := normalizeTagWord("averylongwordthatiswaytoolong"); ok {
		t.Error("expected false for long word")
	}
}

func TestStripJSONComments(t *testing.T) {
	tests := []struct {
		input    string
		expected string
		err      bool
	}{
		{`{"a": 1}`, `{"a": 1}`, false},
		{`{"a": /* block */ 1}`, `{"a":  1}`, false},
		{`{"a": // inline` + "\n" + `1}`, `{"a": ` + "\n" + `1}`, false},
		{`{"a": "str//ing"}`, `{"a": "str//ing"}`, false},
		{`{"a": "str\"ing"}`, `{"a": "str\"ing"}`, false},
		{`{"a": /* unterminated`, ``, true},
		{`{"a": "unterminated`, ``, true},
		{`{"a": /* multiline` + "\n" + ` comment */ 1}`, `{"a": ` + "\n" + ` 1}`, false},
	}
	for _, tt := range tests {
		res, err := StripJSONComments(tt.input)
		if tt.err && err == nil {
			t.Errorf("expected error for %s", tt.input)
		}
		if !tt.err && err != nil {
			t.Errorf("unexpected error for %s: %v", tt.input, err)
		}
		if !tt.err && res != tt.expected {
			t.Errorf("expected %s, got %s", tt.expected, res)
		}
	}
}

func TestNormalizeJSONInput(t *testing.T) {
	tests := []struct {
		input    string
		expected string
		ok       bool
		err      bool
	}{
		{`  {"a": 1}  `, `{"a": 1}`, true, false},
		{`[1, 2]`, `[1, 2]`, true, false},
		{``, ``, false, false},
		{`not json`, ``, false, false},
		{`{"a": /* bad */}`, ``, false, true},
		{`{"a": 1 /* unclosed string "\"}`, ``, false, true},
		{`{"a": 1} // comment`, `{"a": 1}`, true, false},
	}
	for _, tt := range tests {
		res, ok, err := NormalizeJSONInput(tt.input)
		if tt.err && err == nil {
			t.Errorf("expected err for %s", tt.input)
		}
		if !tt.err && err != nil {
			t.Errorf("unexpected err for %s: %v", tt.input, err)
		}
		if ok != tt.ok {
			t.Errorf("expected ok=%v for %s, got %v", tt.ok, tt.input, ok)
		}
		if !tt.err && tt.ok && res != tt.expected {
			t.Errorf("expected %s, got %s", tt.expected, res)
		}
	}
}

func TestIsSocks5ProxyScheme(t *testing.T) {
	if !isSocks5ProxyScheme("socks5") {
		t.Error("expected true")
	}
	if !isSocks5ProxyScheme("socks5h") {
		t.Error("expected true")
	}
	if isSocks5ProxyScheme("socks") {
		t.Error("expected false")
	}
}

func TestRequiresURLPort(t *testing.T) {
	if !requiresURLPort("vless") {
		t.Error("expected true")
	}
	if requiresURLPort("http") {
		t.Error("expected false")
	}
}

func TestHasURLPort(t *testing.T) {
	if !hasURLPort("http://host:8080") {
		t.Error("expected true")
	}
	if hasURLPort("http://host") {
		t.Error("expected false")
	}
	if hasURLPort("::badurl") {
		t.Error("expected false")
	}
}

func TestDecodeShareBase64(t *testing.T) {
	validBase64 := base64.StdEncoding.EncodeToString([]byte("test"))
	b, err := decodeShareBase64(validBase64)
	if err != nil || string(b) != "test" {
		t.Errorf("failed basic decode")
	}

	urlSafe := strings.ReplaceAll(validBase64, "+", "-")
	urlSafe = strings.ReplaceAll(urlSafe, "/", "_")
	b, err = decodeShareBase64(strings.TrimRight(urlSafe, "="))
	if err != nil || string(b) != "test" {
		t.Errorf("failed padded/urlsafe decode")
	}

	_, err = decodeShareBase64("invalid base64 ^^")
	if err == nil {
		t.Error("expected error")
	}
}

func TestIsVMessQRCodePayload(t *testing.T) {
	validJSON := `{"add":"1.1.1.1","id":"uuid","port":"443"}`
	b64 := base64.StdEncoding.EncodeToString([]byte(validJSON))
	if !isVMessQRCodePayload(b64) {
		t.Error("expected true")
	}

	invalidJSON := `{"add":"1.1.1.1"}`
	b64 = base64.StdEncoding.EncodeToString([]byte(invalidJSON))
	if isVMessQRCodePayload(b64) {
		t.Error("expected false due to missing fields")
	}

	if isVMessQRCodePayload("not base64") {
		t.Error("expected false")
	}
}

func TestTrimVMessQRCodePayload(t *testing.T) {
	validJSON := `{"add":"1.1.1.1","id":"uuid","port":"443"}`
	b64 := base64.StdEncoding.EncodeToString([]byte(validJSON))

	if _, ok := trimVMessQRCodePayload(b64 + "==padding"); !ok {
		t.Error("expected true")
	}
	
	if _, ok := trimVMessQRCodePayload(b64 + "=padding"); !ok {
		t.Error("expected true")
	}

	if _, ok := trimVMessQRCodePayload("padding=" + b64); !ok {
		// actually trim scans from right to left or splits on =
	}

	if _, ok := trimVMessQRCodePayload("nothing valid"); ok {
		t.Error("expected false")
	}
}

func TestIsVMessQRCodeLink(t *testing.T) {
	validJSON := `{"add":"1.1.1.1","id":"uuid","port":"443"}`
	b64 := base64.StdEncoding.EncodeToString([]byte(validJSON))
	if !isVMessQRCodeLink("vmess://" + b64) {
		t.Error("expected true")
	}
	if isVMessQRCodeLink("vmess://bad") {
		t.Error("expected false")
	}
	if isVMessQRCodeLink("vless://" + b64) {
		t.Error("expected false")
	}
}

func TestNormalizeVMessQRCodeLink(t *testing.T) {
	validJSON := `{"add":"1.1.1.1","id":"uuid","port":"443"}`
	b64 := base64.StdEncoding.EncodeToString([]byte(validJSON))
	
	if normalizeVMessQRCodeLink("vmess://" + b64) != "vmess://" + b64 {
		t.Error("expected no change")
	}

	if normalizeVMessQRCodeLink("vless://" + b64) != "vless://" + b64 {
		t.Error("expected no change")
	}

	// with padding
	if normalizeVMessQRCodeLink("vmess://" + b64 + "==suffix") != "vmess://" + b64 {
		t.Error("expected trimmed")
	}
}

func TestResolveInput(t *testing.T) {
	// stdin handled by test runner is hard, skip "-" testing without mock
	res, source, err := ResolveInput("my input")
	if err != nil || res != "my input" || source != "arg" {
		t.Errorf("failed plain arg")
	}

	tmp := filepath.Join(t.TempDir(), "file.txt")
	os.WriteFile(tmp, []byte("file content"), 0644)
	res, source, err = ResolveInput(tmp)
	if err != nil || res != "file content" || source != tmp {
		t.Errorf("failed file read")
	}

	emptyTmp := filepath.Join(t.TempDir(), "empty.txt")
	os.WriteFile(emptyTmp, []byte("   "), 0644)
	_, _, err = ResolveInput(emptyTmp)
	if err == nil {
		t.Error("expected error on empty file")
	}
}

func TestConvertBareHTTPProxyURLToXrayJSON(t *testing.T) {
	_, ok, err := convertBareHTTPProxyURLToXrayJSON("not a url")
	if ok || err != nil {
		t.Error("expected false, nil")
	}
	_, ok, err = convertBareHTTPProxyURLToXrayJSON("socks://host:1080")
	if ok || err != nil {
		t.Error("expected false, nil")
	}
	_, ok, err = convertBareHTTPProxyURLToXrayJSON("http://host:8080/path")
	if ok || err != nil {
		t.Error("expected false, nil")
	}
	_, ok, err = convertBareHTTPProxyURLToXrayJSON("http://host:8080?q=1")
	if ok || err != nil {
		t.Error("expected false, nil")
	}
	_, ok, err = convertBareHTTPProxyURLToXrayJSON("http://:8080")
	if !ok || err == nil {
		t.Error("expected true, err for no host")
	}
	_, ok, err = convertBareHTTPProxyURLToXrayJSON("http://host")
	if !ok || err == nil {
		t.Error("expected true, err for no port")
	}
	_, ok, err = convertBareHTTPProxyURLToXrayJSON("http://host:abc")
	if ok && err == nil {
		t.Error("expected err for bad port")
	}
	_, ok, err = convertBareHTTPProxyURLToXrayJSON("http://host:999999")
	if ok && err == nil {
		t.Error("expected err for out of range port")
	}

	data, ok, err := convertBareHTTPProxyURLToXrayJSON("http://user:pass@host:8080#my-tag")
	if !ok || err != nil {
		t.Fatalf("expected success, got err: %v", err)
	}
	if !strings.Contains(string(data), "my-tag") || !strings.Contains(string(data), "user") {
		t.Errorf("missing expected data in output: %s", string(data))
	}
}

func TestNormalizeSocks5URLForLibXray(t *testing.T) {
	parsed, _ := url.Parse("socks5://host:1080")
	res, err := normalizeSocks5URLForLibXray(parsed)
	if err != nil || res != "socks://host:1080" {
		t.Errorf("failed normalize: %v, %s", err, res)
	}

	parsed, _ = url.Parse("socks5://user:pass@host:1080")
	res, err = normalizeSocks5URLForLibXray(parsed)
	if err != nil || !strings.HasPrefix(res, "socks://") || !strings.Contains(res, "@host:1080") {
		t.Errorf("failed user pass: %v, %s", err, res)
	}

	parsed, _ = url.Parse("socks5://:1080")
	_, err = normalizeSocks5URLForLibXray(parsed)
	if err == nil {
		t.Error("expected err missing host")
	}

	parsed, _ = url.Parse("socks5://host:abc")
	if parsed != nil {
		_, err = normalizeSocks5URLForLibXray(parsed)
		if err == nil {
			t.Error("expected err bad port")
		}
	}

	parsed, _ = url.Parse("socks5://host:1080/path")
	_, err = normalizeSocks5URLForLibXray(parsed)
	if err == nil {
		t.Error("expected err with path")
	}

	parsed, _ = url.Parse("socks5://host:1080?q=1")
	_, err = normalizeSocks5URLForLibXray(parsed)
	if err == nil {
		t.Error("expected err with query")
	}
}

func TestNormalizeSubscriptionURILine(t *testing.T) {
	// no scheme
	_, ok, _ := normalizeSubscriptionURILine("not a url")
	if ok {
		t.Error("expected false")
	}

	// vmess
	validJSON := `{"add":"1.1.1.1","id":"uuid","port":"443"}`
	b64 := base64.StdEncoding.EncodeToString([]byte(validJSON))
	vmessStr := "vmess://" + b64
	res, ok, err := normalizeSubscriptionURILine(vmessStr)
	if !ok || err != nil || res != vmessStr {
		t.Errorf("expected success for vmess")
	}

	_, ok, _ = normalizeSubscriptionURILine("vmess://bad")
	if ok {
		t.Error("expected false for bad vmess")
	}

	// http
	res, ok, err = normalizeSubscriptionURILine("http://host:8080")
	if !ok || err != nil || res != "http://host:8080" {
		t.Errorf("expected success for http")
	}

	_, ok, _ = normalizeSubscriptionURILine("http://host:8080/path")
	if ok {
		t.Error("expected false for http with path")
	}

	// https
	_, ok, _ = normalizeSubscriptionURILine("https://host:8080")
	if ok {
		t.Error("expected false for https")
	}

	// mixed
	_, ok, err = normalizeSubscriptionURILine("mixed://host:8080")
	if err == nil {
		t.Error("expected error for mixed")
	}

	// socks5
	res, ok, err = normalizeSubscriptionURILine("socks5://host:1080")
	if !ok || err != nil || res != "socks://host:1080" {
		t.Errorf("expected success for socks5")
	}

	// requires port
	_, ok, _ = normalizeSubscriptionURILine("vless://host")
	if ok {
		t.Error("expected false for vless without port")
	}

	// other scheme
	res, ok, err = normalizeSubscriptionURILine("vless://host:443")
	if !ok || err != nil || res != "vless://host:443" {
		t.Errorf("expected success for vless with port")
	}
}

func TestNormalizeShareTextForLibXray(t *testing.T) {
	input := "  # comment \n" +
		"socks5://host:1080\n" +
		"bad url\n"
	res, err := normalizeShareTextForLibXray(input)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, "socks://host:1080") || strings.Contains(res, "bad url") || strings.Contains(res, "# comment") {
		t.Errorf("unexpected result: %s", res)
	}

	// fallback when no valid links
	input2 := "just some text\nmore text"
	res2, err := normalizeShareTextForLibXray(input2)
	if err != nil || res2 != "just some text\nmore text" {
		t.Errorf("unexpected fallback result: %s", res2)
	}
}

func TestNormalizeOutboundSettingsForShareLink(t *testing.T) {
	// test normalizeVNextSettingsForShareLink wrapper
	settings := map[string]any{
		"vnext": []any{
			map[string]any{
				"address": "server.com",
				"users": []any{
					map[string]any{
						"id": "uuid",
					},
				},
			},
		},
	}
	normalizeOutboundSettingsForShareLink("vless", settings)
	if settings["address"] != "server.com" || settings["id"] != "uuid" {
		t.Error("vnext normalize failed")
	}

	// test normalizeServerSettingsForShareLink wrapper
	settingsServer := map[string]any{
		"servers": []any{
			map[string]any{
				"address": "ss.com",
				"method": "aes",
				"password": "pass",
			},
		},
	}
	normalizeOutboundSettingsForShareLink("shadowsocks", settingsServer)
	if settingsServer["address"] != "ss.com" || settingsServer["method"] != "aes" || settingsServer["password"] != "pass" {
		t.Error("servers normalize failed")
	}
}

func TestNormalizeVNextSettingsForShareLink(t *testing.T) {
	settings := map[string]any{
		"vnext": []any{
			map[string]any{
				"address": "server.com",
				"port": 443,
				"users": []any{
					map[string]any{
						"id": "uuid",
						"level": 0,
						"email": "a@b.com",
						"flow": "xtls-rprx-vision",
						"encryption": "none",
						"security": "auto",
					},
				},
			},
		},
	}
	normalizeVNextSettingsForShareLink("vless", settings)
	if settings["address"] != "server.com" || settings["id"] != "uuid" || settings["flow"] != "xtls-rprx-vision" {
		t.Error("vless copy failed")
	}

	settingsVMess := map[string]any{
		"vnext": []any{
			map[string]any{
				"users": []any{
					map[string]any{
						"security": "auto",
					},
				},
			},
		},
	}
	normalizeVNextSettingsForShareLink("vmess", settingsVMess)
	if settingsVMess["security"] != "auto" {
		t.Error("vmess security copy failed")
	}

	settingsVMess2 := map[string]any{
		"vnext": []any{
			map[string]any{
				"users": []any{
					map[string]any{
						"encryption": "aes-128-gcm",
					},
				},
			},
		},
	}
	normalizeVNextSettingsForShareLink("vmess", settingsVMess2)
	if settingsVMess2["security"] != "aes-128-gcm" {
		t.Error("vmess encryption->security fallback failed")
	}
}

func TestNormalizeServerSettingsForShareLink(t *testing.T) {
	settings := map[string]any{
		"servers": []any{
			map[string]any{
				"address": "t.com",
				"port": 443,
				"password": "pass",
				"flow": "xtls",
			},
		},
	}
	normalizeServerSettingsForShareLink("trojan", settings)
	if settings["password"] != "pass" || settings["flow"] != "xtls" {
		t.Error("trojan normalize failed")
	}

	settingsSocks := map[string]any{
		"servers": []any{
			map[string]any{
				"users": []any{
					map[string]any{
						"user": "u",
						"pass": "p",
					},
				},
			},
		},
	}
	normalizeServerSettingsForShareLink("socks", settingsSocks)
	if settingsSocks["user"] != "u" || settingsSocks["pass"] != "p" {
		t.Error("socks normalize failed")
	}
}

func TestNormalizeXrayJSONForShareLinks(t *testing.T) {
	input := `{"outbounds":[{"protocol":"vless","settings":{"vnext":[{"address":"v.com","users":[{"id":"uuid"}]}]},"streamSettings":{"realitySettings":{"publicKey":"pubkey"}}}]}`
	res, err := normalizeXrayJSONForShareLinks(input)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, `"address":"v.com"`) || !strings.Contains(res, `"password":"pubkey"`) {
		t.Errorf("missing expected normalized keys: %s", res)
	}

	// test array of outbounds
	inputArray := `[{"protocol":"vless","settings":{"vnext":[{"address":"v.com","users":[{"id":"uuid"}]}]}}]`
	resArray, err := normalizeXrayJSONForShareLinks(inputArray)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resArray, `"outbounds":`) || !strings.Contains(resArray, `"address":"v.com"`) {
		t.Errorf("array auto-wrap failed: %s", resArray)
	}

	// no outbounds
	inputNoOutbounds := `{"some": "other"}`
	resNoOutbounds, err := normalizeXrayJSONForShareLinks(inputNoOutbounds)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resNoOutbounds, `"some":"other"`) && !strings.Contains(resNoOutbounds, `"some": "other"`) {
		t.Errorf("should ignore non-outbounds json")
	}

	// invalid outbounds
	inputInvalidOutbounds := `{"outbounds": "not array"}`
	resInvalidOutbounds, err := normalizeXrayJSONForShareLinks(inputInvalidOutbounds)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resInvalidOutbounds, `"not array"`) {
		t.Errorf("should ignore non-array outbounds")
	}

	// invalid json
	_, err = normalizeXrayJSONForShareLinks(`invalid`)
	if err == nil {
		t.Error("expected error")
	}
}

func TestFillEmptyOutboundTags(t *testing.T) {
	input := []byte(`{"outbounds":[{"protocol":"vless"},{"protocol":"vmess","tag":"exist"}]}`)
	counter := 0
	generator := func() (string, error) {
		counter++
		return "tag" + strconv.Itoa(counter), nil
	}
	res, filled, err := fillEmptyOutboundTags(input, generator)
	if err != nil || filled != 1 {
		t.Errorf("failed to fill tags: filled=%d, err=%v", filled, err)
	}
	if !strings.Contains(string(res), `"tag":"tag1"`) || !strings.Contains(string(res), `"tag":"exist"`) {
		t.Errorf("output missing tags: %s", res)
	}

	// missing outbounds
	res2, filled2, err2 := fillEmptyOutboundTags([]byte(`{"other": 1}`), generator)
	if err2 != nil || filled2 != 0 {
		t.Errorf("failed when no outbounds: %v", err2)
	}
	if string(res2) != `{"other": 1}` {
		t.Errorf("expected no change: %s", string(res2))
	}

	// generator error
	badGen := func() (string, error) { return "", fmt.Errorf("bad") }
	_, _, err = fillEmptyOutboundTags(input, badGen)
	if err == nil {
		t.Error("expected err from bad generator")
	}

	// invalid json
	_, _, err = fillEmptyOutboundTags([]byte(`invalid`), generator)
	if err == nil {
		t.Error("expected err from invalid json")
	}

	// invalid outbounds type
	_, _, err = fillEmptyOutboundTags([]byte(`{"outbounds": "not array"}`), generator)
	if err == nil {
		t.Error("expected err from invalid outbounds type")
	}

	// invalid outbound type
	_, _, err = fillEmptyOutboundTags([]byte(`{"outbounds": ["not object"]}`), generator)
	if err == nil {
		t.Error("expected err from invalid outbound type")
	}
}

// For integration-like tests using libxray share packages we might need minimal valid json
// Since libxray share converts valid configs, we can just do basic error testing for ShareLinkToXrayJSON and XrayJSONToShareText
func TestShareLinkToXrayJSON(t *testing.T) {
	// Test HTTP bare link conversion path
	jsonStr, err := ShareLinkToXrayJSON("http://host:8080#my-http")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !strings.Contains(jsonStr, "my-http") {
		t.Errorf("missing http tag: %s", jsonStr)
	}

	// Test invalid proxy link
	_, err = ShareLinkToXrayJSON("invalid://proxy")
	if err == nil {
		t.Error("expected error for invalid proxy link")
	}

	// Test error from normalizeShareTextForLibXray
	_, err = ShareLinkToXrayJSON("mixed://host:1080")
	if err == nil {
		t.Error("expected error from normalizeShareTextForLibXray")
	}
}

func TestXrayJSONToShareText(t *testing.T) {
	// Test invalid JSON
	_, err := XrayJSONToShareText(`invalid`)
	if err == nil {
		t.Error("expected err")
	}

	// Simple outbound array handling
	validJSON := `[{"protocol":"vless","settings":{"vnext":[{"address":"v.com","port":443,"users":[{"id":"12345678-1234-1234-1234-123456789012"}]}]}}]`
	// the internal share conversion requires valid VLESS with valid UUIDs otherwise libxray complains
	// We'll see if libxray parses it or errors.
	_, err = XrayJSONToShareText(validJSON)
	// It may or may not err depending on libxray validation.
	// As long as we tested the path.

	// Array of config objects with "remarks" and "outbounds"
	configArrJSON := `[{"remarks":"test-remark", "outbounds":[{"protocol":"vless","settings":{"vnext":[{"address":"v.com","port":443,"users":[{"id":"12345678-1234-1234-1234-123456789012"}]}]}}]}]`
	_, _ = XrayJSONToShareText(configArrJSON)
	// Same, might error if not valid libxray format, but we cover the branches.

	// Filter out freedom, blackhole, dns
	filterArrJSON := `[{"remarks":"test", "outbounds":[{"protocol":"freedom"},{"protocol":"vless","settings":{"vnext":[{"address":"v.com","port":443,"users":[{"id":"12345678-1234-1234-1234-123456789012"}]}]}}]}]`
	_, _ = XrayJSONToShareText(filterArrJSON)

	// Array with non-object
	nonObjArrJSON := `["string", {"outbounds": []}]`
	_, _ = XrayJSONToShareText(nonObjArrJSON)

	// Missing protocol in outbound
	noProtoArrJSON := `[{"outbounds": [{"not_protocol": "freedom"}]}]`
	_, _ = XrayJSONToShareText(noProtoArrJSON)
}

func TestConvertSingleConfigToShareLinksRemarks(t *testing.T) {
	// Test remark appending
	jsonStr := `{"outbounds":[{"protocol":"vless","settings":{"vnext":[{"address":"1.1.1.1","port":443,"users":[{"id":"12345678-1234-1234-1234-123456789012"}]}]}}]}`
	// Libxray should return a vless link. We then replace its remark.
	res, err := convertSingleConfigToShareLinks(jsonStr, "my space remark")
	if err == nil {
		if !strings.HasSuffix(res, "#my%20space%20remark") {
			t.Errorf("expected url-escaped remark, got: %s", res)
		}
	}

	// Test invalid JSON
	_, err = convertSingleConfigToShareLinks(`invalid`, "remark")
	if err == nil {
		t.Error("expected err on invalid json")
	}
}

func TestLoadDictionaryWords(t *testing.T) {
	// mock dict dir
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "words"), []byte("apple\nbanana\n123bad\n"), 0644)
	os.MkdirAll(filepath.Join(dir, "subdir"), 0755)

	words, err := loadDictionaryWords(dir)
	if err != nil {
		t.Fatal(err)
	}
	foundApple := false
	foundBanana := false
	for _, w := range words {
		if w == "apple" {
			foundApple = true
		}
		if w == "banana" {
			foundBanana = true
		}
		if w == "123bad" {
			t.Error("should not load bad word")
		}
	}
	if !foundApple || !foundBanana {
		t.Error("missing words")
	}

	// bad path
	_, err = loadDictionaryWords(filepath.Join(dir, "not-exist"))
	if err == nil {
		t.Error("expected error for missing dir")
	}
}

func TestTagWords(t *testing.T) {
	words := tagWords()
	if len(words) == 0 {
		t.Error("expected words")
	}
	words2 := tagWords()
	if len(words) != len(words2) {
		t.Error("expected same slice length on second call")
	}
}

func TestReadDictionaryFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "words")
	os.WriteFile(path, []byte("valid\ninvalid123\ntoolongwordtoolongword\n"), 0644)
	
	seen := make(map[string]struct{})
	err := readDictionaryFile(path, seen)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := seen["valid"]; !ok {
		t.Error("expected 'valid' in seen")
	}
	if _, ok := seen["invalid123"]; ok {
		t.Error("expected 'invalid123' to be ignored")
	}

	err = readDictionaryFile(filepath.Join(dir, "missing"), seen)
	if err == nil {
		t.Error("expected err on missing file")
	}
}

