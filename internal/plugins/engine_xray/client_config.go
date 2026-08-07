package engine_xray

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"

	json "github.com/goccy/go-json"
	"github.com/google/uuid"

	"xraytool/internal/pluginapi"
	
)

// BuildClientLinks turns the Xray inbounds that contain u into portable share
// links.  It deliberately reads Xray's native configuration here, at the
// engine boundary, rather than making subscription code understand Xray's
// RawConfig layout.  A future engine (Sing-box, Mihomo, ...) implements the
// same pluginapi.ClientConfigContributor contract with its own config format.
//
// Config keys owned by this plugin:
//   - config_path: path to the active Xray config.json (required)
//   - server_address (aliases: server, host, address): public VPN endpoint
//   - hy2_config_yaml: optional legacy Hysteria2 YAML file
//
// An empty result is valid when the user is not present in an Xray-supported
// inbound.  That lets the subscription aggregator combine multiple engines
// without treating one engine's absence as a fatal error.
func (p *Plugin) BuildClientLinks(ctx context.Context, u pluginapi.VPNUserConfig) ([]pluginapi.ClientLink, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	configPath := strings.TrimSpace(p.cfg.ConfigPath)
	if configPath == "" {
		return nil, fmt.Errorf("engine_xray: BuildClientLinks requires config_path")
	}

	host, err := clientEndpointHost(p.cfg.ServerAddress)
	if err != nil {
		return nil, fmt.Errorf("engine_xray: invalid server_address: %w", err)
	}
	if host == "" {
		return nil, fmt.Errorf("engine_xray: BuildClientLinks requires server_address")
	}

	cfg, err := Read(configPath)
	if err != nil {
		return nil, fmt.Errorf("engine_xray: read active config: %w", err)
	}
	return buildClientLinks(ctx, cfg, host, p.cfg.Hy2ConfigYAML, u)
}

func buildClientLinks(
	ctx context.Context,
	cfg RawConfig,
	host string,
	hy2ConfigYAML string,
	u pluginapi.VPNUserConfig,
) ([]pluginapi.ClientLink, error) {
	inbounds, err := cfg.GetInbounds()
	if err != nil {
		return nil, fmt.Errorf("engine_xray: parse inbounds: %w", err)
	}

	links := make([]pluginapi.ClientLink, 0, len(inbounds))
	for _, inbound := range inbounds {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		protocol := strings.ToLower(strings.TrimSpace(inbound.Protocol()))
		if protocol != "vless" && protocol != "shadowsocks" && protocol != "ss" && !inbound.IsHysteria() {
			continue
		}

		client, found := findInboundClient(inbound, u)
		var link pluginapi.ClientLink
		switch protocol {
		case "vless":
			// A DB-backed user can be newly created before Xray has reloaded its
			// config.  UUID is sufficient for VLESS, so do not discard that link
			// merely because this snapshot has not acquired the client yet.
			if !found && strings.TrimSpace(u.UUID) == "" {
				continue
			}
			link, err = buildVLESSLink(cfg, inbound, client, host, u)
		case "shadowsocks", "ss":
			if !found {
				continue
			}
			link, err = buildShadowsocksLink(cfg, inbound, client, host, u)
		default:
			if !found && strings.TrimSpace(u.Auth) == "" && strings.TrimSpace(u.UUID) == "" {
				continue
			}
			link, err = buildHysteria2Link(cfg, inbound, client, host, hy2ConfigYAML, u)
		}
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(link.URI) != "" {
			links = append(links, link)
		}
	}

	return links, nil
}

func findInboundClient(inbound RawInbound, user pluginapi.VPNUserConfig) (RawClient, bool) {
	clients, err := inbound.GetClients()
	if err != nil {
		return nil, false
	}
	for _, client := range clients {
		if user.Email != "" && client.Email() == user.Email {
			return client, true
		}
		if user.UUID != "" && client.GetString("id") == user.UUID {
			return client, true
		}
		if user.Auth != "" && (client.GetString("auth") == user.Auth || client.GetString("password") == user.Auth) {
			return client, true
		}
	}
	return nil, false
}

