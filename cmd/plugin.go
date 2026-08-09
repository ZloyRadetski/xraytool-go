package cmd

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"xraytool/internal/appconfig"
	"xraytool/internal/pluginapi"
	"xraytool/internal/pluginhost"
	"xraytool/internal/pluginmanifest"
)

// newPluginCmd owns commands which inspect or edit only the declarative plugin
// configuration. Its PersistentPreRunE intentionally shadows the root command
// hook: listing a manifest or changing enabled:false must not open the database
// or construct a VPN engine first.
func newPluginCmd() *cobra.Command {
	return newPluginCmdWithConfigPath(func() string { return cfgFile })
}

func newPluginCmdWithConfigPath(configPath func() string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "plugin",
		Short: "Inspect, validate and configure plugins",
		Long: `Plugin commands operate on the plugins: and engines: sections without
starting the server or opening its database. Changes take effect after the next
server restart.`,
		PersistentPreRunE: func(*cobra.Command, []string) error {
			return nil
		},
	}

	cmd.AddCommand(
		newPluginListCmd(configPath),
		newPluginGraphCmd(configPath),
		newPluginToggleCmd(configPath, true),
		newPluginToggleCmd(configPath, false),
		newPluginVerifyCmd(),
		newPluginLogsCmd(configPath),
		newPluginCommandsCmd(configPath),
		newPluginRunCmd(configPath),
	)
	return cmd
}

func newPluginCommandsCmd(configPath func() string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "commands [plugin]",
		Short: "List self-contained commands contributed by built-in plugins",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := appconfig.Load(configPath())
			if err != nil {
				return err
			}
			factories := pluginhost.BuiltinRegistry(cfg)
			filter := ""
			if len(args) == 1 {
				filter = args[0]
			}
			var rows []string
			for name, factory := range factories {
				if filter != "" && name != filter {
					continue
				}
				contributor, ok := factory().(pluginapi.CLIContributor)
				if !ok {
					continue
				}
				for _, contribution := range contributor.CLICommands() {
					rows = append(rows, fmt.Sprintf("%s %s\t%s", name, contribution.Use, contribution.Short))
				}
			}
			sort.Strings(rows)
			for _, row := range rows {
				if _, err := fmt.Fprintln(cmd.OutOrStdout(), row); err != nil {
					return err
				}
			}
			return nil
		},
	}
	return cmd
}

func newPluginRunCmd(configPath func() string) *cobra.Command {
	return &cobra.Command{
		Use:   "run <plugin> <command> [arguments...]",
		Short: "Run a self-contained built-in plugin command",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := appconfig.Load(configPath())
			if err != nil {
				return err
			}
			factory, ok := pluginhost.BuiltinRegistry(cfg)[args[0]]
			if !ok {
				return fmt.Errorf("unknown built-in plugin %q", args[0])
			}
			contributor, ok := factory().(pluginapi.CLIContributor)
			if !ok {
				return fmt.Errorf("plugin %q does not contribute self-contained CLI commands", args[0])
			}
			result, err := contributor.RunCLI(cmd.Context(), args[1], args[2:])
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), result)
			return err
		},
	}
}

func newPluginListCmd(configPath func() string) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List compiled-in and configured plugins",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return writePluginList(cmd.OutOrStdout(), configPath())
		},
	}
}

func newPluginGraphCmd(configPath func() string) *cobra.Command {
	return &cobra.Command{
		Use:   "graph",
		Short: "Print the dependency graph without starting plugins",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return writePluginGraph(cmd.OutOrStdout(), configPath())
		},
	}
}

func newPluginToggleCmd(configPath func() string, enabled bool) *cobra.Command {
	verb := "disable"
	short := "Disable a plugin in the configuration"
	if enabled {
		verb = "enable"
		short = "Enable a plugin in the configuration"
	}

	return &cobra.Command{
		Use:   verb + " <name>",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := setPluginEnabled(configPath(), args[0], enabled)
			if err != nil {
				return err
			}
			state := "disabled"
			if enabled {
				state = "enabled"
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Plugin %q %s in %s. Restart xraytool for the change to take effect.\n", name, state, configPath())
			return err
		},
	}
}

