package convert

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	json "github.com/goccy/go-json"
	"gopkg.in/yaml.v3"
)

// ClashProxy is an ordered representation of a single Clash/Mihomo proxy entry.
type ClashProxy map[string]any

// XrayJSONToClashYAML converts an Xray JSON config (single config, outbound array
// or config array) to a Clash/Mihomo YAML subscription.
func XrayJSONToClashYAML(xrayJSON string) (string, error) {
	shareText, err := XrayJSONToShareText(xrayJSON)
	if err != nil {
		return "", err
	}
	return ShareTextToClashYAML(shareText)
}

// ShareTextToClashYAML converts share links (one per line) to a Clash/Mihomo YAML subscription.
func ShareTextToClashYAML(shareText string) (string, error) {
	proxies := make([]ClashProxy, 0)
	names := make(map[string]int)

	for _, line := range strings.Split(shareText, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		proxy, err := shareLinkToClashProxy(line)
		if err != nil {
			return "", err
		}
		if proxy == nil {
			continue
		}
		name, _ := proxy["name"].(string)
		if name == "" {
			name = "proxy"
		}
		names[name]++
		if names[name] > 1 {
			name = fmt.Sprintf("%s-%d", name, names[name])
		}
		proxy["name"] = name
		proxies = append(proxies, proxy)
	}

	if len(proxies) == 0 {
		return "", fmt.Errorf("no supported proxies found for clash conversion")
	}

	names2 := make([]string, 0, len(proxies))
	for _, p := range proxies {
		names2 = append(names2, p["name"].(string))
	}

	doc := map[string]any{
		"port":                7890,
		"socks-port":          7891,
		"allow-lan":           false,
		"mode":                "rule",
		"log-level":           "info",
		"external-controller": "127.0.0.1:9090",
		"proxies":             proxies,
		"proxy-groups": []map[string]any{
			{
				"name":    "PROXY",
				"type":    "select",
				"proxies": append([]string{"AUTO"}, names2...),
			},
			{
				"name":     "AUTO",
				"type":     "url-test",
				"proxies":  names2,
				"url":      "http://www.gstatic.com/generate_204",
				"interval": 300,
			},
		},
		"rules": []string{
			"GEOIP,private,DIRECT,no-resolve",
			"MATCH,PROXY",
		},
	}

	out, err := yaml.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("failed to encode clash yaml: %w", err)
	}
	return string(out), nil
}

// shareLinkToClashProxy converts a single share link to a Clash proxy entry.
// It returns (nil, nil) when the protocol has no Clash equivalent.
func shareLinkToClashProxy(link string) (ClashProxy, error) {
	scheme := strings.ToLower(link)
	if idx := strings.Index(scheme, "://"); idx > 0 {
		scheme = scheme[:idx]
	} else {
		return nil, fmt.Errorf("invalid share link: %q", link)
	}

	switch scheme {
	case "vless":
		return vlessLinkToClashProxy(link)
	case "trojan":
		return trojanLinkToClashProxy(link)
	case "vmess":
		return vmessLinkToClashProxy(link)
	case "ss":
		return shadowsocksLinkToClashProxy(link)
	case "hysteria2", "hy2":
		return hysteria2LinkToClashProxy(link)
	case "socks", "socks5", "socks5h":
		return socksLinkToClashProxy(link)
	default:
		return nil, nil
	}
}

