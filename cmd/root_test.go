package cmd

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRootCommandWithoutArgumentsPrintsASCIILogo(t *testing.T) {
	root := NewRootCmd()
	var output bytes.Buffer
	root.SetOut(&output)

	require.NotNil(t, root.Run)
	root.Run(root, nil)

	printed := output.String()
	require.True(t, len(printed) >= len(asciiLogo))
	require.Equal(t, asciiLogo, printed[:len(asciiLogo)])
	require.Contains(t, printed, "Available Commands:")
}