func newPluginVerifyCmd() *cobra.Command {
	var executable string
	var executableArgs []string
	cmd := &cobra.Command{
		Use:   "verify <manifest-path>",
		Short: "Validate a plugin manifest and optionally an external ABI handshake",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			manifest, err := pluginmanifest.Load(args[0])
			if err != nil {
				return err
			}
			if strings.TrimSpace(executable) != "" {
				if manifest.Type != "external" {
					return fmt.Errorf("--exec is only valid for an external plugin manifest")
				}
				runtimeMeta, err := pluginhost.VerifyExternalPlugin(cmd.Context(), manifest.Name, executable, executableArgs)
				if err != nil {
					return fmt.Errorf("external plugin handshake: %w", err)
				}
				if err := verifyRuntimeMetadata(*manifest, runtimeMeta); err != nil {
					return err
				}
				_, err = fmt.Fprintf(cmd.OutOrStdout(),
					"Manifest %q and external handshake are valid: %s %s v%s (plugin API %s).\n",
					args[0], manifest.Name, manifest.Kind, manifest.Version, manifest.APIVersion,
				)
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(),
				"Manifest %q is valid: %s %s v%s (plugin API %s).\n",
				args[0], manifest.Name, manifest.Kind, manifest.Version, manifest.APIVersion,
			)
			return err
		},
	}
	cmd.Flags().StringVar(&executable, "exec", "", "External plugin executable to start for handshake verification")
	cmd.Flags().StringArrayVar(&executableArgs, "arg", nil, "Argument passed to --exec (repeatable)")
	return cmd
}

func verifyRuntimeMetadata(manifest pluginmanifest.Manifest, runtime pluginapi.Metadata) error {
	if runtime.Name != manifest.Name || runtime.Kind != manifest.Kind || runtime.Version != manifest.Version || runtime.APIVersion != manifest.APIVersion {
		return fmt.Errorf("external plugin metadata does not match manifest: got name=%q kind=%q version=%q api_version=%q", runtime.Name, runtime.Kind, runtime.Version, runtime.APIVersion)
	}
	if runtime.Mandatory != manifest.Mandatory {
		return fmt.Errorf("external plugin metadata mandatory=%t does not match manifest mandatory=%t", runtime.Mandatory, manifest.Mandatory)
	}
	if !sameServiceRefs(runtime.Publishes, manifest.Publishes) || !sameServiceRefs(runtime.Requires, manifest.Requires) {
		return fmt.Errorf("external plugin metadata publishes/requires do not match manifest")
	}
	return nil
}

func sameServiceRefs(runtime []pluginapi.ServiceRef, manifest []pluginmanifest.ServiceRef) bool {
	if len(runtime) != len(manifest) {
		return false
	}
	runtimeByName := make(map[string]bool, len(runtime))
	for _, ref := range runtime {
		runtimeByName[ref.Name] = ref.Optional
	}
	for _, ref := range manifest {
		optional, found := runtimeByName[ref.Name]
		if !found || optional != ref.Optional {
			return false
		}
	}
	return true
}

func newPluginLogsCmd(configPath func() string) *cobra.Command {
	var follow bool
	var tail int
	cmd := &cobra.Command{
		Use:   "logs <name>",
		Short: "Show logs from an external plugin",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			doc, err := readPluginConfigDocument(configPath())
			if err != nil {
				return err
			}
			configured, err := configuredPlugins(doc.root)
			if err != nil {
				return err
			}
			entry, ok := configured[args[0]]
			if !ok {
				return fmt.Errorf("plugin %q is not configured", args[0])
			}
			if !isExternalPluginSource(entry.Source) {
				return fmt.Errorf("plugin %q is not external and has no subprocess log stream", args[0])
			}
			path := strings.TrimSpace(entry.LogPath)
			if path == "" {
				path, err = pluginhost.ExternalLogPath(entry.Name)
				if err != nil {
					return err
				}
			}
			return writeExternalPluginLogs(cmd.Context(), cmd.OutOrStdout(), path, tail, follow)
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "Follow log output")
	cmd.Flags().IntVarP(&tail, "tail", "n", 200, "Number of existing log lines to show (0 = all)")
	return cmd
}

func writeExternalPluginLogs(ctx context.Context, out io.Writer, path string, maxLines int, follow bool) error {
	offset, err := writeExternalPluginLogTail(out, path, maxLines)
	if err != nil {
		return err
	}
	if !follow {
		return nil
	}

	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			info, err := os.Stat(path)
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return fmt.Errorf("stat external plugin log %q: %w", path, err)
			}
			if info.Size() < offset {
				// The bounded writer rotated the file. Resume from its new start.
				offset = 0
			}
			if info.Size() == offset {
				continue
			}
			file, err := os.Open(path)
			if err != nil {
				return fmt.Errorf("open external plugin log %q: %w", path, err)
			}
			_, seekErr := file.Seek(offset, io.SeekStart)
			if seekErr == nil {
				_, seekErr = io.Copy(out, file)
			}
			_ = file.Close()
			if seekErr != nil {
				return fmt.Errorf("read external plugin log %q: %w", path, seekErr)
			}
			offset = info.Size()
		}
	}
}

