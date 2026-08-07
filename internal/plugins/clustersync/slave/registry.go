package slave

import (
	"fmt"
	"os"
	"sync"
)

// Registry holds slave server entries and propagates commands in parallel.
type Registry struct {
	servers map[string]Entry
	client  *Client
}

// NewRegistry creates a Registry backed by the given servers map.
func NewRegistry(servers map[string]Entry, client *Client) *Registry {
	if servers == nil {
		servers = make(map[string]Entry)
	}
	return &Registry{servers: servers, client: client}
}

// Servers returns all configured slave servers as a name->Entry map.
func (r *Registry) Servers() (map[string]Entry, error) {
	return r.servers, nil
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

// CallOneDecode calls cmd on a single named slave and decodes the JSON response directly into target.
func (r *Registry) CallOneDecode(serverName, cmd string, params map[string]string, target interface{}) error {
	servers, err := r.Servers()
	if err != nil {
		return err
	}
	entry, ok := servers[serverName]
	if !ok {
		return fmt.Errorf("unknown slave server: %q", serverName)
	}
	return r.client.CallDecode(entry, cmd, params, target)
}