func parseShareURL(link string) (*url.URL, string, int, error) {
	parsed, err := url.Parse(link)
	if err != nil {
		return nil, "", 0, fmt.Errorf("invalid share link %q: %w", link, err)
	}
	host := parsed.Hostname()
	if host == "" {
		return nil, "", 0, fmt.Errorf("share link %q is missing a host", link)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil || port < 1 || port > 65535 {
		return nil, "", 0, fmt.Errorf("share link %q has an invalid port", link)
	}
	return parsed, host, port, nil
}

func shareLinkName(parsed *url.URL, fallback string) string {
	name, err := url.PathUnescape(parsed.Fragment)
	if err != nil || strings.TrimSpace(name) == "" {
		name = strings.TrimSpace(parsed.Fragment)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return fallback
	}
	return name
}

func vlessLinkToClashProxy(link string) (ClashProxy, error) {
	parsed, host, port, err := parseShareURL(link)
	if err != nil {
		return nil, err
	}
	q := parsed.Query()

	proxy := ClashProxy{
		"name":   shareLinkName(parsed, host),
		"type":   "vless",
		"server": host,
		"port":   port,
		"uuid":   parsed.User.Username(),
		"udp":    true,
	}
	if enc := q.Get("encryption"); enc != "" && enc != "none" {
		proxy["encryption"] = enc
	}
	if flow := q.Get("flow"); flow != "" {
		proxy["flow"] = flow
	}
	applyClashTLS(proxy, q, host)
	applyClashNetwork(proxy, q, host)
	return proxy, nil
}

func trojanLinkToClashProxy(link string) (ClashProxy, error) {
	parsed, host, port, err := parseShareURL(link)
	if err != nil {
		return nil, err
	}
	q := parsed.Query()
	password, err := url.PathUnescape(parsed.User.Username())
	if err != nil {
		password = parsed.User.Username()
	}

	proxy := ClashProxy{
		"name":     shareLinkName(parsed, host),
		"type":     "trojan",
		"server":   host,
		"port":     port,
		"password": password,
		"udp":      true,
	}
	applyClashTLS(proxy, q, host)
	delete(proxy, "tls")
	applyClashNetwork(proxy, q, host)
	return proxy, nil
}

func shadowsocksLinkToClashProxy(link string) (ClashProxy, error) {
	parsed, err := url.Parse(link)
	if err != nil {
		return nil, fmt.Errorf("invalid share link %q: %w", link, err)
	}

	var cipher, password, host string
	var port int

	if parsed.Host != "" {
		host = parsed.Hostname()
		port, err = strconv.Atoi(parsed.Port())
		if err != nil || port < 1 || port > 65535 {
			return nil, fmt.Errorf("share link %q has an invalid port", link)
		}
		userinfo := ""
		if parsed.User != nil {
			userinfo = parsed.User.String()
		}
		if decoded, decodeErr := decodeShareBase64(userinfo); decodeErr == nil && strings.Contains(string(decoded), ":") {
			userinfo = string(decoded)
		} else if unescaped, unescapeErr := url.PathUnescape(userinfo); unescapeErr == nil {
			userinfo = unescaped
		}
		cipher, password, _ = strings.Cut(userinfo, ":")
	} else {
		// Fully base64-encoded legacy form: ss://base64(method:password@host:port)
		payload := strings.TrimPrefix(link, "ss://")
		if idx := strings.IndexByte(payload, '#'); idx >= 0 {
			payload = payload[:idx]
		}
		decoded, decodeErr := decodeShareBase64(payload)
		if decodeErr != nil {
			return nil, fmt.Errorf("invalid shadowsocks link %q", link)
		}
		creds, hostPort, ok := strings.Cut(string(decoded), "@")
		if !ok {
			return nil, fmt.Errorf("invalid shadowsocks link %q", link)
		}
		cipher, password, _ = strings.Cut(creds, ":")
		hostname, portStr, splitErr := net.SplitHostPort(hostPort)
		if splitErr != nil {
			return nil, fmt.Errorf("invalid shadowsocks link %q", link)
		}
		host = hostname
		port, err = strconv.Atoi(portStr)
		if err != nil || port < 1 || port > 65535 {
			return nil, fmt.Errorf("share link %q has an invalid port", link)
		}
	}

	if cipher == "" {
		return nil, fmt.Errorf("shadowsocks link %q is missing a cipher", link)
	}

	proxy := ClashProxy{
		"name":     shareLinkName(parsed, host),
		"type":     "ss",
		"server":   host,
		"port":     port,
		"cipher":   cipher,
		"password": password,
		"udp":      true,
	}
	return proxy, nil
}

func vmessLinkToClashProxy(link string) (ClashProxy, error) {
	payload := link[len("vmess://"):]
	if idx := strings.IndexByte(payload, '#'); idx >= 0 {
		payload = payload[:idx]
	}
	decoded, err := decodeShareBase64(payload)
	if err != nil {
		return nil, fmt.Errorf("invalid vmess link %q", link)
	}

	var qr map[string]any
	if err := json.Unmarshal(decoded, &qr); err != nil {
		return nil, fmt.Errorf("invalid vmess link %q", link)
	}

	host := jsonString(qr["add"])
	port, err := strconv.Atoi(jsonString(qr["port"]))
	if err != nil || port < 1 || port > 65535 {
		return nil, fmt.Errorf("vmess link %q has an invalid port", link)
	}

	name := jsonString(qr["ps"])
	if name == "" {
		name = host
	}
	alterID, _ := strconv.Atoi(jsonString(qr["aid"]))
	cipher := jsonString(qr["scy"])
	if cipher == "" {
		cipher = "auto"
	}

	proxy := ClashProxy{
		"name":    name,
		"type":    "vmess",
		"server":  host,
		"port":    port,
		"uuid":    jsonString(qr["id"]),
		"alterId": alterID,
		"cipher":  cipher,
		"udp":     true,
	}

	if strings.EqualFold(jsonString(qr["tls"]), "tls") {
		proxy["tls"] = true
		if sni := jsonString(qr["sni"]); sni != "" {
			proxy["servername"] = sni
		}
	}

	network := jsonString(qr["net"])
	switch network {
	case "ws":
		proxy["network"] = "ws"
		wsOpts := map[string]any{}
		if path := jsonString(qr["path"]); path != "" {
			wsOpts["path"] = path
		}
		if wsHost := jsonString(qr["host"]); wsHost != "" {
			wsOpts["headers"] = map[string]string{"Host": wsHost}
		}
		if len(wsOpts) > 0 {
			proxy["ws-opts"] = wsOpts
		}
	case "grpc":
		proxy["network"] = "grpc"
		if service := jsonString(qr["path"]); service != "" {
			proxy["grpc-opts"] = map[string]any{"grpc-service-name": service}
		}
	case "h2":
		proxy["network"] = "h2"
	}

	return proxy, nil
}

func hysteria2LinkToClashProxy(link string) (ClashProxy, error) {
	parsed, host, port, err := parseShareURL(link)
	if err != nil {
		return nil, err
	}
	q := parsed.Query()
	password := parsed.User.Username()
	if pass, ok := parsed.User.Password(); ok && pass != "" {
		password = password + ":" + pass
	}
	if unescaped, unescapeErr := url.PathUnescape(password); unescapeErr == nil {
		password = unescaped
	}

	proxy := ClashProxy{
		"name":     shareLinkName(parsed, host),
		"type":     "hysteria2",
		"server":   host,
		"port":     port,
		"password": password,
	}
	if sni := q.Get("sni"); sni != "" {
		proxy["sni"] = sni
	}
	if obfs := q.Get("obfs"); obfs != "" {
		proxy["obfs"] = obfs
		if obfsPass := q.Get("obfs-password"); obfsPass != "" {
			proxy["obfs-password"] = obfsPass
		}
	}
	if q.Get("insecure") == "1" || strings.EqualFold(q.Get("insecure"), "true") {
		proxy["skip-cert-verify"] = true
	}
	return proxy, nil
}

func socksLinkToClashProxy(link string) (ClashProxy, error) {
	parsed, host, port, err := parseShareURL(link)
	if err != nil {
		return nil, err
	}

	proxy := ClashProxy{
		"name":   shareLinkName(parsed, host),
		"type":   "socks5",
		"server": host,
		"port":   port,
		"udp":    true,
	}

	if parsed.User != nil {
		username := parsed.User.Username()
		password, hasPassword := parsed.User.Password()
		if !hasPassword {
			// libxray encodes socks credentials as base64("user:pass")
			if decoded, decodeErr := decodeShareBase64(username); decodeErr == nil && strings.Contains(string(decoded), ":") {
				username, password, _ = strings.Cut(string(decoded), ":")
			}
		}
		if username != "" {
			proxy["username"] = username
			proxy["password"] = password
		}
	}
	return proxy, nil
}

func applyClashTLS(proxy ClashProxy, q url.Values, host string) {
	security := strings.ToLower(q.Get("security"))
	switch security {
	case "tls":
		proxy["tls"] = true
	case "reality":
		proxy["tls"] = true
		realityOpts := map[string]any{}
		if pbk := q.Get("pbk"); pbk != "" {
			realityOpts["public-key"] = pbk
		}
		if sid := q.Get("sid"); sid != "" {
			realityOpts["short-id"] = sid
		}
		if len(realityOpts) > 0 {
			proxy["reality-opts"] = realityOpts
		}
	default:
		return
	}

	sni := q.Get("sni")
	if sni == "" {
		sni = q.Get("host")
	}
	if sni == "" {
		sni = host
	}
	proxy["servername"] = sni

	if fp := q.Get("fp"); fp != "" {
		proxy["client-fingerprint"] = fp
	}
	if alpn := q.Get("alpn"); alpn != "" {
		proxy["alpn"] = strings.Split(alpn, ",")
	}
	if q.Get("allowInsecure") == "1" || strings.EqualFold(q.Get("allowInsecure"), "true") {
		proxy["skip-cert-verify"] = true
	}
}

func applyClashNetwork(proxy ClashProxy, q url.Values, host string) {
	network := strings.ToLower(q.Get("type"))
	switch network {
	case "", "tcp", "raw":
		return
	case "ws":
		proxy["network"] = "ws"
		wsOpts := map[string]any{}
		if path := q.Get("path"); path != "" {
			wsOpts["path"] = path
		}
		headerHost := q.Get("host")
		if headerHost == "" {
			headerHost = host
		}
		wsOpts["headers"] = map[string]string{"Host": headerHost}
		proxy["ws-opts"] = wsOpts
	case "grpc":
		proxy["network"] = "grpc"
		if service := q.Get("serviceName"); service != "" {
			proxy["grpc-opts"] = map[string]any{"grpc-service-name": service}
		}
	case "http", "h2":
		proxy["network"] = "h2"
		h2Opts := map[string]any{}
		if path := q.Get("path"); path != "" {
			h2Opts["path"] = path
		}
		if headerHost := q.Get("host"); headerHost != "" {
			h2Opts["host"] = strings.Split(headerHost, ",")
		}
		if len(h2Opts) > 0 {
			proxy["h2-opts"] = h2Opts
		}
	default:
		proxy["network"] = network
	}
}

func jsonString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(typed)
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", typed)
	}
}
