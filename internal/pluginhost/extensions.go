package pluginhost

import (
	"strings"

	"xraytool/internal/pluginapi"
)

// NotificationProviders returns the loaded notification providers that accept
// channel. The result follows the deterministic plugin load order, so callers
// can fan out notifications without depending on concrete plugin types.
func (h *Host) NotificationProviders(channel string) []pluginapi.NotificationProvider {
	channel = strings.TrimSpace(channel)
	if channel == "" {
		return nil
	}

	// Plugins are user-controlled code. Snapshot under the Host mutex, then
	// call Channels outside it so a provider cannot block Shutdown or deadlock
	// by calling back into Host while declaring its capabilities.
	h.mu.RLock()
	loaded := append([]loadedPlugin(nil), h.loaded...)
	h.mu.RUnlock()

	providers := make([]pluginapi.NotificationProvider, 0)
	for _, lp := range loaded {
		if external, ok := lp.plugin.(*externalPlugin); ok && !external.publishes("notification_provider") {
			continue
		}
		provider, ok := lp.plugin.(pluginapi.NotificationProvider)
		if !ok {
			continue
		}
		for _, supportedChannel := range provider.Channels() {
			if strings.EqualFold(strings.TrimSpace(supportedChannel), channel) {
				providers = append(providers, provider)
				break
			}
		}
	}
	return providers
}
