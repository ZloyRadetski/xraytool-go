package pluginhost

import (
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"

	"xraytool/internal/pluginmanifest"
)

// TestBuiltinManifestMetadataParity prevents a built-in plugin's declarative
// manifest from drifting away from the contract actually compiled into the
// binary. The host validates Metadata at startup, while deployment tooling
// validates plugin.yaml, so both views must stay identical.
func TestBuiltinManifestMetadataParity(t *testing.T) {
	factories := BuiltinRegistry(nil)
	names := make([]string, 0, len(factories))
	for name := range factories {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		name := name
		t.Run(name, func(t *testing.T) {
			manifest, err := pluginmanifest.Load(filepath.Join("..", "plugins", name, "plugin.yaml"))
			require.NoError(t, err)

			plugin := factories[name]()
			require.NotNil(t, plugin)
			runtime := plugin.Metadata()
			declared := manifest.Metadata()

			require.Equal(t, declared.Name, runtime.Name)
			require.Equal(t, declared.Kind, runtime.Kind)
			require.Equal(t, declared.Version, runtime.Version)
			require.Equal(t, declared.APIVersion, runtime.APIVersion)
			require.Equal(t, declared.Mandatory, runtime.Mandatory)
			require.ElementsMatch(t, declared.Publishes, runtime.Publishes)
			require.ElementsMatch(t, declared.Requires, runtime.Requires)
		})
	}
}
