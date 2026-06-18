// Package xrayapi wraps the xray binary's CLI API commands:
//   - "xray api adu"         — hot-add a user (AddUser)
//   - "xray api rmu"         — hot-remove a user (RemoveUser)
//   - "xray api statsquery"  — query traffic counters (QueryStats)
//
// For HY2: one request is made using exactly what the template produced.
// There is no fallback format loop — the template is the source of truth.
package xrayapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"xraytool/internal/xrayconfig"
)

// Client wraps xray binary API calls.
type Client struct {
	addr string // e.g. "127.0.0.1:10085"
}

// New creates a new Client.
func New(addr string) *Client {
	return &Client{addr: addr}
}

// ---------------------------------------------------------------------------
// AddUser — xray api adu
// ---------------------------------------------------------------------------

// AddUser hot-adds users to all relevant inbounds via "xray api adu".
// The payload is a slice of TaggedClient (one per inbound).
// configPath is used to read the current inbound metadata (port, clientsKey).
func (c *Client) AddUser(payload []xrayconfig.TaggedClient, configPath string) error {
	if len(payload) == 0 {
		return nil
	}
	
	// Hot-remove user from memory before adding to seamlessly support updates.
	email := payload[0].Client.Email()
	if email != "" {
		var tags []string
		for _, tc := range payload {
			tags = append(tags, tc.Tag)
		}
		_ = c.RemoveUser(email, tags)
	}

	apiPld := buildAddPayload(payload, configPath)
	data, err := json.Marshal(apiPld)
	if err != nil {
		return fmt.Errorf("marshaling adu payload: %w", err)
	}

	// Fix for Windows: Write payload to temp file instead of using /dev/stdin
	f, err := os.CreateTemp("", "xray-api-adu-*")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("writing to temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing temp file: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "xray", "api", "adu", "-s", c.addr, f.Name())
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}

	// Fallback: apply per-inbound so one problematic protocol does not break hot add for all tags.
	fmt.Fprintf(os.Stderr, "[WARN] Batch hot-add failed: %v\nOutput: %s\nFalling back to per-inbound registration...\n", err, strings.TrimSpace(string(out)))
	var lastErr error
	for _, aib := range apiPld.Inbounds {
		singlePld := aduPayload{Inbounds: []aduInbound{aib}}
		singleData, err := json.Marshal(singlePld)
		if err != nil {
			lastErr = err
			continue
		}

		func() {
			sf, err := os.CreateTemp("", "xray-api-adu-single-*")
			if err != nil {
				lastErr = err
				return
			}
			defer os.Remove(sf.Name())
			
			if _, err := sf.Write(singleData); err != nil {
				sf.Close()
				lastErr = err
				return
			}
			if err := sf.Close(); err != nil {
				lastErr = err
				return
			}

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			singleCmd := exec.CommandContext(ctx, "xray", "api", "adu", "-s", c.addr, sf.Name())
			singleOut, err := singleCmd.CombinedOutput()

			if err != nil {
				lastErr = fmt.Errorf("tag=%s: %v", aib.Tag, err)
				if len(singleOut) > 0 {
					lastErr = fmt.Errorf("%w (output: %s)", lastErr, strings.TrimSpace(string(singleOut)))
				}
				fmt.Fprintf(os.Stderr, "[ERROR] xray api adu failed for tag=%s: %v\n", aib.Tag, lastErr)
			}
		}()
	}
	return lastErr
}

// ---------------------------------------------------------------------------
// RemoveUser — xray api rmu
// ---------------------------------------------------------------------------

// RemoveUser hot-removes a user from every provided inbound tag.
// All errors are collected; the operation continues even if one tag fails.
func (c *Client) RemoveUser(email string, tags []string) error {
	if len(tags) == 0 {
		return nil
	}
	if !regexp.MustCompile(`^[a-zA-Z0-9_@.\-]+$`).MatchString(email) || strings.HasPrefix(email, "-") {
		return fmt.Errorf("invalid email format")
	}

	var errs []string
	for _, tag := range tags {
		func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "xray", "api", "rmu", "-s", c.addr,
				fmt.Sprintf("-tag=%s", tag), email)
			out, err := cmd.CombinedOutput()
			if err != nil {
				errs = append(errs, fmt.Sprintf("tag=%s: %v (output: %s)", tag, err, strings.TrimSpace(string(out))))
			}
		}()
	}
	if len(errs) > 0 {
		return fmt.Errorf("xray api rmu errors: %s", strings.Join(errs, "; "))
	}
	return nil
}

// ---------------------------------------------------------------------------
// QueryStats — xray api statsquery
// ---------------------------------------------------------------------------

// UserStat is the traffic counters for one user.
type UserStat struct {
	Email string
	Up    int64
	Down  int64
}

