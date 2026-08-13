package pluginmanifest

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	// Keep manifest-provided schemas and config serialisation bounded. A schema
	// is operator-controlled input, and schema validation happens before the
	// host starts any plugin process.
	maxConfigSchemaBytes = 1 << 20 // 1 MiB
	maxConfigJSONBytes   = 4 << 20 // 4 MiB
	maxSchemaDepth       = 64
)

// builtinConfigSchemaFiles contains the schemas for compiled-in plugins.
// They are embedded so config validation works from a released binary, where
// the source-tree paths used by plugin.yaml are not available.
//
//go:embed schemas/*.schema.json
var builtinConfigSchemaFiles embed.FS

var builtinConfigSchemas = map[string]string{
	"core":                "schemas/core.schema.json",
	"pricing_default":     "schemas/pricing_default.schema.json",
	"payment_platega":     "schemas/payment_platega.schema.json",
	"antifraud":           "schemas/antifraud.schema.json",
	"mailer_resend":       "schemas/mailer_resend.schema.json",
	"eventsink_webhook":   "schemas/eventsink_webhook.schema.json",
	"cluster_replication": "schemas/cluster_replication.schema.json",
	"engine_xray":         "schemas/engine_xray.schema.json",
	"engine_singbox":      "schemas/engine_singbox.schema.json",
}

// HasBuiltinConfigSchema reports whether a compiled-in plugin has a schema
// known to this version of the host. Unknown plugin names intentionally return
// false: their source may be supplied by a future build or an external plugin.
func HasBuiltinConfigSchema(name string) bool {
	_, ok := builtinConfigSchemas[name]
	return ok
}

// ValidateBuiltinConfig validates a compiled-in plugin configuration against
// its embedded JSON Schema. It is safe to call from appconfig because it never
// reads a path supplied by a configuration file.
func ValidateBuiltinConfig(name string, config any) error {
	schemaName, ok := builtinConfigSchemas[name]
	if !ok {
		return nil
	}
	data, err := builtinConfigSchemaFiles.ReadFile(schemaName)
	if err != nil {
		return fmt.Errorf("read embedded schema for builtin plugin %q: %w", name, err)
	}
	if err := validateConfigAgainstSchema(data, "builtin:"+name, config); err != nil {
		return fmt.Errorf("builtin plugin %q: %w", name, err)
	}
	return nil
}

// ConfigSchemaPath resolves a manifest config_schema reference. Relative paths
// are always relative to the manifest file, never to the process working
// directory. URL/file URI references are deliberately unsupported: schema
// validation must not perform network I/O or follow a path outside the
// operator-provided local bundle implicitly.
func ConfigSchemaPath(manifestPath string, manifest Manifest) (string, error) {
	ref := strings.TrimSpace(manifest.ConfigSchema)
	if ref == "" {
		return "", nil
	}
	if strings.Contains(ref, "://") || strings.HasPrefix(strings.ToLower(ref), "file:") {
		return "", fmt.Errorf("manifest.config_schema must be a local file path, got %q", manifest.ConfigSchema)
	}
	if strings.TrimSpace(manifestPath) == "" {
		return "", fmt.Errorf("cannot resolve relative manifest.config_schema %q without a manifest path", manifest.ConfigSchema)
	}
	if filepath.IsAbs(ref) {
		return "", fmt.Errorf("manifest.config_schema must stay inside the plugin bundle, got absolute path %q", manifest.ConfigSchema)
	}

	manifestDir, err := filepath.Abs(filepath.Dir(manifestPath))
	if err != nil {
		return "", fmt.Errorf("resolve manifest directory: %w", err)
	}
	candidate := filepath.Clean(filepath.Join(manifestDir, ref))
	// Built-in manifests share the embedded schema directory with the host.
	// This is a fixed, repository-owned location rather than a plugin supplied
	// escape hatch; external bundles remain constrained to their own directory.
	pluginRoot := filepath.Dir(manifestDir)
	allowBuiltinSchemaDir := manifest.Type == "internal" && filepath.Base(pluginRoot) == "plugins" &&
		pathWithin(filepath.Clean(filepath.Join(filepath.Dir(pluginRoot), "pluginmanifest", "schemas")), candidate)
	if !pathWithin(manifestDir, candidate) && !allowBuiltinSchemaDir {
		return "", fmt.Errorf("manifest.config_schema %q escapes plugin bundle", manifest.ConfigSchema)
	}

	// A lexical Clean check alone is insufficient: a symlink inside the bundle
	// could point at an arbitrary host file. Resolve both paths before exposing
	// the schema to the parser.
	realManifestDir, err := filepath.EvalSymlinks(manifestDir)
	if err != nil {
		return "", fmt.Errorf("resolve plugin bundle directory: %w", err)
	}
	realCandidate, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", fmt.Errorf("resolve manifest.config_schema %q: %w", manifest.ConfigSchema, err)
	}
	realBuiltinSchemaDir := filepath.Clean(filepath.Join(filepath.Dir(filepath.Dir(realManifestDir)), "pluginmanifest", "schemas"))
	if !pathWithin(realManifestDir, realCandidate) && !(allowBuiltinSchemaDir && pathWithin(realBuiltinSchemaDir, realCandidate)) {
		return "", fmt.Errorf("manifest.config_schema %q escapes plugin bundle through a symlink", manifest.ConfigSchema)
	}
	return realCandidate, nil
}

