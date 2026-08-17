package pluginmanifest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParse_ValidManifestAcceptsScalarAndExpandedServiceRefs(t *testing.T) {
	manifest, err := Parse([]byte(`
name: antifraud
kind: antifraud
version: 1.4.0
api_version: "1"
type: internal
build_tag: plugin_antifraud
publishes:
  - antifraud_provider
requires:
  - name: subscription_repository
  - name: engine.softban
    optional: true
`))
	require.NoError(t, err)
	require.Equal(t, "antifraud", manifest.Name)
	require.Len(t, manifest.Requires, 2)
	require.True(t, manifest.Requires[1].Optional)
	require.Equal(t, "antifraud_provider", manifest.Metadata().Publishes[0].Name)
}

func TestParse_AcceptsAdminToolKind(t *testing.T) {
	manifest, err := Parse([]byte(`
name: server_routing
kind: admin_tool
version: 1.0.0
api_version: "1"
description: "Admin traffic routing management."
type: internal
mandatory: false
`))
	require.NoError(t, err)
	require.Equal(t, "admin_tool", manifest.Kind)
}

func TestBuiltinManifestFilesAreValid(t *testing.T) {
	paths := []string{
		filepath.Join("..", "plugins", "core", "plugin.yaml"),
		filepath.Join("..", "plugins", "billing", "plugin.yaml"),
		filepath.Join("..", "plugins", "antifraud", "plugin.yaml"),
		filepath.Join("..", "plugins", "engine_xray", "plugin.yaml"),
		filepath.Join("..", "plugins", "engine_singbox", "plugin.yaml"),
		filepath.Join("..", "plugins", "eventsink_webhook", "plugin.yaml"),
		filepath.Join("..", "plugins", "mailer_resend", "plugin.yaml"),
		filepath.Join("..", "plugins", "pricing_default", "plugin.yaml"),
		filepath.Join("..", "plugins", "promo", "plugin.yaml"),
		filepath.Join("..", "plugins", "referral", "plugin.yaml"),
		filepath.Join("..", "plugins", "payment_platega", "plugin.yaml"),
		filepath.Join("..", "plugins", "cluster_replication", "plugin.yaml"),
		filepath.Join("..", "plugins", "api_server", "plugin.yaml"),
		filepath.Join("..", "plugins", "config_storage", "plugin.yaml"),
		filepath.Join("..", "plugins", "identity_memory", "plugin.yaml"),
		filepath.Join("..", "plugins", "subscription_autobalancer", "plugin.yaml"),
		filepath.Join("..", "plugins", "subscription_format_legacy", "plugin.yaml"),
		filepath.Join("..", "plugins", "subscription_lifecycle", "plugin.yaml"),
		filepath.Join("..", "plugins", "subscription_runtime", "plugin.yaml"),
		filepath.Join("..", "plugins", "traffic_file", "plugin.yaml"),
		filepath.Join("..", "plugins", "user_management", "plugin.yaml"),
		filepath.Join("..", "plugins", "support_chat", "plugin.yaml"),
	}
	for _, path := range paths {
		path := path
		t.Run(path, func(t *testing.T) {
			manifest, err := Load(path)
			require.NoError(t, err)
			require.NotEmpty(t, manifest.ConfigSchema)
			require.NotEmpty(t, manifest.Description)
		})
	}
}

func TestBuiltinManifestFilesWithoutConfigSchemaAreValid(t *testing.T) {
	paths := []string{
		filepath.Join("..", "plugins", "server_routing", "plugin.yaml"),
	}
	for _, path := range paths {
		path := path
		t.Run(path, func(t *testing.T) {
			manifest, err := Load(path)
			require.NoError(t, err)
			require.Empty(t, manifest.ConfigSchema)
			require.NotEmpty(t, manifest.Description)
		})
	}
}

