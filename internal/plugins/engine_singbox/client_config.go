package engine_singbox

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"xraytool/internal/pluginapi"
)

// BuildClientLinks derives native share links directly from the managed
// Sing-box JSON. It is intentionally independent of Xray RawConfig and is the
// second real ClientConfigContributor required for multi-engine subscriptions.
func (p *Plugin) BuildClientLinks(_ context.Context, user pluginapi.VPNUserConfig) ([]pluginapi.ClientLink, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.initialized {
		return nil, errors.New("engine_singbox: plugin is not initialised")
	}
	document, _, err := p.loadConfigLocked()
	if err != nil {
		return nil, err
	}

	var links []pluginapi.ClientLink
	var errs []error
	for _, inbound := range managedInbounds(document, p.cfg.ManagedInboundTags) {
		link, err := buildClientLink(p.cfg, inbound, user)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		links = append(links, link)
	}
	if len(links) == 0 && len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return links, errors.Join(errs...)
}

func buildClientLink(cfg pluginConfig, inbound map[string]any, supplied pluginapi.VPNUserConfig) (pluginapi.ClientLink, error) {
	protocol := strings.ToLower(mapString(inbound, "type"))
	port := inboundPort(inbound)
	if port <= 0 || port > 65535 {
		return pluginapi.ClientLink{}, fmt.Errorf("engine_singbox: inbound %q has invalid listen_port", mapString(inbound, "tag"))
	}
	host, err := clientHost(cfg, inbound)
	if err != nil {
		return pluginapi.ClientLink{}, err
	}
	user := supplied
	for _, record := range inboundUsers(inbound) {
		if userEmail(record) == supplied.Email {
			fromConfig := vpnUserFromRecord(record)
			user = mergeLinkUser(supplied, fromConfig)
			break
		}
	}
	label := firstNonEmpty(mapString(inbound, "tag"), "Sing-box "+protocol)
	hostPort := net.JoinHostPort(host, strconv.Itoa(port))

	switch protocol {
	case "vless":
		if user.UUID == "" {
			return pluginapi.ClientLink{}, fmt.Errorf("engine_singbox: VLESS inbound %q has no UUID for %q", label, user.Email)
		}
		query := url.Values{"encryption": {"none"}}
		applyTransportAndTLS(query, inbound)
		if user.Flow != "" {
			query.Set("flow", user.Flow)
		}
		return pluginapi.ClientLink{Protocol: protocol, URI: (&url.URL{
			Scheme: "vless", User: url.User(user.UUID), Host: hostPort, RawQuery: query.Encode(), Fragment: label,
		}).String(), Label: label}, nil
	case "vmess":
		if user.UUID == "" {
			return pluginapi.ClientLink{}, fmt.Errorf("engine_singbox: VMess inbound %q has no UUID for %q", label, user.Email)
		}
		payload := map[string]any{
			"v": "2", "ps": label, "add": host, "port": strconv.Itoa(port), "id": user.UUID,
			"aid": "0", "scy": "auto", "net": transportType(inbound), "type": "none",
		}
		if tlsEnabled(inbound) {
			payload["tls"] = "tls"
			payload["sni"] = tlsServerName(inbound)
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			return pluginapi.ClientLink{}, err
		}
		return pluginapi.ClientLink{Protocol: protocol, URI: "vmess://" + base64.StdEncoding.EncodeToString(encoded), Label: label}, nil
	case "trojan":
		password := firstNonEmpty(user.Auth, user.UUID)
		if password == "" {
			return pluginapi.ClientLink{}, fmt.Errorf("engine_singbox: Trojan inbound %q has no password for %q", label, user.Email)
		}
		query := url.Values{}
		applyTransportAndTLS(query, inbound)
		return pluginapi.ClientLink{Protocol: protocol, URI: (&url.URL{
			Scheme: "trojan", User: url.User(password), Host: hostPort, RawQuery: query.Encode(), Fragment: label,
		}).String(), Label: label}, nil
	case "shadowsocks":
		password := firstNonEmpty(user.Auth, user.UUID)
		method := firstNonEmpty(user.Cipher, mapString(inbound, "method"))
		if password == "" || method == "" {
			return pluginapi.ClientLink{}, fmt.Errorf("engine_singbox: Shadowsocks inbound %q has incomplete credentials for %q", label, user.Email)
		}
		credentials := base64.RawURLEncoding.EncodeToString([]byte(method + ":" + password))
		return pluginapi.ClientLink{Protocol: "shadowsocks", URI: "ss://" + credentials + "@" + hostPort + "#" + url.PathEscape(label), Label: label}, nil
	case "hysteria2":
		password := firstNonEmpty(user.Auth, user.UUID)
		if password == "" {
			return pluginapi.ClientLink{}, fmt.Errorf("engine_singbox: Hysteria2 inbound %q has no password for %q", label, user.Email)
		}
		query := url.Values{}
		if sni := tlsServerName(inbound); sni != "" {
			query.Set("sni", sni)
		}
		return pluginapi.ClientLink{Protocol: protocol, URI: (&url.URL{
			Scheme: "hysteria2", User: url.User(password), Host: hostPort, RawQuery: query.Encode(), Fragment: label,
		}).String(), Label: label}, nil
	case "tuic":
		if user.UUID == "" {
			return pluginapi.ClientLink{}, fmt.Errorf("engine_singbox: TUIC inbound %q has no UUID for %q", label, user.Email)
		}
		password := firstNonEmpty(user.Auth, user.UUID)
		query := url.Values{}
		if sni := tlsServerName(inbound); sni != "" {
			query.Set("sni", sni)
		}
		return pluginapi.ClientLink{Protocol: protocol, URI: (&url.URL{
			Scheme: "tuic", User: url.UserPassword(user.UUID, password), Host: hostPort, RawQuery: query.Encode(), Fragment: label,
		}).String(), Label: label}, nil
	default:
		return pluginapi.ClientLink{}, fmt.Errorf("engine_singbox: unsupported inbound type %q", protocol)
	}
}