// ValidateConfig validates config against the JSON Schema declared by the
// manifest at manifestPath. A manifest without config_schema is valid and is
// intentionally left unchecked for backwards compatibility with third-party
// plugins that have not adopted declarative schemas yet.
func ValidateConfig(manifestPath string, config any) error {
	manifest, err := Load(manifestPath)
	if err != nil {
		return err
	}
	return ValidateConfigForManifest(manifestPath, *manifest, config)
}

// ValidateConfigForManifest is the non-reloading variant of ValidateConfig.
// It is useful to callers that already loaded a manifest to inspect metadata.
func ValidateConfigForManifest(manifestPath string, manifest Manifest, config any) error {
	schemaPath, err := ConfigSchemaPath(manifestPath, manifest)
	if err != nil {
		return err
	}
	if schemaPath == "" {
		return nil
	}
	data, err := readBoundedFile(schemaPath, maxConfigSchemaBytes)
	if err != nil {
		return fmt.Errorf("read config schema %q: %w", schemaPath, err)
	}
	if err := validateConfigAgainstSchema(data, schemaPath, config); err != nil {
		return err
	}
	return nil
}

// ValidateManifestSchema makes plugin verify fail early for a malformed or
// missing declared schema, even when no concrete configuration is available.
func ValidateManifestSchema(manifestPath string, manifest Manifest) error {
	schemaPath, err := ConfigSchemaPath(manifestPath, manifest)
	if err != nil || schemaPath == "" {
		return err
	}
	data, err := readBoundedFile(schemaPath, maxConfigSchemaBytes)
	if err != nil {
		return fmt.Errorf("read config schema %q: %w", schemaPath, err)
	}
	schema, err := parseSchema(data, schemaPath)
	if err != nil {
		return err
	}
	return validateSchemaReferences(schema, schema, "$", 0)
}

