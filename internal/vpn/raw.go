// Package xrayconfig provides types and helpers for reading and manipulating
// the xray-core config.json in a way that preserves all unknown fields.
//
// The core design uses map[string]json.RawMessage at every level so that
// round-tripping through JSON never silently drops fields that xraytool
// does not know about.
package vpn

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ---------------------------------------------------------------------------
// Top-level config
// ---------------------------------------------------------------------------

// RawConfig is a faithful, field-preserving representation of xray config.json.
type RawConfig map[string]json.RawMessage

// GetInbounds extracts the inbounds array from the config.
func (c RawConfig) GetInbounds() ([]RawInbound, error) {
	raw, ok := c["inbounds"]
	if !ok {
		return nil, nil
	}
	var inbounds []RawInbound
	if err := json.Unmarshal(raw, &inbounds); err != nil {
		return nil, fmt.Errorf("parsing inbounds: %w", err)
	}
	return inbounds, nil
}

// SetInbounds replaces the inbounds array in the config.
func (c RawConfig) SetInbounds(inbounds []RawInbound) error {
	if c == nil {
		return fmt.Errorf("RawConfig is nil")
	}
	data, err := json.Marshal(inbounds)
	if err != nil {
		return fmt.Errorf("marshaling inbounds: %w", err)
	}
	c["inbounds"] = data
	return nil
}

// ---------------------------------------------------------------------------
// Inbound
// ---------------------------------------------------------------------------

// RawInbound is a field-preserving representation of a single inbound entry.
type RawInbound map[string]json.RawMessage

// Tag returns the inbound's tag string.
func (ib RawInbound) Tag() string {
	return rawString(ib, "tag")
}

// Protocol returns the inbound's protocol, lower-cased.
func (ib RawInbound) Protocol() string {
	return strings.ToLower(rawString(ib, "protocol"))
}

// IsHysteria returns true for hysteria / hysteria2 / hy2 protocols.
func (ib RawInbound) IsHysteria() bool {
	p := ib.Protocol()
	return p == "hysteria" || p == "hysteria2" || p == "hy2"
}

// HasClientList returns true if settings.clients or settings.users exists.
func (ib RawInbound) HasClientList() bool {
	return ib.clientsKey() != ""
}

// clientsKey returns the actual key used in settings ("clients" or "users"),
// or "" if neither exists. It reads from the actual JSON rather than guessing
// from the protocol, so the config always wins.
func (ib RawInbound) clientsKey() string {
	settings, err := ib.parseSettings()
	if err != nil {
		return ""
	}
	if _, ok := settings["clients"]; ok {
		return "clients"
	}
	if _, ok := settings["users"]; ok {
		return "users"
	}
	return ""
}

// ClientsKey is the exported version used outside this package.
func (ib RawInbound) ClientsKey() string {
	return ib.clientsKey()
}

// GetClients returns the clients (or users) from this inbound's settings.
// Returns nil slice (not error) when the inbound has no client list at all.
func (ib RawInbound) GetClients() ([]RawClient, error) {
	settings, err := ib.parseSettings()
	if err != nil {
		return nil, err
	}

	for _, key := range []string{"clients", "users"} {
		rawClients, ok := settings[key]
		if !ok {
			continue
		}
		var clients []RawClient
		if err := json.Unmarshal(rawClients, &clients); err != nil {
			return nil, fmt.Errorf("parsing %s: %w", key, err)
		}
		return clients, nil
	}
	return nil, nil
}

// SetClients replaces the client list in settings, using whichever key
// ("clients" or "users") was already present. Falls back to protocol-based
// detection for empty inbounds.
func (ib RawInbound) SetClients(clients []RawClient) error {
	settings, err := ib.parseSettings()
	if err != nil {
		return err
	}

	// Determine the key to write.
	key := ""
	if _, ok := settings["clients"]; ok {
		key = "clients"
	} else if _, ok := settings["users"]; ok {
		key = "users"
	} else {
		// Nothing set yet: fall back to protocol.
		if ib.IsHysteria() {
			key = "users"
		} else {
			key = "clients"
		}
	}

	// Clean legacy fields from every client.
	cleaned := make([]RawClient, len(clients))
	for i, c := range clients {
		cleaned[i] = c.cleanLegacy()
	}

	data, err := json.Marshal(cleaned)
	if err != nil {
		return err
	}
	settings[key] = data

	// Persist settings back to the inbound.
	newSettings, err := json.Marshal(settings)
	if err != nil {
		return err
	}
	ib["settings"] = newSettings
	return nil
}