func TestValidateBuiltinConfig(t *testing.T) {
	require.NoError(t, ValidateBuiltinConfig("payment_platega", map[string]any{
		"merchant_id": "merchant",
		"secret":      "secret",
		"currency":    "RUB",
	}))

	err := ValidateBuiltinConfig("antifraud", map[string]any{"max_ips": "seven"})
	require.Error(t, err)
	require.ErrorContains(t, err, "$.max_ips: expected integer")

	err = ValidateBuiltinConfig("pricing_default", map[string]any{"typo": true})
	require.Error(t, err)
	require.ErrorContains(t, err, "additional property is not allowed")
}

func TestValidateConfig_ResolvesSchemaRelativeToManifest(t *testing.T) {
	bundle := t.TempDir()
	schemaDir := filepath.Join(bundle, "schemas")
	require.NoError(t, os.MkdirAll(schemaDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(schemaDir, "config.schema.json"), []byte(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["api_key"],
  "properties": {"api_key": {"type": "string", "minLength": 1}}
}`), 0600))
	manifestPath := filepath.Join(bundle, "plugin.yaml")
	require.NoError(t, os.WriteFile(manifestPath, []byte(`
name: payment_example
kind: payment
version: 1.0.0
api_version: "1"
type: external
config_schema: ./schemas/config.schema.json
`), 0600))

	require.NoError(t, ValidateConfig(manifestPath, map[string]any{"api_key": "test-key"}))
	err := ValidateConfig(manifestPath, map[string]any{"api_key": 17})
	require.Error(t, err)
	require.ErrorContains(t, err, "$.api_key: expected string")
}

func TestValidateConfig_RejectsRemoteSchemaReferences(t *testing.T) {
	bundle := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bundle, "config.schema.json"), []byte(`{
  "$ref": "https://untrusted.example/schema.json"
}`), 0600))
	manifestPath := filepath.Join(bundle, "plugin.yaml")
	require.NoError(t, os.WriteFile(manifestPath, []byte(`
name: notification_example
kind: notification
version: 1.0.0
api_version: "1"
type: external
config_schema: ./config.schema.json
`), 0600))

	err := ValidateConfig(manifestPath, map[string]any{})
	require.Error(t, err)
	require.ErrorContains(t, err, "external JSON Schema reference")
}

func TestConfigSchemaPath_RejectsExternalBundleEscape(t *testing.T) {
	bundle := t.TempDir()
	manifestPath := filepath.Join(bundle, "plugin.yaml")
	manifest := Manifest{Type: "external", ConfigSchema: "../outside.schema.json"}
	_, err := ConfigSchemaPath(manifestPath, manifest)
	require.Error(t, err)
	require.ErrorContains(t, err, "escapes plugin bundle")
}

func TestValidateManifestSchema_RejectsBrokenLocalReference(t *testing.T) {
	bundle := t.TempDir()
	schemaPath := filepath.Join(bundle, "config.schema.json")
	require.NoError(t, os.WriteFile(schemaPath, []byte(`{"$ref":"#/definitions/missing"}`), 0o600))
	manifestPath := filepath.Join(bundle, "plugin.yaml")
	manifest := Manifest{Type: "external", ConfigSchema: "config.schema.json"}
	err := ValidateManifestSchema(manifestPath, manifest)
	require.Error(t, err)
	require.ErrorContains(t, err, "does not exist")
}

func TestParse_RejectsUnsupportedAPIAndInvalidCore(t *testing.T) {
	_, err := Parse([]byte(`
name: core
kind: core
version: 1.0.0
api_version: "999"
type: external
mandatory: false
`))
	require.Error(t, err)
	require.ErrorContains(t, err, "newer than host-supported")
}

func TestParse_RejectsDuplicateAndOptionalPublications(t *testing.T) {
	_, err := Parse([]byte(`
name: example
kind: notification
version: 1.0.0
api_version: "1"
type: external
publishes:
  - name: notification_provider
    optional: true
  - notification_provider
`))
	require.Error(t, err)
	require.ErrorContains(t, err, "cannot be optional")
}