func writeExternalPluginLogTail(out io.Writer, path string, maxLines int) (int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, fmt.Errorf("external plugin log %q does not exist yet; start the server or configure plugins.<name>.log_path", path)
		}
		return 0, fmt.Errorf("read external plugin log %q: %w", path, err)
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if maxLines > 0 && len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	for _, line := range lines {
		if _, err := fmt.Fprintln(out, line); err != nil {
			return 0, err
		}
	}
	return int64(len(data)), nil
}

// pluginConfigDocument keeps the original yaml.Node tree alive while changing
// one scalar. Unlike unmarshalling into Config and marshalling it again, this
// preserves unknown top-level sections, extension fields, comments and order.
type pluginConfigDocument struct {
	path string
	doc  yaml.Node
	root *yaml.Node
	mode os.FileMode
}

func readPluginConfigDocument(path string) (*pluginConfigDocument, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("plugin configuration path must not be empty")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read configuration %q: %w", path, err)
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse configuration %q: %w", path, err)
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("configuration %q must contain a YAML mapping at its root", path)
	}

	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat configuration %q: %w", path, err)
	}
	return &pluginConfigDocument{path: path, doc: doc, root: doc.Content[0], mode: info.Mode().Perm()}, nil
}

func (d *pluginConfigDocument) save() error {
	data, err := yaml.Marshal(&d.doc)
	if err != nil {
		return fmt.Errorf("encode configuration %q: %w", d.path, err)
	}
	if !bytes.HasSuffix(data, []byte("\n")) {
		data = append(data, '\n')
	}

	dir := filepath.Dir(d.path)
	file, err := os.CreateTemp(dir, ".xraytool-plugin-config-*")
	if err != nil {
		return fmt.Errorf("create temporary configuration next to %q: %w", d.path, err)
	}
	temporaryPath := file.Name()
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()

	mode := d.mode
	if mode == 0 {
		mode = 0600
	}
	if err := file.Chmod(mode); err != nil {
		_ = file.Close()
		return fmt.Errorf("set permissions on temporary configuration: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return fmt.Errorf("write temporary configuration: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync temporary configuration: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close temporary configuration: %w", err)
	}
	if err := os.Rename(temporaryPath, d.path); err != nil {
		return fmt.Errorf("replace configuration %q atomically: %w", d.path, err)
	}
	removeTemporary = false
	return nil
}

func mappingValue(mapping *yaml.Node, key string) *yaml.Node {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}

func ensureMappingValue(mapping *yaml.Node, key string) (*yaml.Node, error) {
	value := mappingValue(mapping, key)
	if value == nil {
		value = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		mapping.Content = append(mapping.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
			value,
		)
		return value, nil
	}
	if value.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("configuration section %q must be a YAML mapping", key)
	}
	return value, nil
}

func ensurePluginEntry(section *yaml.Node, key string) (*yaml.Node, error) {
	if section == nil || section.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("plugin configuration section must be a YAML mapping")
	}
	entry := mappingValue(section, key)
	if entry == nil {
		entry = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		section.Content = append(section.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
			entry,
		)
	}
	if entry.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("plugin %q configuration must be a YAML mapping", key)
	}
	return entry, nil
}

func setStringField(mapping *yaml.Node, key, value string) {
	field := mappingValue(mapping, key)
	if field == nil {
		mapping.Content = append(mapping.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value},
		)
		return
	}
	field.Kind = yaml.ScalarNode
	field.Tag = "!!str"
	field.Value = value
	field.Content = nil
}

func setBoolField(mapping *yaml.Node, key string, value bool) {
	field := mappingValue(mapping, key)
	if field == nil {
		mapping.Content = append(mapping.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: strconv.FormatBool(value)},
		)
		return
	}
	field.Kind = yaml.ScalarNode
	field.Tag = "!!bool"
	field.Value = strconv.FormatBool(value)
	field.Content = nil
}

func boolField(mapping *yaml.Node, key string) (bool, error) {
	field := mappingValue(mapping, key)
	if field == nil {
		return false, nil
	}
	var value bool
	if err := field.Decode(&value); err != nil {
		return false, fmt.Errorf("plugin field %q must be a boolean: %w", key, err)
	}
	return value, nil
}

