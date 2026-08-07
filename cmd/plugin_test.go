package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

const pluginCommandConfig = `# keep this comment and every unknown section
unknown_extension:
  nested:
    keep: true
plugins:
  core:
    enabled: true
    source: builtin
  antifraud:
    enabled: false
    source: builtin
    config:
      max_ips: 5
engines:
  routing_mode: broadcast
  xray:
    enabled: true
    source: builtin
`

func TestPluginDisablePreservesUnknownYAMLSections(t *testing.T) {
	configPath := writePluginCommandConfig(t, pluginCommandConfig)

	output, err := executePluginCommand(t, configPath, "disable", "antifraud")
	require.NoError(t, err)
	require.Contains(t, output, `Plugin "antifraud" disabled`)

	data, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.Contains(t, string(data), "# keep this comment")

	var decoded map[string]any
	require.NoError(t, yaml.Unmarshal(data, &decoded))
	unknown := decoded["unknown_extension"].(map[string]any)
	nested := unknown["nested"].(map[string]any)
	require.Equal(t, true, nested["keep"])

	plugins := decoded["plugins"].(map[string]any)
	antifraud := plugins["antifraud"].(map[string]any)
	require.Equal(t, false, antifraud["enabled"])
	require.Equal(t, "builtin", antifraud["source"])
}

func TestPluginEnableAddsKnownBuiltinWithoutChangingOtherSections(t *testing.T) {
	configPath := writePluginCommandConfig(t, pluginCommandConfig)

	_, err := executePluginCommand(t, configPath, "enable", "pricing_default")
	require.NoError(t, err)

	data, err := os.ReadFile(configPath)
	require.NoError(t, err)
	var decoded map[string]any
	require.NoError(t, yaml.Unmarshal(data, &decoded))
	plugins := decoded["plugins"].(map[string]any)
	pricing := plugins["pricing_default"].(map[string]any)
	require.Equal(t, true, pricing["enabled"])
	require.Equal(t, "builtin", pricing["source"])
	require.NotNil(t, decoded["unknown_extension"])
}

func TestPluginDisableCoreReturnsClearError(t *testing.T) {
	configPath := writePluginCommandConfig(t, pluginCommandConfig)

	_, err := executePluginCommand(t, configPath, "disable", "core")
	require.Error(t, err)
	require.ErrorContains(t, err, `plugin "core" is mandatory and cannot be disabled`)
}

func TestPluginListAndGraphDoNotLoadRuntimeDependencies(t *testing.T) {
	configPath := writePluginCommandConfig(t, pluginCommandConfig)

	listOutput, err := executePluginCommand(t, configPath, "list")
	require.NoError(t, err)
	require.Contains(t, listOutput, "NAME")
	require.Contains(t, listOutput, "antifraud")
	require.Contains(t, listOutput, "core")

	graphOutput, err := executePluginCommand(t, configPath, "graph")
	require.NoError(t, err)
	require.Contains(t, graphOutput, "Plugin dependency graph (load order):")
	corePosition := strings.Index(graphOutput, "core (core)")
	enginePosition := strings.Index(graphOutput, "engine_xray (engine)")
	require.GreaterOrEqual(t, corePosition, 0)
	require.GreaterOrEqual(t, enginePosition, 0)
	require.Less(t, corePosition, enginePosition)
}

func TestPluginVerify(t *testing.T) {
	configPath := writePluginCommandConfig(t, pluginCommandConfig)
	manifestPath := filepath.Join(t.TempDir(), "plugin.yaml")
	require.NoError(t, os.WriteFile(manifestPath, []byte(`
name: sample-notifier
kind: notification
version: 1.0.0
api_version: "1"
type: external
requires:
  - name: user_repository
    optional: true
`), 0600))

	verifyOutput, err := executePluginCommand(t, configPath, "verify", manifestPath)
	require.NoError(t, err)
	require.Contains(t, verifyOutput, "Manifest")
	require.Contains(t, verifyOutput, "sample-notifier")

}

func TestPluginLogsReadsPersistentExternalLog(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "remote-payment.log")
	require.NoError(t, os.WriteFile(logPath, []byte("first line\nsecond line\n"), 0600))
	configPath := writePluginCommandConfig(t, `
plugins:
  core:
    enabled: true
    source: builtin
  remote-payment:
    enabled: true
    source: external
    exec: /opt/xraytool/plugins/remote-payment
    log_path: `+logPath+`
`)

	output, err := executePluginCommand(t, configPath, "logs", "remote-payment", "--tail", "1")
	require.NoError(t, err)
	require.Equal(t, "second line\n", output)
}

func TestPluginGraphLoadsExternalManifestWithoutStartingProcess(t *testing.T) {
	configDir := t.TempDir()
	pluginDir := filepath.Join(configDir, "plugins", "remote-payment")
	require.NoError(t, os.MkdirAll(pluginDir, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(pluginDir, "plugin.yaml"), []byte(`
name: remote-payment
kind: payment
version: 1.2.3
api_version: "1"
type: external
publishes:
  - payment_provider.remote
requires: []
`), 0600))
	configPath := filepath.Join(configDir, "config.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(`
plugins:
  core:
    enabled: true
    source: builtin
  pricing_default:
    enabled: true
    source: builtin
  remote-payment:
    enabled: true
    source: external
    exec: ./plugins/remote-payment/xraytool-plugin-remote-payment
engines:
  xray:
    enabled: true
    source: builtin
`), 0600))

	output, err := executePluginCommand(t, configPath, "graph")
	require.NoError(t, err)
	require.Contains(t, output, "remote-payment (payment)")
	require.Contains(t, output, "payment_provider.remote")
}

func writePluginCommandConfig(t *testing.T, config string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(config), 0600))
	return path
}

func executePluginCommand(t *testing.T, configPath string, args ...string) (string, error) {
	t.Helper()
	oldConfigPath := cfgFile
	t.Cleanup(func() { cfgFile = oldConfigPath })

	root := NewRootCmd()
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs(append([]string{"--config", configPath, "plugin"}, args...))
	err := root.Execute()
	return output.String(), err
}
