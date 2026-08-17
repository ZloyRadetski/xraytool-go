package engine_xray

import (
	"fmt"
	json "github.com/goccy/go-json"
	"log/slog"

	"xraytool/internal/domain"
)

// MergeUsers writes one desired user snapshot into the template's structural
// skeleton. It never changes the template object passed by the caller.
//
// For every inbound with a client list, email-bearing template clients are
// replaced by fresh clients built from users. Anonymous entries are retained
// because they cannot be represented by VPNUserConfig.
//
// The returned RawConfig is a fully populated xray config ready to write to disk.
func MergeUsers(template RawConfig, users []domain.VPNUserConfig) (RawConfig, error) {
	// Clone the template map to prevent side-effects on the caller's template object.
	cloned := make(RawConfig, len(template))
	for k, v := range template {
		cloned[k] = v
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

		// Preserve only anonymous records that cannot participate in a snapshot.
		var static []RawClient
		for _, c := range existing {
			if c.Email() == "" {
				static = append(static, c)
			}
		}

		// Build the complete desired client list, scoped to this inbound.
		dynamic, err := buildDynamicClients(ib, users)
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

// buildDynamicClients builds RawClient objects for every desired user, using the
// inbound's protocol to determine field layout.
func buildDynamicClients(ib RawInbound, users []domain.VPNUserConfig) ([]RawClient, error) {
	result := make([]RawClient, 0, len(users))
	seen := make(map[string]bool, len(users))
	for _, u := range users {
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

// RegenerateConfig reads the template, applies the desired user snapshot, and writes the result
// to configPath atomically. This is the single entry point for config generation.
func RegenerateConfig(templatePath, configPath string, users []domain.VPNUserConfig, realityRotation bool, realityKeysPath string) error {
	template, err := Read(templatePath)
	if err != nil {
		return fmt.Errorf("regenerate: read template %q: %w", templatePath, err)
	}

	if realityKeysPath != "" {
		var keys *RealityKeys
		var err error
		if realityRotation {
			keys, err = LoadOrCreateRealityKeys(realityKeysPath)
		} else {
			keys, err = LoadRealityKeys(realityKeysPath)
		}
		if err == nil {
			if err := injectRealityKeys(template, keys); err != nil {
				return fmt.Errorf("regenerate: inject reality keys: %w", err)
			}
		} else {
			if realityRotation {
				return fmt.Errorf("regenerate: load or create reality keys: %w", err)
			} else {
				slog.Default().Warn("template: reality keys file not found or invalid on slave node, skipping injection", "path", realityKeysPath, "err", err)
			}
		}
	}

	merged, err := MergeUsers(template, users)
	if err != nil {
		return fmt.Errorf("regenerate: merge users: %w", err)
	}

	// Preserve existing dynamic routing and outbounds from the live config if it exists
	if existing, err := Read(configPath); err == nil && existing != nil {
		if rawRouting, ok := existing["routing"]; ok && len(rawRouting) > 0 {
			merged["routing"] = rawRouting
		}
		if rawOutbounds, ok := existing["outbounds"]; ok && len(rawOutbounds) > 0 {
			merged["outbounds"] = rawOutbounds
		}
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
		proto := ib.Protocol()
		if proto != "vless" && proto != "xhttp" && proto != "splithttp" {
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