func stringField(mapping *yaml.Node, key string) (string, error) {
	field := mappingValue(mapping, key)
	if field == nil {
		return "", nil
	}
	if field.Kind != yaml.ScalarNode {
		return "", fmt.Errorf("plugin field %q must be a string", key)
	}
	return field.Value, nil
}

type pluginLocation uint8

const (
	pluginsLocation pluginLocation = iota
	enginesLocation
)

type configuredPlugin struct {
	Name         string
	Enabled      bool
	Source       string
	Exec         string
	ManifestPath string
	LogPath      string
	Location     pluginLocation
	Entry        *yaml.Node
}

func configuredPlugins(root *yaml.Node) (map[string]configuredPlugin, error) {
	result := make(map[string]configuredPlugin)
	if err := collectPluginSection(result, mappingValue(root, "plugins"), pluginsLocation); err != nil {
		return nil, err
	}
	if err := collectPluginSection(result, mappingValue(root, "engines"), enginesLocation); err != nil {
		return nil, err
	}
	return result, nil
}

func collectPluginSection(result map[string]configuredPlugin, section *yaml.Node, location pluginLocation) error {
	if section == nil {
		return nil
	}
	if section.Kind != yaml.MappingNode {
		name := "plugins"
		if location == enginesLocation {
			name = "engines"
		}
		return fmt.Errorf("configuration section %q must be a YAML mapping", name)
	}
	for i := 0; i+1 < len(section.Content); i += 2 {
		name := section.Content[i].Value
		if location == enginesLocation && name == "routing_mode" {
			continue
		}
		if location == enginesLocation && !strings.HasPrefix(name, "engine_") {
			name = "engine_" + name
		}
		entry := section.Content[i+1]
		if entry.Kind != yaml.MappingNode {
			return fmt.Errorf("plugin %q configuration must be a YAML mapping", name)
		}
		if _, exists := result[name]; exists {
			return fmt.Errorf("plugin %q is configured more than once (plugins and engines cannot overlap)", name)
		}
		enabled, err := boolField(entry, "enabled")
		if err != nil {
			return fmt.Errorf("plugin %q: %w", name, err)
		}
		source, err := stringField(entry, "source")
		if err != nil {
			return fmt.Errorf("plugin %q: %w", name, err)
		}
		execPath, err := stringField(entry, "exec")
		if err != nil {
			return fmt.Errorf("plugin %q: %w", name, err)
		}
		manifestPath, err := stringField(entry, "manifest")
		if err != nil {
			return fmt.Errorf("plugin %q: %w", name, err)
		}
		logPath, err := stringField(entry, "log_path")
		if err != nil {
			return fmt.Errorf("plugin %q: %w", name, err)
		}
		result[name] = configuredPlugin{
			Name: name, Enabled: enabled, Source: source, Exec: execPath,
			ManifestPath: manifestPath, LogPath: logPath, Location: location, Entry: entry,
		}
	}
	return nil
}

func builtinPluginMetadata() (map[string]pluginapi.Metadata, error) {
	registry := pluginhost.BuiltinRegistry(nil)
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	sort.Strings(names)

	metadata := make(map[string]pluginapi.Metadata, len(names))
	for _, name := range names {
		factory := registry[name]
		if factory == nil {
			return nil, fmt.Errorf("builtin plugin registry has nil factory for %q", name)
		}
		plugin := factory()
		if plugin == nil {
			return nil, fmt.Errorf("builtin plugin registry factory %q returned nil", name)
		}
		meta := plugin.Metadata()
		if meta.Name != name {
			return nil, fmt.Errorf("builtin plugin registry key %q does not match metadata name %q", name, meta.Name)
		}
		metadata[name] = meta
	}
	return metadata, nil
}