func buildVLESSLink(
	cfg RawConfig,
	inbound RawInbound,
	client RawClient,
	host string,
	u pluginapi.VPNUserConfig,
) (pluginapi.ClientLink, error) {
	port, err := inboundPort(inbound)
	if err != nil {
		return pluginapi.ClientLink{}, err
	}

	userID := firstNonEmpty(client.GetString("id"), u.UUID)
	if userID == "" {
		return pluginapi.ClientLink{}, fmt.Errorf("engine_xray: VLESS inbound %q has no user UUID", inbound.Tag())
	}

	stream := inboundObject(inbound, "streamSettings")
	values := url.Values{}
	values.Set("encryption", firstNonEmpty(client.GetString("encryption"), "none"))
	values.Set("type", firstNonEmpty(mapString(stream, "network"), "tcp"))

	security := strings.ToLower(mapString(stream, "security"))
	if security != "" && security != "none" {
		values.Set("security", security)
	}
	if flow := firstNonEmpty(client.GetString("flow"), u.Flow); flow != "" {
		values.Set("flow", flow)
	}

	switch security {
	case "reality":
		reality := mapObject(stream, "realitySettings")
		if sni := firstNonEmpty(mapString(reality, "serverName"), firstString(mapStrings(reality, "serverNames"))); sni != "" {
			values.Set("sni", sni)
		} else if sni := firstRealitySNI(cfg); sni != "" {
			values.Set("sni", sni)
		}
		publicKey := firstNonEmpty(mapString(reality, "publicKey"), derivePublicKey(firstRealityPrivateKey(cfg)), firstRealityPublicKey(cfg))
		if publicKey != "" {
			values.Set("pbk", publicKey)
		}
		shortID := firstNonEmpty(mapString(reality, "shortId"), randomRealityShortID(cfg))
		if shortID != "" {
			values.Set("sid", shortID)
		}
		if fingerprint := firstNonEmpty(mapString(reality, "fingerprint"), mapString(reality, "fp")); fingerprint != "" {
			values.Set("fp", fingerprint)
		}
	case "tls":
		tls := mapObject(stream, "tlsSettings")
		if sni := firstNonEmpty(mapString(tls, "serverName"), mapString(tls, "sni")); sni != "" {
			values.Set("sni", sni)
		}
		if fingerprint := firstNonEmpty(mapString(tls, "fingerprint"), mapString(tls, "fp")); fingerprint != "" {
			values.Set("fp", fingerprint)
		}
	}
	appendTransportValues(values, stream)

	label := firstNonEmpty(inbound.Tag(), "Xray VLESS")
	uri := (&url.URL{
		Scheme:   "vless",
		User:     url.User(userID),
		Host:     net.JoinHostPort(host, strconv.Itoa(port)),
		RawQuery: values.Encode(),
		Fragment: label,
	}).String()
	return pluginapi.ClientLink{Protocol: "vless", URI: uri, Label: label}, nil
}

func buildShadowsocksLink(
	cfg RawConfig,
	inbound RawInbound,
	client RawClient,
	host string,
	u pluginapi.VPNUserConfig,
) (pluginapi.ClientLink, error) {
	port, err := inboundPort(inbound)
	if err != nil {
		return pluginapi.ClientLink{}, err
	}
	settings := inboundObject(inbound, "settings")
	method := firstNonEmpty(mapString(settings, "method"), u.Cipher, "2022-blake3-aes-256-gcm")
	serverPassword := firstNonEmpty(mapString(settings, "password"), ssServerPassword(cfg))
	if serverPassword == "" {
		return pluginapi.ClientLink{}, fmt.Errorf("engine_xray: Shadowsocks inbound %q has no server password", inbound.Tag())
	}
	userPassword := firstNonEmpty(client.GetString("password"), u.Auth)
	credentials := method + ":" + serverPassword
	if userPassword != "" {
		credentials += ":" + userPassword
	}

	label := firstNonEmpty(inbound.Tag(), "Xray Shadowsocks")
	encoded := base64.StdEncoding.EncodeToString([]byte(credentials))
	uri := "ss://" + encoded + "@" + net.JoinHostPort(host, strconv.Itoa(port)) + "#" + url.PathEscape(label)
	return pluginapi.ClientLink{Protocol: "shadowsocks", URI: uri, Label: label}, nil
}

