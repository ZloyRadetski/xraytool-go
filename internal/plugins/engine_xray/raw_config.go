package engine_xray

import (
	"fmt"
	"strings"

	json "github.com/goccy/go-json"
)

// RawConfig is the Xray engine's field-preserving representation of config.json.
// It intentionally lives with the engine: no other plugin is allowed to parse
// or mutate Xray's native JSON format.
type RawConfig map[string]json.RawMessage

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

// RawInbound preserves unknown inbound fields while exposing the operations
// required by the Xray adapter.
type RawInbound map[string]json.RawMessage

func (ib RawInbound) Tag() string      { return rawString(ib, "tag") }
func (ib RawInbound) Protocol() string { return strings.ToLower(rawString(ib, "protocol")) }
func (ib RawInbound) IsHysteria() bool {
	p := ib.Protocol()
	return p == "hysteria" || p == "hysteria2" || p == "hy2"
}
func (ib RawInbound) HasClientList() bool { return ib.clientsKey() != "" }
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
func (ib RawInbound) ClientsKey() string { return ib.clientsKey() }

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

func (ib RawInbound) SetClients(clients []RawClient) error {
	settings, err := ib.parseSettings()
	if err != nil {
		return err
	}
	key := ""
	if _, ok := settings["clients"]; ok {
		key = "clients"
	} else if _, ok := settings["users"]; ok {
		key = "users"
	} else if ib.IsHysteria() {
		key = "users"
	} else {
		key = "clients"
	}
	cleaned := make([]RawClient, len(clients))
	for i, client := range clients {
		cleaned[i] = client.cleanLegacy()
	}
	data, err := json.Marshal(cleaned)
	if err != nil {
		return err
	}
	settings[key] = data
	data, err = json.Marshal(settings)
	if err != nil {
		return err
	}
	ib["settings"] = data
	return nil
}

func (ib RawInbound) parseSettings() (map[string]json.RawMessage, error) {
	rawSettings, ok := ib["settings"]
	if !ok {
		return make(map[string]json.RawMessage), nil
	}
	var settings map[string]json.RawMessage
	if err := json.Unmarshal(rawSettings, &settings); err != nil {
		return nil, fmt.Errorf("parsing inbound settings: %w", err)
	}
	if settings == nil {
		settings = make(map[string]json.RawMessage)
	}
	return settings, nil
}

func (ib RawInbound) Settings() (map[string]json.RawMessage, error) { return ib.parseSettings() }

// RawClient preserves unknown client fields and strips xraytool-only metadata
// only when a payload is sent to Xray's gRPC API.
type RawClient map[string]json.RawMessage

var metaFields = map[string]bool{"subfile": true, "expire": true, "limit": true}

func (c RawClient) ForXrayAPI() RawClient {
	result := make(RawClient, len(c))
	for key, value := range c {
		if !metaFields[key] {
			result[key] = value
		}
	}
	return result
}
func (c RawClient) cleanLegacy() RawClient {
	result := make(RawClient, len(c))
	for key, value := range c {
		result[key] = value
	}
	delete(result, "hy2_auth")
	delete(result, "hy2_obfs")
	return result
}
func (c RawClient) CleanLegacy() RawClient { return c.cleanLegacy() }
func (c RawClient) Email() string {
	if email := c.GetString("email"); email != "" {
		return email
	}
	return c.GetString("name")
}
func (c RawClient) GetString(key string) string {
	raw, ok := c[key]
	if !ok {
		return ""
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		var number json.Number
		if err := json.Unmarshal(raw, &number); err == nil {
			return strings.TrimSpace(number.String())
		}
		return ""
	}
	return strings.TrimSpace(value)
}
func (c RawClient) GetNumber(key string) (float64, bool) {
	raw, ok := c[key]
	if !ok {
		return 0, false
	}
	var value float64
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, false
	}
	return value, true
}
func (c RawClient) Has(key string) bool { _, ok := c[key]; return ok }
func (c RawClient) Set(key, value string) {
	data, err := json.Marshal(value)
	if err == nil {
		c[key] = data
	}
}
func (c RawClient) SetNumber(key string, value float64) {
	data, err := json.Marshal(value)
	if err == nil {
		c[key] = data
	}
}
func (c RawClient) Delete(key string) { delete(c, key) }

type TaggedClient struct {
	Tag    string
	Client RawClient
}

type InboundInfo struct {
	Tag      string
	Protocol string
}

func rawString(m map[string]json.RawMessage, key string) string {
	raw, ok := m[key]
	if !ok {
		return ""
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return ""
	}
	return strings.TrimSpace(value)
}