func writePluginList(out io.Writer, path string) error {
	doc, err := readPluginConfigDocument(path)
	if err != nil {
		return err
	}
	configured, err := configuredPlugins(doc.root)
	if err != nil {
		return err
	}
	builtins, err := builtinPluginMetadata()
	if err != nil {
		return err
	}

	names := make(map[string]struct{}, len(configured)+len(builtins))
	for name := range configured {
		names[name] = struct{}{}
	}
	for name := range builtins {
		names[name] = struct{}{}
	}
	ordered := make([]string, 0, len(names))
	for name := range names {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)

	table := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(table, "NAME\tKIND\tSOURCE\tENABLED\tSTATUS\tDESCRIPTION")
	for _, name := range ordered {
		meta, installed := builtins[name]
		entry, configured := configured[name]

		kind := "unknown"
		description := "configured plugin (manifest required for offline metadata)"
		if installed {
			kind = meta.Kind
			description = meta.Description
		}
		source := "builtin"
		enabled := false
		status := "available"
		if configured {
			source = entry.Source
			enabled = entry.Enabled
			if enabled {
				status = "enabled"
			} else {
				status = "disabled"
			}
			if isExternalPluginSource(entry.Source) {
				if externalMeta, err := loadExternalManifest(path, entry); err != nil {
					status = "invalid: " + err.Error()
				} else {
					kind = externalMeta.Kind
					description = externalMeta.Description
				}
			}
		}
		if name == "core" {
			if configured && !entry.Enabled {
				status = "invalid: mandatory disabled"
			} else {
				status = "mandatory"
				enabled = true
			}
		}
		_, _ = fmt.Fprintf(table, "%s\t%s\t%s\t%t\t%s\t%s\n", name, kind, source, enabled, status, description)
	}
	return table.Flush()
}

func writePluginGraph(out io.Writer, path string) error {
	doc, err := readPluginConfigDocument(path)
	if err != nil {
		return err
	}
	configured, err := configuredPlugins(doc.root)
	if err != nil {
		return err
	}
	builtins, err := builtinPluginMetadata()
	if err != nil {
		return err
	}

	// Config files created before the plugins: section existed are still valid at
	// runtime: appconfig materialises the mandatory core and the xray engine.
	// Mirror those two invariant defaults here so graph is useful during a
	// migration as well as for newly written configuration.
	if _, exists := configured["core"]; !exists {
		configured["core"] = configuredPlugin{Name: "core", Enabled: true, Source: "builtin"}
	}
	hasEngine := false
	for name := range configured {
		if strings.HasPrefix(name, "engine_") {
			hasEngine = true
			break
		}
	}
	if !hasEngine {
		configured["engine_xray"] = configuredPlugin{Name: "engine_xray", Enabled: true, Source: "builtin", Location: enginesLocation}
	}

	metas := make([]pluginapi.Metadata, 0, len(configured))
	enabled := make(map[string]bool, len(configured))
	for name, entry := range configured {
		if !entry.Enabled {
			if name == "core" {
				return fmt.Errorf("plugin %q is mandatory and cannot be disabled", name)
			}
			continue
		}
		meta, err := configuredPluginMetadata(path, entry, builtins)
		if err != nil {
			return err
		}
		metas = append(metas, meta)
		enabled[name] = true
	}

	order, err := pluginhost.Graph(metas, enabled)
	if err != nil {
		return fmt.Errorf("plugin dependency graph: %w", err)
	}
	metaByName := make(map[string]pluginapi.Metadata, len(metas))
	for _, meta := range metas {
		metaByName[meta.Name] = meta
	}

	_, _ = fmt.Fprintln(out, "Plugin dependency graph (load order):")
	for index, name := range order {
		meta := metaByName[name]
		_, _ = fmt.Fprintf(out, "%d. %s (%s)\n", index+1, meta.Name, meta.Kind)
		if len(meta.Publishes) > 0 {
			_, _ = fmt.Fprintf(out, "   publishes: %s\n", formatServiceRefs(meta.Publishes))
		}
		if len(meta.Requires) > 0 {
			_, _ = fmt.Fprintf(out, "   requires:  %s\n", formatServiceRefs(meta.Requires))
		}
	}
	return nil
}

func configuredPluginMetadata(configPath string, entry configuredPlugin, builtins map[string]pluginapi.Metadata) (pluginapi.Metadata, error) {
	if entry.Source == "builtin" {
		meta, ok := builtins[entry.Name]
		if !ok {
			return pluginapi.Metadata{}, fmt.Errorf("enabled builtin plugin %q is not compiled into this binary", entry.Name)
		}
		return meta, nil
	}
	if isExternalPluginSource(entry.Source) {
		return loadExternalManifest(configPath, entry)
	}
	return pluginapi.Metadata{}, fmt.Errorf("cannot build graph for enabled plugin %q with unsupported source %q", entry.Name, entry.Source)
}

func isExternalPluginSource(source string) bool {
	source = strings.TrimSpace(source)
	return source == "external" || strings.HasPrefix(source, "external:")
}