func (ib RawInbound) parseSettings() (map[string]json.RawMessage, error) {
	rawSettings, ok := ib["settings"]
	if !ok {
		return make(map[string]json.RawMessage), nil
	}
	var s map[string]json.RawMessage
	if err := json.Unmarshal(rawSettings, &s); err != nil {
		return nil, fmt.Errorf("parsing inbound settings: %w", err)
	}
	if s == nil {
		s = make(map[string]json.RawMessage)
	}
	return s, nil
}

// ---------------------------------------------------------------------------
// Client / User object
// ---------------------------------------------------------------------------

// RawClient is a field-preserving representation of a single client/user entry.
type RawClient map[string]json.RawMessage

// metaFields are fields stored by xraytool in config.json that must be
// stripped before sending the client to the xray API.
var metaFields = map[string]bool{
	"subfile": true,
	"expire":  true,
	"limit":   true,
}

// ForXrayAPI returns a copy of the client with all xraytool metadata fields
// removed. This is the object safe to pass to xray api adu.
func (c RawClient) ForXrayAPI() RawClient {
	result := make(RawClient, len(c))
	for k, v := range c {
		if !metaFields[k] {
			result[k] = v
		}
	}
	return result
}

// cleanLegacy removes deprecated compatibility fields (hy2_auth, hy2_obfs).
func (c RawClient) cleanLegacy() RawClient {
	result := make(RawClient, len(c))
	for k, v := range c {
		result[k] = v
	}
	delete(result, "hy2_auth")
	delete(result, "hy2_obfs")
	return result
}

// Email returns the user's email identifier. Falls back to "name" for
// protocols that use it (e.g. some hysteria2 configurations).
func (c RawClient) Email() string {
	if e := c.GetString("email"); e != "" {
		return e
	}
	return c.GetString("name")
}

// GetString returns the string value of a field, or "" if missing/null.
func (c RawClient) GetString(key string) string {
	raw, ok := c[key]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		// Try coercing a number to string.
		var n json.Number
		if err2 := json.Unmarshal(raw, &n); err2 == nil {
			return strings.TrimSpace(n.String())
		}
		return ""
	}
	return strings.TrimSpace(s)
}

// GetNumber returns a numeric field's value and whether it existed.
func (c RawClient) GetNumber(key string) (float64, bool) {
	raw, ok := c[key]
	if !ok {
		return 0, false
	}
	var n float64
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, false
	}
	return n, true
}

// Has returns true if the key exists in the client object (even if null/empty).
func (c RawClient) Has(key string) bool {
	_, ok := c[key]
	return ok
}

// Set sets a string field.
func (c RawClient) Set(key, value string) {
	data, err := json.Marshal(value)
	if err != nil {
		return // marshaling a string should never fail
	}
	c[key] = data
}

// SetNumber sets a numeric field.
func (c RawClient) SetNumber(key string, value float64) {
	data, err := json.Marshal(value)
	if err != nil {
		return // NaN or Inf — skip
	}
	c[key] = data
}

// Delete removes a field.
func (c RawClient) Delete(key string) {
	delete(c, key)
}

// ---------------------------------------------------------------------------
// TaggedClient — associates a built client with an inbound tag
// ---------------------------------------------------------------------------

// TaggedClient associates a ready-to-insert client object with an inbound tag.
type TaggedClient struct {
	Tag    string
	Client RawClient
}

// ---------------------------------------------------------------------------
// InboundInfo — lightweight inbound descriptor
// ---------------------------------------------------------------------------

// InboundInfo carries the essential identity of an inbound.
type InboundInfo struct {
	Tag      string
	Protocol string
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func rawString(m map[string]json.RawMessage, key string) string {
	raw, ok := m[key]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return strings.TrimSpace(s)
}
