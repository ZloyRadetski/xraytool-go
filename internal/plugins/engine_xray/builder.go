package engine_xray

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	json "github.com/goccy/go-json"
	"strings"

	"xraytool/internal/domain"

	"github.com/google/uuid"
)

// ClientParams holds the values to fill into a client object.
type ClientParams struct {
	Email   string
	UUID    string
	Auth    string
	Subfile string
	Expire  string
	Flow    string
	Limit   *float64
}

// BuildForAllInbounds builds a TaggedClient entry for every client inbound
// by dynamically inferring the required fields from the inbound's protocol.
func BuildForAllInbounds(cfg RawConfig, params ClientParams) ([]TaggedClient, error) {
	inbounds, err := cfg.GetInbounds()
	if err != nil {
		return nil, err
	}

	var payload []TaggedClient
	for _, ib := range inbounds {
		if !ib.HasClientList() {
			continue
		}
		tag := ib.Tag()
		if tag == "" {
			continue
		}

		client, err := BuildClient(ib, params)
		if err != nil {
			return nil, fmt.Errorf("building client for inbound %q: %w", tag, err)
		}
		payload = append(payload, TaggedClient{Tag: tag, Client: client})
	}
	return payload, nil
}

// BuildClient constructs a RawClient for a given inbound based on its protocol.
func BuildClient(ib RawInbound, params ClientParams) (RawClient, error) {
	result := make(RawClient)

	// Always add metadata fields
	result.Set("email", params.Email)
	if params.Subfile != "" {
		result.Set("subfile", params.Subfile)
	}
	if params.Expire != "" {
		result.Set("expire", params.Expire)
	}
	if params.Limit != nil {
		result.SetNumber("limit", *params.Limit)
	}

	protocol := ib.Protocol()

	switch protocol {
	case "vless":
		if params.UUID == "" {
			return nil, fmt.Errorf("vless requires UUID")
		}
		result.Set("id", params.UUID)

		flow := params.Flow
		if flow == "" && hasXTLS(ib) {
			flow = "xtls-rprx-vision"
		}
		if flow != "" {
			result.Set("flow", flow)
		}

	case "xhttp", "splithttp":
		if params.UUID == "" {
			return nil, fmt.Errorf("%s requires UUID", protocol)
		}
		result.Set("id", params.UUID)
		// Vision is only valid for VLESS over a TCP transport. xHTTP and
		// splitHTTP must never inherit or synthesize a VLESS flow, even when
		// their Reality settings are otherwise similar.

	case "vmess":
		if params.UUID == "" {
			return nil, fmt.Errorf("vmess requires UUID")
		}
		result.Set("id", params.UUID)

	case "trojan", "shadowsocks":
		auth := params.Auth
		if auth == "" {
			if params.UUID != "" {
				auth = params.UUID
			} else {
				return nil, fmt.Errorf("%s requires auth/password", protocol)
			}
		}
		result.Set("password", auth)

	case "hysteria", "hysteria2", "hy2":
		auth := params.Auth
		if auth == "" || IsUUID(auth) {
			auth = BuildDeterministicHy2Pass(params.UUID, params.Email)
		}
		// Based on the old templates, hysteria2 usually uses "auth" or "password".
		// We'll set "auth" and "password" to be safe, or just "auth" if that was the old default.
		// Wait, the old template for hy2 had "auth". We will use "auth".
		result.Set("auth", auth)

	default:
		return nil, fmt.Errorf("unsupported protocol for user generation: %s", protocol)
	}

	return result, nil
}

