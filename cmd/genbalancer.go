package cmd

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"xraytool/internal/safeio"

	"github.com/spf13/cobra"
)

const defaultSubURL = "https://github.com/igareck/vpn-configs-for-russia/raw/refs/heads/main/WHITE-CIDR-RU-all.txt"

// ---------------------------------------------------------------------------
// Balancer config JSON structures
// ---------------------------------------------------------------------------

type balancerConfig struct {
	BurstObservatory burstObservatory `json:"burstObservatory"`
	DNS              balancerDNS      `json:"dns"`
	Inbounds         []any            `json:"inbounds"`
	Log              balancerLog      `json:"log"`
	Outbounds        []any            `json:"outbounds"`
	Remarks          string           `json:"remarks"`
	Routing          balancerRouting  `json:"routing"`
}

type burstObservatory struct {
	PingConfig      pingConfig `json:"pingConfig"`
	SubjectSelector []string   `json:"subjectSelector"`
}

type pingConfig struct {
	Destination string `json:"destination"`
	Interval    string `json:"interval"`
	Sampling    int    `json:"sampling"`
	Timeout     string `json:"timeout"`
}

type balancerDNS struct {
	QueryStrategy string   `json:"queryStrategy"`
	Servers       []string `json:"servers"`
}

type balancerLog struct {
	Access   string `json:"access"`
	DNSLog   bool   `json:"dnsLog"`
	LogLevel string `json:"loglevel"`
}

type balancerRouting struct {
	Balancers      []routingBalancer `json:"balancers"`
	DomainMatcher  string            `json:"domainMatcher"`
	DomainStrategy string            `json:"domainStrategy"`
	Rules          []any             `json:"rules"`
}

type routingBalancer struct {
	FallbackTag string          `json:"fallbackTag"`
	Selector    []string        `json:"selector"`
	Strategy    routingStrategy `json:"strategy"`
	Tag         string          `json:"tag"`
}

type routingStrategy struct {
	Settings routingStrategySettings `json:"settings"`
	Type     string                  `json:"type"`
}

type routingStrategySettings struct {
	Baselines []string `json:"baselines"`
	Expected  int      `json:"expected"`
	MaxRTT    string   `json:"maxRTT"`
	Tolerance float64  `json:"tolerance"`
}

// ---------------------------------------------------------------------------
// Command
// ---------------------------------------------------------------------------

