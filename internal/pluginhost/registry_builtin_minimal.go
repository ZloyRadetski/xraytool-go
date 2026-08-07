//go:build minimal

// Package pluginhost contains the minimal built-in plugin registry.
package pluginhost

import (
	"xraytool/internal/appconfig"
	"xraytool/internal/pluginapi"
	corePlugin "xraytool/internal/plugins/core"
	enginePlugin "xraytool/internal/plugins/engine_xray"
	pricingPlugin "xraytool/internal/plugins/pricing_default"
)

// BuiltinRegistry returns only the plugins required for a minimal server.
// Optional business integrations are expected to be configured as external
// plugins in this build.
func BuiltinRegistry(cfg *appconfig.Config) map[string]func() pluginapi.Plugin {
	return map[string]func() pluginapi.Plugin{
		"core":            func() pluginapi.Plugin { return corePlugin.New(cfg) },
		"pricing_default": func() pluginapi.Plugin { return pricingPlugin.New() },
		"engine_xray":     func() pluginapi.Plugin { return enginePlugin.New() },
	}
}
