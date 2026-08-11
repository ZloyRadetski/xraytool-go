package engine_xray

import (
	"context"
	"fmt"
	"os"
	"strings"

	json "github.com/goccy/go-json"

	"xraytool/internal/pluginapi"
)

// SubscriptionConfigSnapshot projects the Xray configuration into the small,
// engine-neutral shape required by subscription delivery. Raw Xray JSON never
// leaves this package.
func (p *Plugin) SubscriptionConfigSnapshot(ctx context.Context) (pluginapi.SubscriptionConfigSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return pluginapi.SubscriptionConfigSnapshot{}, err
	}
	path := strings.TrimSpace(p.cfg.ConfigPath)
	if path == "" {
		return pluginapi.SubscriptionConfigSnapshot{}, fmt.Errorf("engine_xray: subscription config requires config_path")
	}
	config, err := Read(path)
	if err != nil {
		return pluginapi.SubscriptionConfigSnapshot{}, fmt.Errorf("engine_xray: read active config: %w", err)
	}

	snapshot := pluginapi.SubscriptionConfigSnapshot{
		ActiveClients:      subscriptionClients(config),
		RealityServerName:  firstRealitySNI(config),
		ShadowSocksSecret:  ssServerPassword(config),
		HysteriaObfsSecret: getOrCreateHy2ObfsPassword(p.cfg.Hy2ConfigYAML, config),
	}
	if info, statErr := os.Stat(path); statErr == nil {
		snapshot.Revision = info.ModTime().UnixNano()
	}

	if p.cfg.RealityRotation && strings.TrimSpace(p.cfg.RealityKeysPath) != "" {
		if keys, keysErr := LoadRealityKeys(p.cfg.RealityKeysPath); keysErr == nil {
			snapshot.RealityPublicKey = keys.PublicKey
			snapshot.RealityShortIDs = append([]string(nil), keys.ShortIDs...)
		}
		if info, statErr := os.Stat(p.cfg.RealityKeysPath); statErr == nil && info.ModTime().UnixNano() > snapshot.Revision {
			snapshot.Revision = info.ModTime().UnixNano()
		}
	}
	if snapshot.RealityPublicKey == "" {
		snapshot.RealityPublicKey = derivePublicKey(firstRealityPrivateKey(config))
		if snapshot.RealityPublicKey == "" {
			snapshot.RealityPublicKey = firstRealityPublicKey(config)
		}
	}
	if len(snapshot.RealityShortIDs) == 0 {
		snapshot.RealityShortIDs = realityShortIDs(config)
	}

	if templatePath := strings.TrimSpace(p.cfg.TemplatePath); templatePath != "" {
		if template, templateErr := Read(templatePath); templateErr == nil {
			snapshot.TemplateClients = subscriptionClients(template)
			if info, statErr := os.Stat(templatePath); statErr == nil && info.ModTime().UnixNano() > snapshot.Revision {
				snapshot.Revision = info.ModTime().UnixNano()
			}
		}
	}
	return snapshot, nil
}

func subscriptionClients(config RawConfig) []pluginapi.SubscriptionClient {
	inbounds, err := config.GetInbounds()
	if err != nil {
		return nil
	}
	clients := make([]pluginapi.SubscriptionClient, 0)
	for _, inbound := range inbounds {
		entries, clientsErr := inbound.GetClients()
		if clientsErr != nil {
			continue
		}
		for _, entry := range entries {
			maxDevices := 3
			if limit, ok := entry.GetNumber("limit"); ok && limit > 0 {
				maxDevices = int(limit)
			}
			clients = append(clients, pluginapi.SubscriptionClient{
				Email:      entry.Email(),
				ID:         entry.GetString("id"),
				Password:   entry.GetString("password"),
				Auth:       entry.GetString("auth"),
				Obfs:       entry.GetString("hy2_obfs"),
				Subfile:    entry.GetString("subfile"),
				Expire:     entry.GetString("expire"),
				MaxDevices: maxDevices,
			})
		}
	}
	return clients
}

func realityShortIDs(config RawConfig) []string {
	inbounds, err := config.GetInbounds()
	if err != nil {
		return nil
	}
	for _, inbound := range inbounds {
		stream, found := inbound["streamSettings"]
		if !found {
			continue
		}
		var streamSettings map[string]json.RawMessage
		if json.Unmarshal(stream, &streamSettings) != nil {
			continue
		}
		reality, found := streamSettings["realitySettings"]
		if !found {
			continue
		}
		var settings struct {
			ShortIDs []string `json:"shortIds"`
		}
		if json.Unmarshal(reality, &settings) != nil {
			continue
		}
		out := make([]string, 0, len(settings.ShortIDs))
		for _, id := range settings.ShortIDs {
			if id = strings.TrimSpace(id); id != "" {
				out = append(out, id)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

var _ pluginapi.SubscriptionConfigProvider = (*Plugin)(nil)