func genBalancerCmd() *cobra.Command {
	var (
		subURL     string
		remarks    string
		output     string
		upsertInto string
		upsertSub  bool
	)

	cmd := &cobra.Command{
		Use:   "genbalancer",
		Short: "Download a subscription file, parse VPN links and generate a balancer config JSON",
		Long: `Downloads a subscription file containing vless://, vmess://, trojan://, ss:// links,
parses each link into an xray outbound JSON object, assigns AT_001..AT_NNN tags,
and prints a complete balancer config to stdout (or file with -o).

With --upsert-into, the generated config is inserted into (or replaces an existing
entry in) a JSON subscription array file, matched by the "remarks" field.

With --upsert-sub, the path is taken automatically from the tool config
(paths.json_subscription_template).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Resolve --upsert-sub → path from config
			effectiveUpsert := upsertInto
			if upsertSub {
				if cfg == nil {
					return fmt.Errorf("--upsert-sub requires a valid config file (use --config)")
				}
				if effectiveUpsert == "" {
					effectiveUpsert = cfg.Paths.JSONSubscriptionTemplate
				}
				if effectiveUpsert == "" {
					return fmt.Errorf("--upsert-sub: paths.json_subscription_template is not set in config")
				}
				fmt.Fprintf(os.Stderr, "Using subscription file from config: %s\n", effectiveUpsert)
			}
			return runGenBalancer2(subURL, remarks, output, effectiveUpsert)
		},
	}

	cmd.Flags().StringVarP(&subURL, "url", "u", defaultSubURL, "Subscription URL to download VPN configs from")
	cmd.Flags().StringVar(&remarks, "remarks", "🇪🇺 БАЛАНСЕР", "Remarks field for the config")
	cmd.Flags().StringVarP(&output, "output", "o", "", "Output file (default: stdout)")
	cmd.Flags().StringVarP(&upsertInto, "upsert-into", "s", "", "JSON subscription array file to upsert the balancer config into (add or replace by remarks)")
	cmd.Flags().BoolVar(&upsertSub, "upsert-sub", false, "Upsert into the subscription file configured in paths.json_subscription_template")

	return cmd
}

func runGenBalancer2(subURL, remarks, outputFile, upsertInto string) error {
	// 1. Download subscription
	fmt.Fprintf(os.Stderr, "Downloading subscription from %s\n", subURL)
	lines, err := fetchLines(subURL)
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Downloaded %d lines\n", len(lines))

	// 2. Parse VPN links into outbounds
	var atTags []string
	var atOutbounds []any

	idx := 1
	skipped := 0
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		ob, err := parseLinkToOutbound(line)
		if err != nil {
			skipped++
			fmt.Fprintf(os.Stderr, "  skip [%s]: %v\n", truncate(line, 60), err)
			continue
		}
		tag := fmt.Sprintf("AT_%03d", idx)
		ob["tag"] = tag
		atTags = append(atTags, tag)
		atOutbounds = append(atOutbounds, ob)
		idx++
	}

	if len(atTags) == 0 {
		return fmt.Errorf("no valid VPN links found in subscription (skipped %d)", skipped)
	}
	fmt.Fprintf(os.Stderr, "Parsed %d outbound(s), skipped %d\n", len(atTags), skipped)

	// 3. Build balancer config
	balCfg := buildBalancerConfig2(atTags, atOutbounds, remarks)

	// 4. Marshal
	data, err := json.MarshalIndent(balCfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal error: %w", err)
	}

	// 5. Upsert into subscription file if requested
	if upsertInto != "" {
		if err := upsertBalancerIntoSubFile(upsertInto, remarks, data); err != nil {
			return fmt.Errorf("upsert failed: %w", err)
		}
	}

	// 6. Write standalone output (can be combined with upsert)
	if outputFile != "" {
		if err := safeio.WriteToFile(outputFile, data, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", outputFile, err)
		}
		fmt.Fprintf(os.Stderr, "Written to %s\n", outputFile)
	} else if upsertInto == "" {
		fmt.Println(string(data))
	}
	return nil
}

// ---------------------------------------------------------------------------
// Fetch helpers
// ---------------------------------------------------------------------------

func fetchLines(rawURL string) ([]string, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(rawURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	const maxBodyBytes = 10 * 1024 * 1024
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, err
	}

	// Some subscriptions are base64-encoded
	text := string(body)
	if decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(text)); err == nil && isPrintable(decoded) {
		text = string(decoded)
	} else if decoded, err := base64.RawStdEncoding.DecodeString(strings.TrimSpace(text)); err == nil && isPrintable(decoded) {
		text = string(decoded)
	}

	return strings.Split(text, "\n"), nil
}

func isPrintable(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	count := 0
	for _, c := range b {
		if c == '\n' || c == '\r' || c == '\t' || (c >= 0x20 && c <= 0x7e) {
			count++
		}
	}
	return float64(count)/float64(len(b)) > 0.8
}

// ---------------------------------------------------------------------------
// VPN link → xray outbound parser
// ---------------------------------------------------------------------------

// parseLinkToOutbound converts a VPN URI string to an xray outbound map.
func parseLinkToOutbound(link string) (map[string]any, error) {
	switch {
	case strings.HasPrefix(link, "vless://"):
		return parseVless(link)
	case strings.HasPrefix(link, "vmess://"):
		return parseVmess(link)
	case strings.HasPrefix(link, "trojan://"):
		return parseTrojan(link)
	case strings.HasPrefix(link, "ss://"):
		return parseSS(link)
	case strings.HasPrefix(link, "hysteria2://"), strings.HasPrefix(link, "hy2://"):
		return parseHysteria2(link)
	default:
		return nil, fmt.Errorf("unsupported protocol")
	}
}

// ---- VLESS ----

func parseVless(raw string) (map[string]any, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	uuid := u.User.Username()
	host := u.Hostname()
	portStr := u.Port()
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		return nil, fmt.Errorf("invalid port: %q", portStr)
	}

	q := u.Query()
	flow := q.Get("flow")
	encryption := q.Get("encryption")
	if encryption == "" {
		encryption = "none"
	}

	user := map[string]any{
		"id":         uuid,
		"encryption": encryption,
	}
	if flow != "" {
		user["flow"] = flow
	}

	settings := map[string]any{
		"vnext": []any{
			map[string]any{
				"address": host,
				"port":    port,
				"users":   []any{user},
			},
		},
	}

	ss := buildStreamSettings(q)

	return map[string]any{
		"protocol":       "vless",
		"settings":       settings,
		"streamSettings": ss,
	}, nil
}

// ---- VMESS ----

type vmessJSON struct {
	Add  string `json:"add"`
	Port any    `json:"port"` // may be string or number
	ID   string `json:"id"`
	Aid  any    `json:"aid"`
	Net  string `json:"net"`
	Type string `json:"type"`
	Host string `json:"host"`
	Path string `json:"path"`
	TLS  string `json:"tls"`
	SNI  string `json:"sni"`
	FP   string `json:"fp"`
	ALPN string `json:"alpn"`
	Ps   string `json:"ps"`
	V    string `json:"v"`
	Scy  string `json:"scy"`
}

func parseVmess(raw string) (map[string]any, error) {
	b64 := strings.TrimPrefix(raw, "vmess://")
	// strip fragment
	if i := strings.Index(b64, "#"); i >= 0 {
		b64 = b64[:i]
	}
	var decoded []byte
	var err error
	decoded, err = base64.StdEncoding.DecodeString(b64)
	if err != nil {
		decoded, err = base64.RawStdEncoding.DecodeString(b64)
		if err != nil {
			return nil, fmt.Errorf("vmess base64 decode: %w", err)
		}
	}

	var v vmessJSON
	if err := json.Unmarshal(decoded, &v); err != nil {
		return nil, fmt.Errorf("vmess json: %w", err)
	}

	port := toInt(v.Port)
	aid := toInt(v.Aid)

	user := map[string]any{
		"id":       v.ID,
		"alterId":  aid,
		"security": "auto",
	}
	if v.Scy != "" {
		user["security"] = v.Scy
	}

	settings := map[string]any{
		"vnext": []any{
			map[string]any{
				"address": v.Add,
				"port":    port,
				"users":   []any{user},
			},
		},
	}

	q := url.Values{}
	q.Set("type", v.Net)
	q.Set("security", v.TLS)
	q.Set("host", v.Host)
	q.Set("path", v.Path)
	q.Set("sni", v.SNI)
	q.Set("fp", v.FP)
	q.Set("alpn", v.ALPN)
	if v.Type != "" {
		q.Set("headerType", v.Type)
	}
	ss := buildStreamSettings(q)

	return map[string]any{
		"protocol":       "vmess",
		"settings":       settings,
		"streamSettings": ss,
	}, nil
}

// ---- TROJAN ----

func parseTrojan(raw string) (map[string]any, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	password := u.User.Username()
	host := u.Hostname()
	portStr := u.Port()
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		return nil, fmt.Errorf("invalid port: %q", portStr)
	}

	q := u.Query()

	settings := map[string]any{
		"servers": []any{
			map[string]any{
				"address":  host,
				"port":     port,
				"password": password,
			},
		},
	}

	// trojan is always tls by default
	if q.Get("security") == "" {
		q.Set("security", "tls")
	}
	ss := buildStreamSettings(q)

	return map[string]any{
		"protocol":       "trojan",
		"settings":       settings,
		"streamSettings": ss,
	}, nil
}

// ---- SHADOWSOCKS ----

func parseSS(raw string) (map[string]any, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}

	host := u.Hostname()
	portStr := u.Port()
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		return nil, fmt.Errorf("invalid port: %q", portStr)
	}

	method := ""
	password := ""

	// userinfo may be base64(method:password) or plain method:password
	userinfo := u.User.String()
	if dec, err := base64.StdEncoding.DecodeString(userinfo); err == nil {
		userinfo = string(dec)
	} else if dec, err := base64.RawStdEncoding.DecodeString(userinfo); err == nil {
		userinfo = string(dec)
	}
	if parts := strings.SplitN(userinfo, ":", 2); len(parts) == 2 {
		method = parts[0]
		password = parts[1]
	} else {
		method = "none"
		password = userinfo
	}

	settings := map[string]any{
		"servers": []any{
			map[string]any{
				"address":  host,
				"port":     port,
				"method":   method,
				"password": password,
			},
		},
	}

	return map[string]any{
		"protocol": "shadowsocks",
		"settings": settings,
		"streamSettings": map[string]any{
			"network": "tcp",
		},
	}, nil
}

// ---- HYSTERIA2 ----

func parseHysteria2(raw string) (map[string]any, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	password := u.User.Username()
	host := u.Hostname()
	portStr := u.Port()
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		return nil, fmt.Errorf("invalid port: %q", portStr)
	}
	q := u.Query()
	sni := q.Get("sni")
	if sni == "" {
		sni = host
	}

	settings := map[string]any{
		"servers": []any{
			map[string]any{
				"address":  host,
				"port":     port,
				"password": password,
			},
		},
	}

	ss := map[string]any{
		"network":  "tcp",
		"security": "tls",
		"tlsSettings": map[string]any{
			"serverName":    sni,
			"allowInsecure": false,
		},
	}

	return map[string]any{
		"protocol":       "hysteria2",
		"settings":       settings,
		"streamSettings": ss,
	}, nil
}

// ---------------------------------------------------------------------------
// Stream settings builder
// ---------------------------------------------------------------------------

func buildStreamSettings(q url.Values) map[string]any {
	network := strings.ToLower(q.Get("type"))
	if network == "" || network == "raw" {
		network = "tcp"
	}
	security := strings.ToLower(q.Get("security"))

	ss := map[string]any{
		"network": network,
	}

	// Security layer
	switch security {
	case "reality":
		rs := map[string]any{}
		if fp := q.Get("fp"); fp != "" {
			rs["fingerprint"] = fp
		}
		if pbk := q.Get("pbk"); pbk != "" {
			rs["publicKey"] = pbk
		}
		if sni := q.Get("sni"); sni != "" {
			rs["serverName"] = sni
		}
		if sid := q.Get("sid"); sid != "" {
			rs["shortId"] = sid
		}
		ss["security"] = "reality"
		ss["realitySettings"] = rs

	case "tls":
		ts := map[string]any{}
		sni := q.Get("sni")
		if sni == "" {
			sni = q.Get("host")
		}
		if sni != "" {
			ts["serverName"] = sni
		}
		if fp := q.Get("fp"); fp != "" {
			ts["fingerprint"] = fp
		}
		if alpn := q.Get("alpn"); alpn != "" {
			ts["alpn"] = strings.Split(alpn, ",")
		}
		ss["security"] = "tls"
		ss["tlsSettings"] = ts
	}

	// Transport-specific settings
	switch network {
	case "ws":
		ws := map[string]any{}
		if path := q.Get("path"); path != "" {
			ws["path"] = path
		}
		host := q.Get("host")
		if host == "" {
			host = q.Get("sni")
		}
		if host != "" {
			ws["headers"] = map[string]any{"Host": host}
		}
		ss["wsSettings"] = ws

	case "grpc":
		gs := map[string]any{
			"serviceName": q.Get("serviceName"),
			"mode":        "gun",
		}
		if authority := q.Get("authority"); authority != "" {
			gs["authority"] = authority
		}
		ss["grpcSettings"] = gs

	case "xhttp", "splithttp":
		xs := map[string]any{}
		if path := q.Get("path"); path != "" {
			xs["path"] = path
		}
		if host := q.Get("host"); host != "" {
			xs["host"] = host
		}
		if mode := q.Get("mode"); mode != "" {
			xs["mode"] = mode
		}
		ss["xhttpSettings"] = xs
		ss["network"] = "xhttp"

	case "tcp":
		if ht := q.Get("headerType"); ht == "http" {
			host := q.Get("host")
			path := q.Get("path")
			ss["tcpSettings"] = map[string]any{
				"header": map[string]any{
					"type": "http",
					"request": map[string]any{
						"headers": map[string]any{"Host": []string{host}},
						"path":    []string{path},
					},
				},
			}
		}

	case "h2":
		h2 := map[string]any{}
		if host := q.Get("host"); host != "" {
			h2["host"] = []string{host}
		}
		if path := q.Get("path"); path != "" {
			h2["path"] = path
		}
		ss["httpSettings"] = h2
		ss["network"] = "h2"
	}

	return ss
}

// ---------------------------------------------------------------------------
// Build final balancer config
// ---------------------------------------------------------------------------

func buildBalancerConfig2(atTags []string, atOutbounds []any, remarks string) *balancerConfig {
	fixedOutbounds := []any{
		map[string]any{"protocol": "freedom", "tag": "proxy"},
		map[string]any{"protocol": "freedom", "tag": "direct"},
		map[string]any{"protocol": "blackhole", "tag": "block"},
	}
	outbounds := append(fixedOutbounds, atOutbounds...)

	rules := []any{
		map[string]any{
			"outboundTag": "direct",
			"protocol":    []string{"bittorrent"},
			"type":        "field",
		},
		map[string]any{
			"domain":      []string{`regexp:.*\.xn--p1ai$`},
			"outboundTag": "direct",
			"type":        "field",
		},
		map[string]any{
			"domain": []string{
				"geosite:private",
				"geoip:private",
				"domain:gosuslugi.ru",
				"domain:vk.com",
				"domain:yandex.ru",
				"domain:ok.ru",
			},
			"outboundTag": "direct",
		},
		map[string]any{
			"balancerTag": "AT_Balancer",
			"network":     "tcp,udp",
			"type":        "field",
		},
	}

	return &balancerConfig{
		BurstObservatory: burstObservatory{
			PingConfig: pingConfig{
				Destination: "http://www.gstatic.com/generate_204",
				Interval:    "1m",
				Sampling:    1,
				Timeout:     "3s",
			},
			SubjectSelector: atTags,
		},
		DNS: balancerDNS{
			QueryStrategy: "UseIP",
			Servers: []string{
				"77.88.8.8",
				"77.88.8.1",
				"1.1.1.1",
				"1.0.0.1",
				"localhost",
			},
		},
		Inbounds: []any{
			map[string]any{
				"listen":   "127.0.0.1",
				"port":     10808,
				"protocol": "socks",
				"settings": map[string]any{
					"auth": "noauth",
					"udp":  true,
				},
				"sniffing": map[string]any{
					"destOverride": []string{"http", "tls", "quic"},
					"enabled":      true,
					"routeOnly":    false,
				},
				"tag": "socks",
			},
			map[string]any{
				"listen":   "127.0.0.1",
				"port":     10809,
				"protocol": "http",
				"settings": map[string]any{
					"allowTransparent": false,
				},
				"sniffing": map[string]any{
					"destOverride": []string{"http", "tls", "quic"},
					"enabled":      true,
					"routeOnly":    false,
				},
				"tag": "http",
			},
		},
		Log: balancerLog{
			Access:   "",
			DNSLog:   true,
			LogLevel: "Warning",
		},
		Outbounds: outbounds,
		Remarks:   remarks,
		Routing: balancerRouting{
			Balancers: []routingBalancer{
				{
					FallbackTag: "direct",
					Selector:    atTags,
					Strategy: routingStrategy{
						Settings: routingStrategySettings{
							Baselines: []string{"1s"},
							Expected:  2,
							MaxRTT:    "1s",
							Tolerance: 0.01,
						},
						Type: "leastLoad",
					},
					Tag: "AT_Balancer",
				},
			},
			DomainMatcher:  "hybrid",
			DomainStrategy: "IPIfNonMatch",
			Rules:          rules,
		},
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func toInt(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case string:
		n, _ := strconv.Atoi(x)
		return n
	}
	return 0
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ---------------------------------------------------------------------------
// Upsert into JSON subscription array file
// ---------------------------------------------------------------------------

// upsertBalancerIntoSubFile reads a JSON file (possibly containing leading comment lines starting with #)
// that has either a JSON array of configs or a single JSON config as its main content.
// It finds an existing config where "remarks" == targetRemarks and replaces it,
// or appends the new config to the array. The file is written back atomically with headers preserved.
func upsertBalancerIntoSubFile(filePath, targetRemarks string, newConfigJSON []byte) error {
	// Parse the new config as raw JSON so we can embed it back exactly.
	var newEntry map[string]json.RawMessage
	if err := json.Unmarshal(newConfigJSON, &newEntry); err != nil {
		return fmt.Errorf("parse new config: %w", err)
	}

	// Read existing file (create empty array if it doesn't exist yet).
	existing, err := os.ReadFile(filePath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", filePath, err)
	}

	var headerText string
	var jsonText string

	if len(existing) > 0 {
		lines := strings.Split(string(existing), "\n")
		var headerLines []string
		var jsonLines []string
		isHeader := true
		for _, line := range lines {
			trimmedLine := strings.TrimSpace(line)
			if isHeader {
				if trimmedLine != "" && (strings.HasPrefix(trimmedLine, "{") || strings.HasPrefix(trimmedLine, "[")) {
					isHeader = false
					jsonLines = append(jsonLines, line)
				} else {
					headerLines = append(headerLines, line)
				}
			} else {
				jsonLines = append(jsonLines, line)
			}
		}
		headerText = strings.Join(headerLines, "\n")
		jsonText = strings.TrimSpace(strings.Join(jsonLines, "\n"))
	}

	var configs []map[string]json.RawMessage

	if jsonText != "" {
		// Replace unquoted routing placeholders with valid JSON objects so standard json.Unmarshal doesn't fail.
		placeholderRU := `{"_xraytool_placeholder_": "RU_ROUTING"}`
		placeholderGlobal := `{"_xraytool_placeholder_": "GLOBAL_ROUTING"}`

		preparedJSONText := jsonText
		preparedJSONText = strings.ReplaceAll(preparedJSONText, "{RU_ROUTING}", placeholderRU)
		preparedJSONText = strings.ReplaceAll(preparedJSONText, "{GLOBAL_ROUTING}", placeholderGlobal)

		if strings.HasPrefix(preparedJSONText, "[") {
			// Array format
			if err := json.Unmarshal([]byte(preparedJSONText), &configs); err != nil {
				return fmt.Errorf("parse JSON array: %w", err)
			}
		} else if strings.HasPrefix(preparedJSONText, "{") {
			// Single object — wrap in array
			var single map[string]json.RawMessage
			if err := json.Unmarshal([]byte(preparedJSONText), &single); err != nil {
				return fmt.Errorf("parse JSON object: %w", err)
			}
			configs = []map[string]json.RawMessage{single}
		} else {
			return fmt.Errorf("JSON body does not appear to be a JSON array or object")
		}
	}

	// Find existing entry index by remarks.
	foundIdx := -1
	for i, entry := range configs {
		raw, ok := entry["remarks"]
		if !ok {
			continue
		}
		var r string
		if err := json.Unmarshal(raw, &r); err != nil {
			continue
		}
		if r == targetRemarks {
			foundIdx = i
			break
		}
	}

	if foundIdx >= 0 {
		configs[foundIdx] = newEntry
		fmt.Fprintf(os.Stderr, "Replaced existing entry at index %d in %s (remarks: %q)\n", foundIdx, filePath, targetRemarks)
	} else {
		configs = append(configs, newEntry)
		fmt.Fprintf(os.Stderr, "Appended new entry to %s (remarks: %q)\n", filePath, targetRemarks)
	}

	// Write back
	out, err := json.MarshalIndent(configs, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal updated configs: %w", err)
	}

	// Replace the placeholders back to unquoted format
	reRU := regexp.MustCompile(`\{\s*"_xraytool_placeholder_"\s*:\s*"RU_ROUTING"\s*\}`)
	reGlobal := regexp.MustCompile(`\{\s*"_xraytool_placeholder_"\s*:\s*"GLOBAL_ROUTING"\s*\}`)

	outStr := reRU.ReplaceAllString(string(out), "{RU_ROUTING}")
	outStr = reGlobal.ReplaceAllString(outStr, "{GLOBAL_ROUTING}")

	// Build final output
	var finalContent strings.Builder
	if headerText != "" {
		finalContent.WriteString(headerText)
		if !strings.HasSuffix(headerText, "\n") {
			finalContent.WriteString("\n")
		}
	}
	finalContent.WriteString(outStr)
	finalContent.WriteString("\n")

	// Atomic write via temp file
	finalBytes := []byte(finalContent.String())
	if err := safeio.WriteToFile(filePath, finalBytes, 0o644); err != nil {
		return fmt.Errorf("write final file: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Subscription file updated: %s (%d entries total)\n", filePath, len(configs))
	return nil
}
