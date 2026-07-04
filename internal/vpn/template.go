package vpn

import (
	"encoding/json"
	"fmt"
	"log/slog"

	"xraytool/internal/domain"
)

// MergeUsers merges DB users into the template config, preserving static
// (non-DB) clients like reverse-proxy and admin accounts.
//
// Algorithm for every inbound that has a client list:
//  1. Read existing clients from the template.
//  2. Separate:
//     - static: clients whose email is NOT in dbUsers → keep as-is.
//     - dynamic: generate fresh client objects from dbUsers via BuildClient.
//  3. Final clients = static + dynamic (DB always wins on collision).
//
// The returned RawConfig is a fully populated xray config ready to write to disk.
func MergeUsers(template RawConfig, dbUsers []domain.VPNUserConfig) (RawConfig, error) {
	// Clone the template map to prevent side-effects on the caller's template object.
	cloned := make(RawConfig, len(template))
	for k, v := range template {
		cloned[k] = v
	}

	// Build DB email index.
	dbEmails := make(map[string]domain.VPNUserConfig, len(dbUsers))
	for _, u := range dbUsers {
		if u.Email == "" {
			continue
		}
		dbEmails[u.Email] = u
	}

	inbounds, err := cloned.GetInbounds()
	if err != nil {
		return nil, fmt.Errorf("merge: read inbounds: %w", err)
	}

	for i, ib := range inbounds {
		if !ib.HasClientList() {
			continue
		}

		existing, err := ib.GetClients()
		if err != nil {
			return nil, fmt.Errorf("merge: inbound %q: read clients: %w", ib.Tag(), err)
		}

		// 1. Keep clients whose email is NOT in DB — they're static.
		var static []RawClient
		for _, c := range existing {
			email := c.Email()
			if email == "" {
				// Empty-email clients are always kept (malformed/edge-case).
				static = append(static, c)
				continue
			}
			if _, isDB := dbEmails[email]; !isDB {
				static = append(static, c)
			}
		}

		// 2. Build dynamic clients from DB users, scoped to this inbound.
		dynamic, err := buildDynamicClients(ib, dbUsers)
		if err != nil {
			return nil, fmt.Errorf("merge: inbound %q: build dynamic clients: %w", ib.Tag(), err)
		}

		if err := inbounds[i].SetClients(append(static, dynamic...)); err != nil {
			return nil, fmt.Errorf("merge: inbound %q: set clients: %w", ib.Tag(), err)
		}
	}

	if err := cloned.SetInbounds(inbounds); err != nil {
		return nil, fmt.Errorf("merge: write inbounds: %w", err)
	}

	return cloned, nil
}

// buildDynamicClients builds RawClient objects for every DB user, using the
// inbound's protocol to determine field layout.
func buildDynamicClients(ib RawInbound, dbUsers []domain.VPNUserConfig) ([]RawClient, error) {
	result := make([]RawClient, 0, len(dbUsers))
	seen := make(map[string]bool, len(dbUsers))
	for _, u := range dbUsers {
		if u.Email == "" {
			continue
		}
		if seen[u.Email] {
			continue
		}
		seen[u.Email] = true

		params := configToParams(u)
		client, err := BuildClient(ib, params)
		if err != nil {
			// Log warning and skip this user for this inbound.
			slog.Default().Warn("template: skipping user for inbound",
				"email", u.Email,
				"inbound", ib.Tag(),
				"protocol", ib.Protocol(),
				"err", err,
			)
			continue
		}
		result = append(result, client)
	}
	return result, nil
}

// RegenerateConfig reads the template, merges DB users, and writes the result
// to configPath atomically. This is the single entry point for config generation.
func RegenerateConfig(templatePath, configPath string, dbUsers []domain.VPNUserConfig, realityRotation bool, realityKeysPath string) error {
	template, err := Read(templatePath)
	if err != nil {
		return fmt.Errorf("regenerate: read template %q: %w", templatePath, err)
	}

	if realityRotation && realityKeysPath != "" {
		keys, err := LoadOrCreateRealityKeys(realityKeysPath)
		if err != nil {
			return fmt.Errorf("regenerate: load or create reality keys: %w", err)
		}
		if err := injectRealityKeys(template, keys); err != nil {
			return fmt.Errorf("regenerate: inject reality keys: %w", err)
		}
	}

	merged, err := MergeUsers(template, dbUsers)
	if err != nil {
		return fmt.Errorf("regenerate: merge users: %w", err)
	}

	if err := Write(configPath, merged); err != nil {
		return fmt.Errorf("regenerate: write config %q: %w", configPath, err)
	}

	return nil
}

// injectRealityKeys replaces privateKey and shortIds settings in the template config.
func injectRealityKeys(cfg RawConfig, keys *RealityKeys) error {
	inbounds, err := cfg.GetInbounds()
	if err != nil {
		return err
	}

	modified := false
	for i, ib := range inbounds {
		if ib.Protocol() != "vless" {
			continue
		}

		rawStream, ok := ib["streamSettings"]
		if !ok {
			continue
		}

		var stream map[string]json.RawMessage
		if err := json.Unmarshal(rawStream, &stream); err != nil {
			continue
		}

		rawSec, ok := stream["security"]
		if !ok {
			continue
		}
		var sec string
		if err := json.Unmarshal(rawSec, &sec); err != nil || sec != "reality" {
			continue
		}

		rawReality, ok := stream["realitySettings"]
		if !ok {
			continue
		}

		var reality map[string]json.RawMessage
		if err := json.Unmarshal(rawReality, &reality); err != nil {
			continue
		}

		// Inject privateKey
		privData, err := json.Marshal(keys.PrivateKey)
		if err != nil {
			return err
		}
		reality["privateKey"] = privData

		// Inject shortIds
		sidsData, err := json.Marshal(keys.ShortIDs)
		if err != nil {
			return err
		}
		reality["shortIds"] = sidsData

		// Marshal realitySettings back
		newReality, err := json.Marshal(reality)
		if err != nil {
			return err
		}
		stream["realitySettings"] = newReality

		// Marshal streamSettings back
		newStream, err := json.Marshal(stream)
		if err != nil {
			return err
		}
		inbounds[i]["streamSettings"] = newStream
		modified = true
	}

	if modified {
		return cfg.SetInbounds(inbounds)
	}
	return nil
}