func buildHysteria2Link(
	cfg RawConfig,
	inbound RawInbound,
	client RawClient,
	host string,
	hy2ConfigYAML string,
	u pluginapi.VPNUserConfig,
) (pluginapi.ClientLink, error) {
	port, err := inboundPort(inbound)
	if err != nil {
		return pluginapi.ClientLink{}, err
	}
	settings := inboundObject(inbound, "settings")
	auth := extractHy2Pass(firstNonEmpty(client.GetString("auth"), u.Auth))
	if auth == "" || isUUID(auth) {
		auth = buildDeterministicHy2Pass(u.UUID, u.Email)
	}
	if auth == "" {
		return pluginapi.ClientLink{}, fmt.Errorf("engine_xray: Hysteria2 inbound %q has no user auth", inbound.Tag())
	}

	values := url.Values{}
	tls := mapObject(settings, "tls")
	if sni := firstNonEmpty(mapString(tls, "sni"), mapString(tls, "serverName"), mapString(settings, "sni"), mapString(settings, "serverName")); sni != "" {
		values.Set("sni", sni)
	}
	if obfs := getOrCreateHy2ObfsPassword(hy2ConfigYAML, cfg); obfs != "" {
		values.Set("obfs", "salamander")
		values.Set("obfs-password", obfs)
	}

	label := firstNonEmpty(inbound.Tag(), "Xray Hysteria2")
	uri := (&url.URL{
		Scheme:   "hysteria2",
		User:     url.User(auth),
		Host:     net.JoinHostPort(host, strconv.Itoa(port)),
		RawQuery: values.Encode(),
		Fragment: label,
	}).String()
	return pluginapi.ClientLink{Protocol: "hysteria2", URI: uri, Label: label}, nil
}

func clientEndpointHost(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if strings.Contains(raw, "://") {
		parsed, err := url.Parse(raw)
		if err != nil {
			return "", err
		}
		raw = parsed.Host
	}
	if host, _, err := net.SplitHostPort(raw); err == nil {
		return strings.Trim(host, "[]"), nil
	}
	raw = strings.Trim(raw, "[]")
	if raw == "" || strings.ContainsAny(raw, "/?#") {
		return "", fmt.Errorf("expected a host, got %q", raw)
	}
	return raw, nil
}

func inboundPort(inbound RawInbound) (int, error) {
	raw, ok := inbound["port"]
	if !ok {
		return 0, fmt.Errorf("engine_xray: inbound %q has no port", inbound.Tag())
	}
	var port int
	if err := json.Unmarshal(raw, &port); err == nil && port > 0 && port <= 65535 {
		return port, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		parsed, err := strconv.Atoi(strings.TrimSpace(text))
		if err == nil && parsed > 0 && parsed <= 65535 {
			return parsed, nil
		}
	}
	return 0, fmt.Errorf("engine_xray: inbound %q has invalid port", inbound.Tag())
}

func inboundObject(inbound RawInbound, key string) map[string]any {
	raw, ok := inbound[key]
	if !ok {
		return nil
	}
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil
	}
	return result
}

func mapObject(value map[string]any, key string) map[string]any {
	if value == nil {
		return nil
	}
	object, _ := value[key].(map[string]any)
	return object
}

func mapString(value map[string]any, key string) string {
	if value == nil {
		return ""
	}
	text, _ := value[key].(string)
	return strings.TrimSpace(text)
}

