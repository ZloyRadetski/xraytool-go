package pluginhost

import (
	"context"
	"fmt"
	"strings"

	"xraytool/internal/pluginapi"
)

// VerifyExternalPlugin performs the same go-plugin handshake and metadata
// validation used by Host.Load, without initialising or starting the plugin.
// It is intended for deployment tooling such as `xraytool plugin verify`.
func VerifyExternalPlugin(ctx context.Context, name, executable string, args []string) (pluginapi.Metadata, error) {
	if ctx == nil {
		return pluginapi.Metadata{}, fmt.Errorf("pluginhost: verification context must not be nil")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return pluginapi.Metadata{}, fmt.Errorf("pluginhost: external plugin name must not be empty")
	}
	plugin := newExternalPlugin(name, PluginEntry{
		Enabled: true,
		Source:  "external",
		Exec:    executable,
		Args:    append([]string(nil), args...),
	}, nil, false)
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), externalPluginRPCTimeout)
		defer cancel()
		plugin.AbortPreflight(stopCtx)
	}()

	if err := plugin.Preflight(ctx); err != nil {
		return pluginapi.Metadata{}, err
	}
	metadata := plugin.Metadata()
	if !apiVersionSupported(metadata.APIVersion) {
		return pluginapi.Metadata{}, fmt.Errorf("external plugin %q requires Plugin API %q, but host supports %q", name, metadata.APIVersion, pluginapi.CurrentAPIVersion)
	}
	plugin.mu.RLock()
	capabilities := cloneExternalCapabilities(plugin.caps)
	plugin.mu.RUnlock()
	if err := validateExternalPublications(name, metadata, capabilities); err != nil {
		return pluginapi.Metadata{}, err
	}
	return metadata, nil
}
