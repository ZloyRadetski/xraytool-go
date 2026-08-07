//go:build minimal

package cmd

import (
	"xraytool/internal/domain"
	"xraytool/internal/pluginapi"
)

// configureOptionalPluginFactories is deliberately empty in a minimal build.
// If a configuration still enables a built-in optional plugin, Host.Load
// reports that it is unavailable instead of silently changing behaviour.
func configureOptionalPluginFactories(
	_ map[string]func() pluginapi.Plugin,
	_ *AppDeps,
	_ domain.Engine,
	_ domain.FraudEventReporter,
) {
}