func pathWithin(base, candidate string) bool {
	rel, err := filepath.Rel(base, candidate)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// validateSchemaReferences checks every reference while validating a manifest,
// rather than waiting until a config value happens to traverse that branch.
// It deliberately resolves, but does not recursively expand, references so
// legitimate recursive JSON Schemas remain supported.
func validateSchemaReferences(value any, root map[string]any, path string, depth int) error {
	if depth > maxSchemaDepth {
		return fmt.Errorf("%s: schema nesting exceeds %d levels", path, maxSchemaDepth)
	}
	switch typed := value.(type) {
	case map[string]any:
		if rawRef, exists := typed["$ref"]; exists {
			ref, ok := rawRef.(string)
			if !ok {
				return fmt.Errorf("%s: $ref must be a string", path)
			}
			target, err := resolveLocalReference(root, ref)
			if err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			switch target.(type) {
			case map[string]any, bool:
			default:
				return fmt.Errorf("%s: JSON Schema reference %q does not resolve to a schema", path, ref)
			}
		}
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if err := validateSchemaReferences(typed[key], root, pathForKey(path, key), depth+1); err != nil {
				return err
			}
		}
	case []any:
		for index, child := range typed {
			if err := validateSchemaReferences(child, root, fmt.Sprintf("%s[%d]", path, index), depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

func readBoundedFile(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("file exceeds %d byte limit", limit)
	}
	return data, nil
}

func validateConfigAgainstSchema(data []byte, schemaName string, config any) error {
	schema, err := parseSchema(data, schemaName)
	if err != nil {
		return err
	}
	normalized, err := normalizeConfig(config)
	if err != nil {
		return fmt.Errorf("config is not JSON-serializable: %w", err)
	}
	if err := validateSchemaValue(schema, schema, normalized, "$", 0); err != nil {
		return fmt.Errorf("config does not match schema %q: %w", schemaName, err)
	}
	return nil
}

func parseSchema(data []byte, schemaName string) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("parse JSON Schema %q: %w", schemaName, err)
	}
	if err := ensureSingleJSONValue(decoder); err != nil {
		return nil, fmt.Errorf("parse JSON Schema %q: %w", schemaName, err)
	}
	schema, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("JSON Schema %q must be an object", schemaName)
	}
	return schema, nil
}

