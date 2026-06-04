// Package templates handles the per-inbound client template system.
//
// Each inbound with a settings.clients/users array must have a corresponding
// template file at <templates_dir>/<tag>.txt. The template is a JSON object
// where empty-string fields get filled with generated values, and non-empty
// fields are kept as-is (allowing static overrides).
//
// For HY2: the template defines the exact fields — no fallback formats.
// Example HY2 template (reality-in-443.txt for a hysteria2 inbound):
//
//	{ "email": "", "auth": "", "subfile": "", "expire": "" }
package templates

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"xraytool/internal/xrayconfig"
)

// ClientParams holds the values to fill into a template.
type ClientParams struct {
	Email   string
	UUID    string // for VLESS/VMESS: fills "id" field
	Auth    string // for HY2: fills "auth" / "password" field
	Subfile string
	Expire  string
	Flow    string   // fills "flow" if template has it
	Limit   *float64 // nil = don't set/change
}

// ---------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------

// Validate ensures every client inbound has a readable, valid template file.
// Missing templates are created with sensible defaults and a warning is printed.
func Validate(dir string, cfg xrayconfig.RawConfig) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating templates dir %q: %w", dir, err)
	}

	inbounds, err := cfg.GetInbounds()
	if err != nil {
		return err
	}

	for _, ib := range inbounds {
		if !ib.HasClientList() {
			continue
		}
		tag := ib.Tag()
		if tag == "" {
			return fmt.Errorf("inbound with client list has empty tag — please add a tag to every inbound in config.json")
		}

		path := templatePath(dir, tag)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			if err := writeDefaultTemplate(path, ib.Protocol()); err != nil {
				return fmt.Errorf("creating default template for %q: %w", tag, err)
			}
			fmt.Fprintf(os.Stderr, "[WARN] Created missing template: %s\n", path)
		}

		if _, err := load(path); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Building client objects
// ---------------------------------------------------------------------------

// BuildForAllInbounds builds a TaggedClient entry for every client inbound,
// applying the ClientParams to each template.
func BuildForAllInbounds(dir string, cfg xrayconfig.RawConfig, params ClientParams) ([]xrayconfig.TaggedClient, error) {
	inbounds, err := cfg.GetInbounds()
	if err != nil {
		return nil, err
	}

	var payload []xrayconfig.TaggedClient
	for _, ib := range inbounds {
		if !ib.HasClientList() {
			continue
		}
		tag := ib.Tag()
		if tag == "" {
			continue
		}

		p := params
		if p.Flow == "" {
			p.Flow = DefaultFlowForTag(tag)
		}

		client, err := buildFromTemplate(dir, tag, p)
		if err != nil {
			return nil, fmt.Errorf("building client for inbound %q: %w", tag, err)
		}
		payload = append(payload, xrayconfig.TaggedClient{Tag: tag, Client: client})
	}
	return payload, nil
}

// buildFromTemplate loads the template for the given tag and fills it with params.
func buildFromTemplate(dir, tag string, params ClientParams) (xrayconfig.RawClient, error) {
	if params.Auth == "" {
		params.Auth = buildDeterministicHy2Pass(params.UUID, params.Email)
	}

	tmpl, err := load(templatePath(dir, tag))
	if err != nil {
		return nil, err
	}

	// Start with a copy of the template.
	result := make(xrayconfig.RawClient, len(tmpl))
	for k, v := range tmpl {
		result[k] = v
	}

	// fillIfEmpty sets the field to value only when the current value is empty.
	fillIfEmpty := func(key, value string) {
		if !result.Has(key) {
			return
		}
		if result.GetString(key) == "" && value != "" {
			result.Set(key, value)
		}
	}

	// "email" is always required and always filled.
	if result.GetString("email") == "" {
		result.Set("email", params.Email)
	}

	fillIfEmpty("id", params.UUID)
	fillIfEmpty("auth", params.Auth)
	fillIfEmpty("password", params.Auth) // some HY2 configs use "password"
	fillIfEmpty("subfile", params.Subfile)
	fillIfEmpty("expire", params.Expire)
	fillIfEmpty("flow", params.Flow)

	// Limit: set if pointer is non-nil.
	if params.Limit != nil {
		result.SetNumber("limit", *params.Limit)
	}

	// --- Validation ---
	if result.GetString("email") == "" {
		return nil, fmt.Errorf("tag %q: email is empty after template fill", tag)
	}
	if result.GetString("subfile") == "" {
		return nil, fmt.Errorf("tag %q: subfile is empty after template fill", tag)
	}
	if result.GetString("expire") == "" {
		return nil, fmt.Errorf("tag %q: expire is empty after template fill", tag)
	}
	if result.Has("id") && result.GetString("id") == "" {
		return nil, fmt.Errorf("tag %q: id field exists in template but is still empty — check UUID generation", tag)
	}
	if result.Has("auth") && result.GetString("auth") == "" {
		return nil, fmt.Errorf("tag %q: auth field exists in template but is still empty — check auth generation", tag)
	}

	return result, nil
}

// ---------------------------------------------------------------------------
// Template file I/O
// ---------------------------------------------------------------------------

func load(path string) (xrayconfig.RawClient, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("template not found: %s", path)
		}
		return nil, fmt.Errorf("reading template %s: %w", path, err)
	}
	var tmpl xrayconfig.RawClient
	if err := json.Unmarshal(data, &tmpl); err != nil {
		return nil, fmt.Errorf("parsing template %s: %w (must be a JSON object)", path, err)
	}
	if tmpl == nil {
		return nil, fmt.Errorf("template %s is empty", path)
	}
	return tmpl, nil
}

func writeDefaultTemplate(path, protocol string) error {
	var fields map[string]string
	if isHysteria(protocol) {
		fields = map[string]string{
			"email":   "",
			"auth":    "",
			"subfile": "",
			"expire":  "",
		}
	} else {
		fields = map[string]string{
			"email":   "",
			"id":      "",
			"flow":    "",
			"subfile": "",
			"expire":  "",
		}
	}
	data, err := json.MarshalIndent(fields, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func templatePath(dir, tag string) string {
	return filepath.Join(dir, tag+".txt")
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func isHysteria(protocol string) bool {
	p := strings.ToLower(protocol)
	return p == "hysteria" || p == "hysteria2" || p == "hy2"
}

// DefaultFlowForTag returns the default XTLS flow for well-known inbound tag patterns.
func DefaultFlowForTag(tag string) string {
	if strings.HasPrefix(tag, "reality-in-") ||
		tag == "reality-in-443" ||
		tag == "reality-in-8443" {
		return "xtls-rprx-vision"
	}
	return ""
}

func buildDeterministicHy2Pass(uuidHint, email string) string {
	seed := strings.ReplaceAll(uuidHint, "-", "")
	if seed != "" && strings.ToLower(seed) != "null" {
		return strings.Repeat(seed, 2)[:32]
	}
	// Build seed from email
	var sb strings.Builder
	for _, r := range email {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			sb.WriteRune(r)
		}
	}
	seed = sb.String()
	if seed == "" {
		seed = "hy2fallback"
	}
	pass := strings.Repeat(seed, 4)
	if len(pass) < 32 {
		pass = pass + strings.Repeat("0", 32-len(pass))
	}
	return pass[:32]
}
