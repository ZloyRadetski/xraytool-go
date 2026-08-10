// Package pluginmanifest parses and validates the declarative plugin manifest
// used by `xraytool plugin verify`. Keeping this contract outside cmd makes it
// reusable by a future external-plugin loader without coupling it to Cobra.
package pluginmanifest

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"xraytool/internal/pluginapi"
)

// Manifest describes a plugin before it is loaded. It deliberately contains
// only static information: runtime settings stay in the application's
// plugins: configuration section.
type Manifest struct {
	Name         string       `yaml:"name"`
	Kind         string       `yaml:"kind"`
	Version      string       `yaml:"version"`
	APIVersion   string       `yaml:"api_version"`
	Description  string       `yaml:"description"`
	Type         string       `yaml:"type"`
	BuildTag     string       `yaml:"build_tag"`
	ConfigSchema string       `yaml:"config_schema"`
	Mandatory    bool         `yaml:"mandatory"`
	Publishes    []ServiceRef `yaml:"publishes"`
	Requires     []ServiceRef `yaml:"requires"`
}

// ServiceRef is the manifest representation of a service declaration. YAML
// accepts both the concise scalar form (`- user_repository`) and the expanded
// form (`- name: user_repository; optional: true`).
type ServiceRef struct {
	Name     string `yaml:"name"`
	Optional bool   `yaml:"optional"`
}

// UnmarshalYAML accepts either a scalar service name or the expanded mapping.
func (r *ServiceRef) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		r.Name = value.Value
		r.Optional = false
		return nil
	case yaml.MappingNode:
		var raw struct {
			Name     string `yaml:"name"`
			Optional bool   `yaml:"optional"`
		}
		if err := value.Decode(&raw); err != nil {
			return err
		}
		r.Name = raw.Name
		r.Optional = raw.Optional
		return nil
	default:
		return fmt.Errorf("service reference must be a string or mapping, got YAML kind %d", value.Kind)
	}
}

// Metadata converts a validated manifest to the static descriptor used by the
// dependency-graph implementation. It does not start or initialise a plugin.
func (m Manifest) Metadata() pluginapi.Metadata {
	publishes := make([]pluginapi.ServiceRef, len(m.Publishes))
	for i, ref := range m.Publishes {
		publishes[i] = pluginapi.ServiceRef{Name: ref.Name, Optional: ref.Optional}
	}
	requires := make([]pluginapi.ServiceRef, len(m.Requires))
	for i, ref := range m.Requires {
		requires[i] = pluginapi.ServiceRef{Name: ref.Name, Optional: ref.Optional}
	}
	return pluginapi.Metadata{
		Name:        m.Name,
		Kind:        m.Kind,
		Version:     m.Version,
		APIVersion:  m.APIVersion,
		Description: m.Description,
		Mandatory:   m.Mandatory,
		Publishes:   publishes,
		Requires:    requires,
	}
}

// Load reads, parses and validates a manifest from path.
func Load(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read plugin manifest %q: %w", path, err)
	}
	manifest, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("parse plugin manifest %q: %w", path, err)
	}
	if err := ValidateManifestSchema(path, *manifest); err != nil {
		return nil, fmt.Errorf("validate plugin manifest schema %q: %w", path, err)
	}
	return manifest, nil
}

// Parse parses and validates a manifest document.
func Parse(data []byte) (*Manifest, error) {
	var manifest Manifest
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("decode YAML: %w", err)
	}
	if err := manifest.Validate(); err != nil {
		return nil, err
	}
	return &manifest, nil
}

var (
	pluginNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	semverPattern     = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)
)

var validKinds = map[string]struct{}{
	"core": {}, "engine": {}, "antifraud": {}, "payment": {}, "pricing": {},
	"notification": {}, "event_sink": {}, "cluster_replication": {}, "traffic": {}, "lifecycle": {}, "storage": {}, "identity": {}, "subscription": {}, "subscription_format": {}, "user_management": {}, "api": {}, "support": {},
}

// Validate checks the static contract a host can know before loading code.
func (m Manifest) Validate() error {
	if !pluginNamePattern.MatchString(m.Name) {
		return fmt.Errorf("manifest.name must be a lowercase plugin identifier, got %q", m.Name)
	}
	if _, ok := validKinds[m.Kind]; !ok {
		return fmt.Errorf("manifest.kind %q is unsupported", m.Kind)
	}
	if !semverPattern.MatchString(m.Version) {
		return fmt.Errorf("manifest.version must be a semantic version, got %q", m.Version)
	}
	if err := validateAPIVersion(m.APIVersion); err != nil {
		return err
	}
	switch m.Type {
	case "internal", "external":
	default:
		return fmt.Errorf("manifest.type must be internal or external, got %q", m.Type)
	}
	if m.Type == "external" && m.BuildTag != "" {
		return fmt.Errorf("manifest.build_tag is only valid for internal plugins")
	}
	if m.ConfigSchema != "" && strings.TrimSpace(m.ConfigSchema) == "" {
		return fmt.Errorf("manifest.config_schema must not be blank")
	}
	if m.Mandatory && m.Name != "core" {
		return fmt.Errorf("manifest %q is mandatory, but only core may be mandatory", m.Name)
	}
	if m.Name == "core" {
		if m.Kind != "core" {
			return fmt.Errorf("core manifest.kind must be core, got %q", m.Kind)
		}
		if m.Type != "internal" {
			return fmt.Errorf("core manifest.type must be internal")
		}
		if !m.Mandatory {
			return fmt.Errorf("core manifest must set mandatory: true")
		}
	}
	if err := validateServiceRefs("publishes", m.Publishes, false); err != nil {
		return err
	}
	if err := validateServiceRefs("requires", m.Requires, true); err != nil {
		return err
	}
	return nil
}

func validateAPIVersion(version string) error {
	majorText, _, _ := strings.Cut(strings.TrimSpace(version), ".")
	major, err := strconv.Atoi(majorText)
	if err != nil || major < 1 {
		return fmt.Errorf("manifest.api_version must be a positive integer major version, got %q", version)
	}
	supported, err := strconv.Atoi(pluginapi.CurrentAPIVersion)
	if err != nil {
		return fmt.Errorf("host has invalid supported plugin API version %q", pluginapi.CurrentAPIVersion)
	}
	if major > supported {
		return fmt.Errorf("manifest.api_version %q is newer than host-supported API version %q", version, pluginapi.CurrentAPIVersion)
	}
	return nil
}

func validateServiceRefs(section string, refs []ServiceRef, allowOptional bool) error {
	seen := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		name := strings.TrimSpace(ref.Name)
		if name == "" {
			return fmt.Errorf("manifest.%s contains an empty service name", section)
		}
		if _, exists := seen[name]; exists {
			return fmt.Errorf("manifest.%s declares service %q more than once", section, name)
		}
		if ref.Optional && !allowOptional {
			return fmt.Errorf("manifest.%s service %q cannot be optional", section, name)
		}
		seen[name] = struct{}{}
	}
	return nil
}