func normalizeConfig(config any) (any, error) {
	if config == nil {
		config = map[string]any{}
	}
	data, err := json.Marshal(config)
	if err != nil {
		return nil, err
	}
	if len(data) > maxConfigJSONBytes {
		return nil, fmt.Errorf("serialized config exceeds %d byte limit", maxConfigJSONBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var normalized any
	if err := decoder.Decode(&normalized); err != nil {
		return nil, err
	}
	if err := ensureSingleJSONValue(decoder); err != nil {
		return nil, err
	}
	return normalized, nil
}

func ensureSingleJSONValue(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if err == io.EOF {
		return nil
	}
	if err == nil {
		return fmt.Errorf("document contains more than one JSON value")
	}
	return err
}

func validateSchemaValue(schema any, root map[string]any, value any, path string, depth int) error {
	if depth > maxSchemaDepth {
		return fmt.Errorf("%s: schema nesting exceeds %d levels", path, maxSchemaDepth)
	}

	// Boolean schemas are legal JSON Schema vocabulary. Supporting them makes
	// additionalProperties and simple conditional branches unambiguous.
	if allowed, ok := schema.(bool); ok {
		if allowed {
			return nil
		}
		return fmt.Errorf("%s: value is prohibited by schema", path)
	}

	object, ok := schema.(map[string]any)
	if !ok {
		return fmt.Errorf("%s: schema node must be an object or boolean", path)
	}
	if rawRef, exists := object["$ref"]; exists {
		ref, ok := rawRef.(string)
		if !ok {
			return fmt.Errorf("%s: $ref must be a string", path)
		}
		target, err := resolveLocalReference(root, ref)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		return validateSchemaValue(target, root, value, path, depth+1)
	}

	if raw, exists := object["allOf"]; exists {
		branches, err := schemaArray(raw, path, "allOf")
		if err != nil {
			return err
		}
		for _, branch := range branches {
			if err := validateSchemaValue(branch, root, value, path, depth+1); err != nil {
				return err
			}
		}
	}
	if raw, exists := object["anyOf"]; exists {
		branches, err := schemaArray(raw, path, "anyOf")
		if err != nil {
			return err
		}
		var firstErr error
		for _, branch := range branches {
			if err := validateSchemaValue(branch, root, value, path, depth+1); err == nil {
				firstErr = nil
				break
			} else if firstErr == nil {
				firstErr = err
			}
		}
		if firstErr != nil {
			return fmt.Errorf("%s: does not match any anyOf branch (%v)", path, firstErr)
		}
	}
	if raw, exists := object["oneOf"]; exists {
		branches, err := schemaArray(raw, path, "oneOf")
		if err != nil {
			return err
		}
		matches := 0
		for _, branch := range branches {
			if err := validateSchemaValue(branch, root, value, path, depth+1); err == nil {
				matches++
			}
		}
		if matches != 1 {
			return fmt.Errorf("%s: must match exactly one oneOf branch, matched %d", path, matches)
		}
	}
	if raw, exists := object["not"]; exists {
		if err := validateSchemaValue(raw, root, value, path, depth+1); err == nil {
			return fmt.Errorf("%s: must not match forbidden schema", path)
		}
	}

	if raw, exists := object["const"]; exists && !jsonValuesEqual(value, raw) {
		return fmt.Errorf("%s: must equal %s", path, renderJSON(raw))
	}
	if raw, exists := object["enum"]; exists {
		values, ok := raw.([]any)
		if !ok {
			return fmt.Errorf("%s: schema enum must be an array", path)
		}
		matched := false
		for _, candidate := range values {
			if jsonValuesEqual(value, candidate) {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("%s: must be one of %s", path, renderJSON(values))
		}
	}
	if raw, exists := object["type"]; exists {
		matched, err := matchesSchemaType(raw, value)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if !matched {
			return fmt.Errorf("%s: expected %s, got %s", path, renderSchemaType(raw), jsonTypeName(value))
		}
	}

	if objectValue, ok := value.(map[string]any); ok {
		if err := validateObjectKeywords(object, root, objectValue, path, depth+1); err != nil {
			return err
		}
	}
	if arrayValue, ok := value.([]any); ok {
		if err := validateArrayKeywords(object, root, arrayValue, path, depth+1); err != nil {
			return err
		}
	}
	if stringValue, ok := value.(string); ok {
		if err := validateStringKeywords(object, stringValue, path); err != nil {
			return err
		}
	}
	if numberValue, ok := asFloat(value); ok {
		if err := validateNumberKeywords(object, numberValue, path); err != nil {
			return err
		}
	}
	return nil
}

func validateObjectKeywords(schema map[string]any, root map[string]any, value map[string]any, path string, depth int) error {
	if raw, exists := schema["minProperties"]; exists {
		minimum, err := nonNegativeInteger(raw, "minProperties", path)
		if err != nil {
			return err
		}
		if len(value) < minimum {
			return fmt.Errorf("%s: must contain at least %d properties", path, minimum)
		}
	}
	if raw, exists := schema["maxProperties"]; exists {
		maximum, err := nonNegativeInteger(raw, "maxProperties", path)
		if err != nil {
			return err
		}
		if len(value) > maximum {
			return fmt.Errorf("%s: must contain at most %d properties", path, maximum)
		}
	}

	properties := map[string]any{}
	if raw, exists := schema["properties"]; exists {
		var ok bool
		properties, ok = raw.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: schema properties must be an object", path)
		}
	}
	if raw, exists := schema["required"]; exists {
		required, ok := raw.([]any)
		if !ok {
			return fmt.Errorf("%s: schema required must be an array", path)
		}
		for _, item := range required {
			name, ok := item.(string)
			if !ok || name == "" {
				return fmt.Errorf("%s: schema required must contain non-empty strings", path)
			}
			if _, exists := value[name]; !exists {
				return fmt.Errorf("%s.%s: is required", path, quotePathSegment(name))
			}
		}
	}

	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		propertySchema, declared := properties[key]
		if declared {
			if err := validateSchemaValue(propertySchema, root, value[key], pathForKey(path, key), depth+1); err != nil {
				return err
			}
			continue
		}

		additional, hasAdditional := schema["additionalProperties"]
		if !hasAdditional {
			continue
		}
		if allowed, ok := additional.(bool); ok {
			if !allowed {
				return fmt.Errorf("%s.%s: additional property is not allowed", path, quotePathSegment(key))
			}
			continue
		}
		if err := validateSchemaValue(additional, root, value[key], pathForKey(path, key), depth+1); err != nil {
			return err
		}
	}
	return nil
}

func validateArrayKeywords(schema map[string]any, root map[string]any, value []any, path string, depth int) error {
	if raw, exists := schema["minItems"]; exists {
		minimum, err := nonNegativeInteger(raw, "minItems", path)
		if err != nil {
			return err
		}
		if len(value) < minimum {
			return fmt.Errorf("%s: must contain at least %d items", path, minimum)
		}
	}
	if raw, exists := schema["maxItems"]; exists {
		maximum, err := nonNegativeInteger(raw, "maxItems", path)
		if err != nil {
			return err
		}
		if len(value) > maximum {
			return fmt.Errorf("%s: must contain at most %d items", path, maximum)
		}
	}
	if raw, exists := schema["items"]; exists {
		for index, item := range value {
			if err := validateSchemaValue(raw, root, item, fmt.Sprintf("%s[%d]", path, index), depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateStringKeywords(schema map[string]any, value, path string) error {
	if raw, exists := schema["minLength"]; exists {
		minimum, err := nonNegativeInteger(raw, "minLength", path)
		if err != nil {
			return err
		}
		if len([]rune(value)) < minimum {
			return fmt.Errorf("%s: length must be at least %d", path, minimum)
		}
	}
	if raw, exists := schema["maxLength"]; exists {
		maximum, err := nonNegativeInteger(raw, "maxLength", path)
		if err != nil {
			return err
		}
		if len([]rune(value)) > maximum {
			return fmt.Errorf("%s: length must be at most %d", path, maximum)
		}
	}
	if raw, exists := schema["pattern"]; exists {
		pattern, ok := raw.(string)
		if !ok {
			return fmt.Errorf("%s: schema pattern must be a string", path)
		}
		compiled, err := regexp.Compile(pattern)
		if err != nil {
			return fmt.Errorf("%s: schema pattern is invalid: %w", path, err)
		}
		if !compiled.MatchString(value) {
			return fmt.Errorf("%s: does not match pattern %q", path, pattern)
		}
	}
	return nil
}

func validateNumberKeywords(schema map[string]any, value float64, path string) error {
	constraints := []struct {
		key       string
		exclusive bool
		compare   func(float64, float64) bool
		message   string
	}{
		{"minimum", false, func(got, limit float64) bool { return got < limit }, "must be greater than or equal to"},
		{"maximum", false, func(got, limit float64) bool { return got > limit }, "must be less than or equal to"},
		{"exclusiveMinimum", true, func(got, limit float64) bool { return got <= limit }, "must be greater than"},
		{"exclusiveMaximum", true, func(got, limit float64) bool { return got >= limit }, "must be less than"},
	}
	for _, constraint := range constraints {
		raw, exists := schema[constraint.key]
		if !exists {
			continue
		}
		limit, ok := asFloat(raw)
		if !ok || math.IsNaN(limit) || math.IsInf(limit, 0) {
			return fmt.Errorf("%s: schema %s must be a finite number", path, constraint.key)
		}
		if constraint.compare(value, limit) {
			return fmt.Errorf("%s: %s %s %s", path, constraint.message, constraint.key, renderJSON(raw))
		}
	}
	return nil
}

func schemaArray(value any, path, key string) ([]any, error) {
	array, ok := value.([]any)
	if !ok || len(array) == 0 {
		return nil, fmt.Errorf("%s: schema %s must be a non-empty array", path, key)
	}
	return array, nil
}

func matchesSchemaType(raw any, value any) (bool, error) {
	switch typed := raw.(type) {
	case string:
		return matchesOneSchemaType(typed, value)
	case []any:
		if len(typed) == 0 {
			return false, fmt.Errorf("schema type array must not be empty")
		}
		for _, item := range typed {
			name, ok := item.(string)
			if !ok {
				return false, fmt.Errorf("schema type array must contain strings")
			}
			matches, err := matchesOneSchemaType(name, value)
			if err != nil {
				return false, err
			}
			if matches {
				return true, nil
			}
		}
		return false, nil
	default:
		return false, fmt.Errorf("schema type must be a string or array of strings")
	}
}

func matchesOneSchemaType(name string, value any) (bool, error) {
	switch name {
	case "object":
		_, ok := value.(map[string]any)
		return ok, nil
	case "array":
		_, ok := value.([]any)
		return ok, nil
	case "string":
		_, ok := value.(string)
		return ok, nil
	case "boolean":
		_, ok := value.(bool)
		return ok, nil
	case "null":
		return value == nil, nil
	case "number":
		_, ok := asFloat(value)
		return ok, nil
	case "integer":
		return isInteger(value), nil
	default:
		return false, fmt.Errorf("schema type %q is unsupported", name)
	}
}

func isInteger(value any) bool {
	number, ok := value.(json.Number)
	if !ok {
		return false
	}
	text := string(number)
	if !strings.ContainsAny(text, ".eE") {
		_, err := strconv.ParseInt(text, 10, 64)
		if err == nil {
			return true
		}
		// Very large integral JSON values are still valid integers even if they
		// do not fit int64. ParseFloat below handles their mathematical shape.
	}
	parsed, err := strconv.ParseFloat(text, 64)
	return err == nil && !math.IsInf(parsed, 0) && !math.IsNaN(parsed) && math.Trunc(parsed) == parsed
}

func asFloat(value any) (float64, bool) {
	switch typed := value.(type) {
	case json.Number:
		parsed, err := typed.Float64()
		return parsed, err == nil
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case int8:
		return float64(typed), true
	case int16:
		return float64(typed), true
	case int32:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case uint:
		return float64(typed), true
	case uint8:
		return float64(typed), true
	case uint16:
		return float64(typed), true
	case uint32:
		return float64(typed), true
	case uint64:
		return float64(typed), true
	default:
		return 0, false
	}
}

func nonNegativeInteger(value any, key, path string) (int, error) {
	number, ok := asFloat(value)
	if !ok || number < 0 || math.Trunc(number) != number || number > math.MaxInt {
		return 0, fmt.Errorf("%s: schema %s must be a non-negative integer", path, key)
	}
	return int(number), nil
}

func resolveLocalReference(root map[string]any, ref string) (any, error) {
	if ref == "#" {
		return root, nil
	}
	if !strings.HasPrefix(ref, "#/") {
		return nil, fmt.Errorf("external JSON Schema reference %q is not allowed", ref)
	}
	var current any = root
	for _, rawToken := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		token := strings.ReplaceAll(strings.ReplaceAll(rawToken, "~1", "/"), "~0", "~")
		switch typed := current.(type) {
		case map[string]any:
			next, ok := typed[token]
			if !ok {
				return nil, fmt.Errorf("JSON Schema reference %q does not exist", ref)
			}
			current = next
		case []any:
			index, err := strconv.Atoi(token)
			if err != nil || index < 0 || index >= len(typed) {
				return nil, fmt.Errorf("JSON Schema reference %q does not exist", ref)
			}
			current = typed[index]
		default:
			return nil, fmt.Errorf("JSON Schema reference %q does not resolve to a schema", ref)
		}
	}
	return current, nil
}

func jsonValuesEqual(left, right any) bool {
	if leftNumber, leftOK := asFloat(left); leftOK {
		if rightNumber, rightOK := asFloat(right); rightOK {
			return leftNumber == rightNumber
		}
	}
	return reflect.DeepEqual(left, right)
}

func renderJSON(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprintf("%v", value)
	}
	return string(data)
}

func renderSchemaType(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			parts = append(parts, fmt.Sprint(item))
		}
		return strings.Join(parts, " or ")
	default:
		return renderJSON(value)
	}
}

func jsonTypeName(value any) string {
	switch value.(type) {
	case nil:
		return "null"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case bool:
		return "boolean"
	case json.Number, float64, float32, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return "number"
	default:
		return fmt.Sprintf("%T", value)
	}
}

func quotePathSegment(key string) string {
	if key == "" {
		return `""`
	}
	return key
}

func pathForKey(path, key string) string {
	return path + "." + quotePathSegment(key)
}
