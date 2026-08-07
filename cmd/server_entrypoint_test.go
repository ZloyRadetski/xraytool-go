package cmd

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStartServerUsesPluginHostEntrypoint(t *testing.T) {
	root := NewRootCmd()

	serverCmd, _, err := root.Find([]string{"start-server"})
	require.NoError(t, err)
	require.Equal(t, "start-server", serverCmd.Name())
	require.Contains(t, serverCmd.Long, "Plugin Host")

	legacyAlias, _, err := root.Find([]string{"start-server-v2"})
	require.NoError(t, err)
	require.True(t, legacyAlias.Hidden)
	require.NotEmpty(t, legacyAlias.Deprecated)
}
