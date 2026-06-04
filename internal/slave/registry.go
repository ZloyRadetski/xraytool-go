package slave

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
)

// Registry loads slave server entries and propagates commands in parallel.
type Registry struct {
	path   string
	client *Client
}

// NewRegistry creates a Registry backed by the given servers.json path.
func NewRegistry(path string, client *Client) *Registry {
	return &Registry{path: path, client: client}
}

// Servers returns all configured slave servers as a name→Entry map.
// Returns nil (no error) when the file does not exist.
func (r *Registry) Servers() (map[string]Entry, error) {
	if _, err := os.Stat(r.path); os.IsNotExist(err) {
		return nil, nil
	}
	data, err := os.ReadFile(r.path)
	if err != nil {
		return nil, fmt.Errorf("reading servers.json: %w", err)
	}
	return parseServers(data)
}

// PropagateResult holds the outcome of one slave API call.
type PropagateResult struct {
	Server string
	Output string
	Err    error
}

// PropagateAll sends cmd with params to every slave in parallel.
// Returns nil when no servers are configured.
func (r *Registry) PropagateAll(cmd string, params map[string]string) []PropagateResult {
	servers, err := r.Servers()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[WARN] slave: не удалось загрузить список серверов: %v\n", err)
		return nil
	}
	if len(servers) == 0 {
		return nil
	}

	results := make([]PropagateResult, 0, len(servers))
	var mu sync.Mutex
	var wg sync.WaitGroup

	for name, entry := range servers {
		name, entry := name, entry
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := r.client.Call(entry, cmd, params)
			mu.Lock()
			results = append(results, PropagateResult{Server: name, Output: out, Err: err})
			mu.Unlock()
		}()
	}
	wg.Wait()
	return results
}

// CallOne calls cmd on a single named slave. Useful for syncstates.
func (r *Registry) CallOne(serverName, cmd string, params map[string]string) (string, error) {
	servers, err := r.Servers()
	if err != nil {
		return "", err
	}
	entry, ok := servers[serverName]
	if !ok {
		return "", fmt.Errorf("unknown slave server: %q", serverName)
	}
	return r.client.Call(entry, cmd, params)
}

// ---------------------------------------------------------------------------
// servers.json parser — supports both object and array formats
// ---------------------------------------------------------------------------

func parseServers(data []byte) (map[string]Entry, error) {
	// Try object format: {"name": {Entry...}, ...}
	var objFmt map[string]Entry
	if err := json.Unmarshal(data, &objFmt); err == nil && len(objFmt) > 0 {
		return objFmt, nil
	}

	// Try array format: [{name: "...", ...}, ...]
	var arrFmt []json.RawMessage
	if err := json.Unmarshal(data, &arrFmt); err != nil {
		return nil, fmt.Errorf("parsing servers.json: %w", err)
	}

	result := make(map[string]Entry, len(arrFmt))
	for i, raw := range arrFmt {
		var entry Entry
		if err := json.Unmarshal(raw, &entry); err != nil {
			continue
		}
		// Determine name from various possible fields.
		var nameFields struct {
			Name   string `json:"name"`
			ID     string `json:"id"`
			Key    string `json:"key"`
			Server string `json:"server"`
		}
		json.Unmarshal(raw, &nameFields) //nolint:errcheck
		name := firstNonEmpty(nameFields.Name, nameFields.ID, nameFields.Key, nameFields.Server)
		if name == "" {
			name = fmt.Sprintf("%d", i)
		}
		result[name] = entry
	}
	return result, nil
}