func mergeLinkUser(supplied, fromConfig pluginapi.VPNUserConfig) pluginapi.VPNUserConfig {
	if fromConfig.UUID != "" {
		supplied.UUID = fromConfig.UUID
	}
	if fromConfig.Auth != "" {
		supplied.Auth = fromConfig.Auth
	}
	if fromConfig.Flow != "" {
		supplied.Flow = fromConfig.Flow
	}
	if fromConfig.Cipher != "" {
		supplied.Cipher = fromConfig.Cipher
	}
	return supplied
}

func clientHost(cfg pluginConfig, inbound map[string]any) (string, error) {
	host := strings.TrimSpace(cfg.ServerAddress)
	if host == "" {
		host = mapString(inbound, "listen")
	}
	if strings.Contains(host, "://") {
		parsed, err := url.Parse(host)
		if err != nil {
			return "", fmt.Errorf("engine_singbox: parse server address: %w", err)
		}
		host = parsed.Hostname()
	}
	if parsedHost, _, err := net.SplitHostPort(host); err == nil {
		host = parsedHost
	}
	host = strings.Trim(host, "[]")
	if host == "" || host == "0.0.0.0" || host == "::" {
		return "", fmt.Errorf("engine_singbox: server_address is required for inbound %q", mapString(inbound, "tag"))
	}
	return host, nil
}

func inboundPort(inbound map[string]any) int {
	for _, key := range []string{"listen_port", "port"} {
		if value := mapInt(inbound, key); value != 0 {
			return value
		}
	}
	return 0
}

func applyTransportAndTLS(query url.Values, inbound map[string]any) {
	query.Set("type", transportType(inbound))
	if !tlsEnabled(inbound) {
		query.Set("security", "none")
		return
	}
	tls, _ := inbound["tls"].(map[string]any)
	reality, _ := tls["reality"].(map[string]any)
	if realityEnabled(reality) {
		query.Set("security", "reality")
		if key := firstNonEmpty(mapString(reality, "public_key"), mapString(reality, "publicKey")); key != "" {
			query.Set("pbk", key)
		}
		if sid := firstNonEmpty(mapString(reality, "short_id"), mapString(reality, "shortId")); sid != "" {
			query.Set("sid", sid)
		}
	} else {
		query.Set("security", "tls")
	}
	if sni := tlsServerName(inbound); sni != "" {
		query.Set("sni", sni)
	}
	if fingerprint := firstNonEmpty(mapString(tls, "utls_fingerprint"), mapString(tls, "fingerprint")); fingerprint != "" {
		query.Set("fp", fingerprint)
	}
}

func transportType(inbound map[string]any) string {
	transport, _ := inbound["transport"].(map[string]any)
	return firstNonEmpty(mapString(transport, "type"), "tcp")
}

func tlsEnabled(inbound map[string]any) bool {
	tls, _ := inbound["tls"].(map[string]any)
	if tls == nil {
		return false
	}
	if enabled, ok := tls["enabled"].(bool); ok {
		return enabled
	}
	return len(tls) > 0
}