// loadExternalManifest provides offline metadata for `plugin list` and
// `plugin graph`. It deliberately does not start the plugin process: external
// executables must ship a checked-in plugin.yaml next to the binary, or the
// configuration may point to it explicitly with manifest:.
func loadExternalManifest(configPath string, entry configuredPlugin) (pluginapi.Metadata, error) {
	manifestPath, err := externalManifestPath(configPath, entry)
	if err != nil {
		return pluginapi.Metadata{}, err
	}
	manifest, err := pluginmanifest.Load(manifestPath)
	if err != nil {
		return pluginapi.Metadata{}, fmt.Errorf("load external manifest %q: %w", manifestPath, err)
	}
	if manifest.Type != "external" {
		return pluginapi.Metadata{}, fmt.Errorf("external plugin %q manifest %q has type %q, want external", entry.Name, manifestPath, manifest.Type)
	}
	if manifest.Name != entry.Name {
		return pluginapi.Metadata{}, fmt.Errorf("external manifest %q declares name %q, configured as %q", manifestPath, manifest.Name, entry.Name)
	}
	return manifest.Metadata(), nil
}

func externalManifestPath(configPath string, entry configuredPlugin) (string, error) {
	path := strings.TrimSpace(entry.ManifestPath)
	if path == "" {
		execPath := strings.TrimSpace(entry.Exec)
		if execPath == "" {
			execPath = strings.TrimSpace(strings.TrimPrefix(entry.Source, "external:"))
		}
		if execPath == "" {
			return "", fmt.Errorf("external plugin %q requires exec or manifest for offline metadata", entry.Name)
		}
		path = filepath.Join(filepath.Dir(execPath), "plugin.yaml")
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	return filepath.Join(filepath.Dir(configPath), path), nil
}

func formatServiceRefs(refs []pluginapi.ServiceRef) string {
	parts := make([]string, 0, len(refs))
	for _, ref := range refs {
		name := ref.Name
		if ref.Optional {
			name += " (optional)"
		}
		parts = append(parts, name)
	}
	return strings.Join(parts, ", ")
}

func setPluginEnabled(path, rawName string, enabled bool) (string, error) {
	name := strings.TrimSpace(rawName)
	if !pluginmanifestNameValid(name) {
		return "", fmt.Errorf("invalid plugin name %q: use lowercase letters, digits, underscores or hyphens", rawName)
	}
	if name == "core" && !enabled {
		return "", fmt.Errorf("plugin %q is mandatory and cannot be disabled", name)
	}

	doc, err := readPluginConfigDocument(path)
	if err != nil {
		return "", err
	}
	configured, err := configuredPlugins(doc.root)
	if err != nil {
		return "", err
	}
	builtins, err := builtinPluginMetadata()
	if err != nil {
		return "", err
	}

	entry, found := configured[name]
	if !found {
		if !enabled {
			return "", fmt.Errorf("plugin %q is not configured", name)
		}
		meta, knownBuiltin := builtins[name]
		if !knownBuiltin {
			return "", fmt.Errorf("plugin %q is not configured and is not a compiled-in plugin; add its source and configuration manually first", name)
		}
		sectionName := "plugins"
		entryKey := name
		location := pluginsLocation
		if meta.Kind == "engine" {
			sectionName = "engines"
			entryKey = strings.TrimPrefix(name, "engine_")
			location = enginesLocation
		}
		section, err := ensureMappingValue(doc.root, sectionName)
		if err != nil {
			return "", err
		}
		entryNode, err := ensurePluginEntry(section, entryKey)
		if err != nil {
			return "", err
		}
		entry = configuredPlugin{Name: name, Source: "builtin", Location: location, Entry: entryNode}
		setStringField(entryNode, "source", "builtin")
	}

	if enabled {
		source := strings.TrimSpace(entry.Source)
		if source == "" {
			if _, knownBuiltin := builtins[name]; !knownBuiltin {
				return "", fmt.Errorf("plugin %q has no source; set source explicitly before enabling it", name)
			}
			setStringField(entry.Entry, "source", "builtin")
		} else if source == "builtin" {
			if _, knownBuiltin := builtins[name]; !knownBuiltin {
				return "", fmt.Errorf("plugin %q uses source builtin but is not compiled into this binary", name)
			}
		}
	}
	setBoolField(entry.Entry, "enabled", enabled)
	if err := doc.save(); err != nil {
		return "", err
	}
	return name, nil
}

func pluginmanifestNameValid(name string) bool {
	if name == "" {
		return false
	}
	for index, char := range name {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '_' || char == '-' {
			if index == 0 && (char == '_' || char == '-') {
				return false
			}
			continue
		}
		return false
	}
	return true
}