// hasXTLS checks if the streamSettings indicate XTLS/Vision compatibility.
func hasXTLS(ib RawInbound) bool {
	_, err := ib.Settings()
	if err != nil {
		return false
	}

	// For vless, if security is reality or tls, vision flow is generally applicable.
	rawStream, ok := ib["streamSettings"]
	if !ok {
		return false
	}

	var stream map[string]json.RawMessage
	if err := json.Unmarshal(rawStream, &stream); err != nil {
		return false
	}

	var net string
	if rawNet, ok := stream["network"]; ok {
		_ = json.Unmarshal(rawNet, &net)
	}
	net = strings.ToLower(strings.TrimSpace(net))
	if net == "" {
		net = "tcp" // default network is tcp
	}

	// XTLS Vision flow is strictly for TCP. It does not work over mKCP, WS, gRPC, or xHTTP.
	if net != "tcp" {
		return false
	}

	rawSec, ok := stream["security"]
	if !ok {
		return false
	}
	var sec string
	if err := json.Unmarshal(rawSec, &sec); err != nil {
		return false
	}
	sec = strings.ToLower(strings.TrimSpace(sec))
	return sec == "reality" || sec == "tls"
}

func BuildDeterministicHy2Pass(uuidVal, email string) string {
	// Revert to a deterministic derivation using HKDF or HMAC
	// We'll use SHA256 over UUID + email for stable stateless passwords
	h := sha256.New()
	h.Write([]byte(uuidVal + ":" + email + ":hy2"))
	return hex.EncodeToString(h.Sum(nil))
}

func IsUUID(s string) bool {
	_, err := uuid.Parse(s)
	return err == nil
}

func GetMetadataString(m domain.Metadata, key string) string {
	if m == nil {
		return ""
	}
	val, ok := m[key]
	if !ok || val == nil {
		return ""
	}
	str, ok := val.(string)
	if !ok {
		return ""
	}
	return str
}

func SubscriptionToVPNUserConfig(sub domain.Subscription) domain.VPNUserConfig {
	authVal := GetMetadataString(sub.Metadata, "auth")
	if authVal == "" {
		authVal = GetMetadataString(sub.Metadata, "password")
	}
	if authVal == "" || IsUUID(authVal) {
		authVal = BuildDeterministicHy2Pass(sub.UUID, sub.Email)
	}

	subfile := GetMetadataString(sub.Metadata, "subfile")
	if subfile == "" {
		subfile = sub.ID
	}

	expireVal := GetMetadataString(sub.Metadata, "expire")
	if expireVal == "" && sub.EndsAt != nil {
		expireVal = sub.EndsAt.Format("02.01.2006")
	}

	return domain.VPNUserConfig{
		Email:      sub.Email,
		UUID:       sub.UUID,
		Auth:       authVal,
		Subfile:    subfile,
		Expire:     expireVal,
		MaxDevices: sub.MaxDevices,
		// engine_ids is deliberately a subscription-level JSON metadata
		// value: it allows an administrator to move one subscription between
		// engines without a schema migration. State-sync serialises this
		// field with the user, so slave MultiEngines make the same decision.
		SubscriptionEngineIDs: GetMetadataStringSlice(sub.Metadata, "engine_ids"),
		// plan_engine_ids is a persisted snapshot of the plan's engine set.
		// The payment/subscription lifecycle may populate it when a plan is
		// applied; keeping the reader here also supports existing imports that
		// already carry it in metadata.
		PlanEngineIDs: GetMetadataStringSlice(sub.Metadata, "plan_engine_ids"),
	}
}

// GetMetadataStringSlice extracts a stable, cleaned string slice from a JSON
// metadata value. YAML/JSON decoders commonly produce []any while callers
// constructing domain values directly often use []string, so both forms are
// accepted. Empty values and duplicates are ignored without changing order.
func GetMetadataStringSlice(m domain.Metadata, key string) []string {
	if m == nil {
		return nil
	}

	var raw []string
	switch value := m[key].(type) {
	case []string:
		raw = value
	case []interface{}:
		raw = make([]string, 0, len(value))
		for _, item := range value {
			if text, ok := item.(string); ok {
				raw = append(raw, text)
			}
		}
	case string:
		raw = strings.Split(value, ",")
	default:
		return nil
	}

	seen := make(map[string]struct{}, len(raw))
	result := make([]string, 0, len(raw))
	for _, value := range raw {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}