func realityEnabled(reality map[string]any) bool {
	if reality == nil {
		return false
	}
	if enabled, ok := reality["enabled"].(bool); ok {
		return enabled
	}
	return mapString(reality, "public_key") != "" || mapString(reality, "publicKey") != ""
}

func tlsServerName(inbound map[string]any) string {
	tls, _ := inbound["tls"].(map[string]any)
	return firstNonEmpty(mapString(tls, "server_name"), mapString(tls, "serverName"))
}

func endpointURL(apiAddr, endpoint string) string {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return ""
	}
	if strings.HasPrefix(endpoint, "http://") || strings.HasPrefix(endpoint, "https://") {
		return endpoint
	}
	apiAddr = strings.TrimSpace(apiAddr)
	if apiAddr == "" {
		return ""
	}
	if !strings.HasPrefix(apiAddr, "http://") && !strings.HasPrefix(apiAddr, "https://") {
		apiAddr = "http://" + apiAddr
	}
	return strings.TrimRight(apiAddr, "/") + "/" + strings.TrimLeft(endpoint, "/")
}

func checkHTTPEndpoint(ctx context.Context, endpoint string, timeout time.Duration) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("engine_singbox: build health request: %w", err)
	}
	response, err := (&http.Client{Timeout: timeout}).Do(request)
	if err != nil {
		return fmt.Errorf("engine_singbox: health endpoint: %w", err)
	}
	defer response.Body.Close() //nolint:errcheck
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("engine_singbox: health endpoint returned HTTP %d", response.StatusCode)
	}
	return nil
}

func fetchTrafficStats(ctx context.Context, endpoint string, timeout time.Duration) ([]pluginapi.TrafficStat, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("engine_singbox: build stats request: %w", err)
	}
	response, err := (&http.Client{Timeout: timeout}).Do(request)
	if err != nil {
		return nil, fmt.Errorf("engine_singbox: stats endpoint: %w", err)
	}
	defer response.Body.Close() //nolint:errcheck
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("engine_singbox: stats endpoint returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("engine_singbox: read stats response: %w", err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.UseNumber()
	var payload any
	if err := decoder.Decode(&payload); err != nil {
		return nil, fmt.Errorf("engine_singbox: decode stats response: %w", err)
	}
	stats := make(map[string]pluginapi.TrafficStat)
	collectTrafficStats(stats, payload, "")
	result := make([]pluginapi.TrafficStat, 0, len(stats))
	for _, stat := range stats {
		result = append(result, stat)
	}
	// Stable output is required because MultiEngine aggregates concurrently.
	// Local sorting also makes direct plugin consumers deterministic.
	sortTrafficStats(result)
	return result, nil
}

func collectTrafficStats(out map[string]pluginapi.TrafficStat, value any, impliedEmail string) {
	switch value := value.(type) {
	case []any:
		for _, item := range value {
			collectTrafficStats(out, item, "")
		}
	case map[string]any:
		email := firstNonEmpty(mapString(value, "email"), mapString(value, "user"), mapString(value, "name"), impliedEmail)
		up, upOK := trafficNumber(value, "up", "upload", "upload_total", "uploadTotal")
		down, downOK := trafficNumber(value, "down", "download", "download_total", "downloadTotal")
		if email != "" && (upOK || downOK) {
			stat := out[email]
			stat.Email = email
			stat.Up += up
			stat.Down += down
			out[email] = stat
			return
		}
		for key, child := range value {
			if key == "email" || key == "user" || key == "name" {
				continue
			}
			childEmail := ""
			if key != "users" && key != "stats" && key != "traffic" && key != "data" {
				childEmail = key
			}
			collectTrafficStats(out, child, childEmail)
		}
	}
}

func trafficNumber(value map[string]any, keys ...string) (int64, bool) {
	for _, key := range keys {
		switch number := value[key].(type) {
		case json.Number:
			parsed, err := number.Int64()
			if err == nil {
				return parsed, true
			}
		case float64:
			return int64(number), true
		case int64:
			return number, true
		case int:
			return int64(number), true
		case string:
			parsed, err := strconv.ParseInt(strings.TrimSpace(number), 10, 64)
			if err == nil {
				return parsed, true
			}
		}
	}
	return 0, false
}

func sortTrafficStats(stats []pluginapi.TrafficStat) {
	for i := 1; i < len(stats); i++ {
		for j := i; j > 0 && stats[j].Email < stats[j-1].Email; j-- {
			stats[j], stats[j-1] = stats[j-1], stats[j]
		}
	}
}

var _ pluginapi.ClientConfigContributor = (*Plugin)(nil)