func mapStrings(value map[string]any, key string) []string {
	if value == nil {
		return nil
	}
	values, _ := value[key].([]any)
	result := make([]string, 0, len(values))
	for _, value := range values {
		if text, ok := value.(string); ok && strings.TrimSpace(text) != "" {
			result = append(result, strings.TrimSpace(text))
		}
	}
	return result
}

func firstString(values []string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func appendTransportValues(values url.Values, stream map[string]any) {
	switch strings.ToLower(mapString(stream, "network")) {
	case "ws":
		ws := mapObject(stream, "wsSettings")
		if path := mapString(ws, "path"); path != "" {
			values.Set("path", path)
		}
		if headers := mapObject(ws, "headers"); headers != nil {
			if host := firstNonEmpty(mapString(headers, "Host"), mapString(headers, "host")); host != "" {
				values.Set("host", host)
			}
		}
	case "grpc":
		grpc := mapObject(stream, "grpcSettings")
		if serviceName := mapString(grpc, "serviceName"); serviceName != "" {
			values.Set("serviceName", serviceName)
		}
	case "httpupgrade":
		httpUpgrade := mapObject(stream, "httpupgradeSettings")
		if path := mapString(httpUpgrade, "path"); path != "" {
			values.Set("path", path)
		}
		if host := mapString(httpUpgrade, "host"); host != "" {
			values.Set("host", host)
		}
	case "xhttp", "splithttp":
		xhttp := mapObject(stream, "xhttpSettings")
		if xhttp == nil {
			xhttp = mapObject(stream, "splithttpSettings")
		}
		if path := mapString(xhttp, "path"); path != "" {
			values.Set("path", path)
		}
		if host := mapString(xhttp, "host"); host != "" {
			values.Set("host", host)
		}
	}
}

// The helpers below were intentionally moved from internal/subscription. They
// are Xray-format knowledge and belong beside the engine that owns the config.

func firstRealityPrivateKey(cfg RawConfig) string {
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

func firstRealityPublicKey(cfg RawConfig) string {
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

func randomRealityShortID(cfg RawConfig) string {
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
									return validSIDs[0]
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

func firstRealitySNI(cfg RawConfig) string {
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

	if !regexp.MustCompile(`^[A-Za-z0-9\-_=]+$`).MatchString(privateKey) {
		pubKeyCache[privateKey] = ""
		return ""
	}
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
	privBytes[0] &= 248
	privBytes[31] &= 127
	privBytes[31] |= 64

	key, err := ecdh.X25519().NewPrivateKey(privBytes)
	if err != nil {
		pubKeyCache[privateKey] = ""
		return ""
	}
	pub = base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes())
	pubKeyCache[privateKey] = pub
	return pub
}

func ssServerPassword(cfg RawConfig) string {
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

func isUUID(value string) bool {
	_, err := uuid.Parse(value)
	return err == nil
}

func buildDeterministicHy2Pass(uuidHint, email string) string {
	h := sha256.New()
	h.Write([]byte(uuidHint + ":" + email + ":hy2"))
	return hex.EncodeToString(h.Sum(nil))
}

func getOrCreateHy2ObfsPassword(yamlPath string, cfg RawConfig) string {
	inbounds, err := cfg.GetInbounds()
	if err == nil {
		for _, ib := range inbounds {
			if !ib.IsHysteria() {
				continue
			}
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

	if value := getHy2ObfsPasswordFromYAML(yamlPath); value != "" {
		return value
	}
	if err == nil {
		for _, ib := range inbounds {
			clients, clientsErr := ib.GetClients()
			if clientsErr != nil {
				continue
			}
			for _, client := range clients {
				if obfs := client.GetString("hy2_obfs"); obfs != "" {
					return obfs
				}
			}
		}
	}
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
			value := strings.TrimPrefix(line, "password:")
			if index := strings.Index(value, "#"); index >= 0 {
				value = value[:index]
			}
			return strings.Trim(strings.TrimSpace(value), `"'`)
		}
	}
	return ""
}