// QueryStats queries the xray API for per-user traffic counters.
func (c *Client) QueryStats() ([]UserStat, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "xray", "api", "statsquery",
		fmt.Sprintf("--server=%s", c.addr))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("xray api statsquery: %w (output: %s)", err, strings.TrimSpace(string(out)))
	}
	return parseStats(out)
}

// ---------------------------------------------------------------------------
// Payload builders
// ---------------------------------------------------------------------------

// aduPayload is the top-level JSON object sent to "xray api adu".
type aduPayload struct {
	Inbounds []aduInbound `json:"inbounds"`
}

type aduInbound struct {
	Tag      string                     `json:"tag"`
	Port     interface{}                `json:"port,omitempty"`
	Protocol string                     `json:"protocol,omitempty"`
	Settings map[string]json.RawMessage `json:"settings"`
}

func buildAddPayload(payload []xrayconfig.TaggedClient, configPath string) aduPayload {
	// Read inbound metadata from the live config.
	cfg, cfgErr := xrayconfig.Read(configPath)
	if cfgErr != nil {
		fmt.Fprintf(os.Stderr, "[WARN] xrayapi: не удалось прочитать конфиг %s: %v\n", configPath, cfgErr)
	}
	var inbounds []xrayconfig.RawInbound
	if cfg != nil {
		inbounds, _ = cfg.GetInbounds()
	}
	ibByTag := make(map[string]xrayconfig.RawInbound, len(inbounds))
	for _, ib := range inbounds {
		ibByTag[ib.Tag()] = ib
	}

	result := aduPayload{}
	for _, tc := range payload {
		apiClient := tc.Client.ForXrayAPI()
		raw, err := json.Marshal(apiClient)
		if err != nil {
			continue
		}

		aib := aduInbound{Tag: tc.Tag}

		settingsMap := make(map[string]json.RawMessage)
		if ib, ok := ibByTag[tc.Tag]; ok {
			aib.Protocol = ib.Protocol()
			// Copy port from config.
			if rawPort, ok := ib["port"]; ok {
				var p interface{}
				if json.Unmarshal(rawPort, &p) == nil {
					aib.Port = p
				}
			}

			// Copy original settings if possible.
			if rawSettings, ok := ib["settings"]; ok {
				_ = json.Unmarshal(rawSettings, &settingsMap)
			}
			if settingsMap == nil {
				settingsMap = make(map[string]json.RawMessage)
			}

			// Wrap client in array.
			rawArray, err := json.Marshal([]json.RawMessage{raw})
			if err == nil {
				key := ib.ClientsKey()
				if key == "" {
					key = "clients"
				}
				settingsMap[key] = rawArray
			}
		} else {
			// Config not available; default to "clients".
			rawArray, err := json.Marshal([]json.RawMessage{raw})
			if err == nil {
				settingsMap["clients"] = rawArray
			}
		}

		aib.Settings = settingsMap
		result.Inbounds = append(result.Inbounds, aib)
	}
	return result
}

// ---------------------------------------------------------------------------
// Stats parser
// ---------------------------------------------------------------------------

type statsResponse struct {
	Stat []struct {
		Name  string `json:"name"`
		Value int64  `json:"value"`
	} `json:"stat"`
}

func parseStats(data []byte) ([]UserStat, error) {
	// Handle empty / null response gracefully.
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}

	var resp statsResponse
	if err := json.Unmarshal(trimmed, &resp); err != nil {
		return nil, fmt.Errorf("parsing statsquery response: %w", err)
	}

	// Accumulate up/down per email.
	type counters struct{ up, down int64 }
	acc := make(map[string]*counters)

	for _, s := range resp.Stat {
		// Format: "user>>>email@example.com>>>traffic>>>uplink"
		if !strings.HasPrefix(s.Name, "user>>>") {
			continue
		}

		var email, key string
		if strings.HasSuffix(s.Name, ">>>traffic>>>uplink") {
			email = s.Name[len("user>>>") : len(s.Name)-len(">>>traffic>>>uplink")]
			key = "up"
		} else if strings.HasSuffix(s.Name, ">>>traffic>>>downlink") {
			email = s.Name[len("user>>>") : len(s.Name)-len(">>>traffic>>>downlink")]
			key = "down"
		} else {
			continue
		}

		if email == "" {
			continue
		}
		if acc[email] == nil {
			acc[email] = &counters{}
		}
		if key == "up" {
			acc[email].up += s.Value
		} else {
			acc[email].down += s.Value
		}
	}

	stats := make([]UserStat, 0, len(acc))
	for email, cnt := range acc {
		stats = append(stats, UserStat{Email: email, Up: cnt.up, Down: cnt.down})
	}
	return stats, nil
}
