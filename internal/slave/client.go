// Package slave provides an HTTP client for communicating with slave servers
// via their REST API (POST JSON → response JSON).
package slave

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Entry holds the connection details for one slave server.
// Fields mirror the servers.json structure (both naming styles are accepted).
type Entry struct {
	// Full URL (preferred). If set, Domain/Scheme/Port/Path are ignored.
	URL string `json:"url" yaml:"url"`

	// Components used to build the URL when URL is not set.
	Domain string      `json:"domain" yaml:"domain"`
	Host   string      `json:"host" yaml:"host"`
	IP     string      `json:"ip" yaml:"ip"`
	Scheme string      `json:"scheme" yaml:"scheme"`
	Port   interface{} `json:"port" yaml:"port"`
	Path   string      `json:"path" yaml:"path"`

	// Authentication - all styles are supported.
	APIKey           string `json:"api_key" yaml:"api_key"`
	APIKeyCamel      string `json:"apiKey" yaml:"apiKey"`
	XAPIKey          string `json:"x_api_key" yaml:"x_api_key"`
	XAPIKeyCamel     string `json:"xApiKey" yaml:"xApiKey"`
	Token            string `json:"token" yaml:"token"`
	APIToken         string `json:"apiToken" yaml:"apiToken"`
	Bearer           string `json:"bearer" yaml:"bearer"`
	BearerToken      string `json:"bearer_token" yaml:"bearer_token"`
	BearerTokenCamel string `json:"bearerToken" yaml:"bearerToken"`
	AuthHeader       string `json:"auth_header" yaml:"auth_header"`     // full "Header-Name: value"
	Authorization    string `json:"authorization" yaml:"authorization"` // ditto

	Insecure      bool `json:"insecure" yaml:"insecure"`
	AllowInsecure bool `json:"allow_insecure" yaml:"allow_insecure"`
}

// Endpoint builds the base URL for this server using remotePath as default path.
func (e Entry) Endpoint(remotePath string) string {
	if e.URL != "" {
		u, err := url.Parse(e.URL)
		if err == nil && (u.Path == "" || u.Path == "/") {
			// Append remotePath if URL is just the root domain
			return strings.TrimRight(e.URL, "/") + "/" + strings.TrimLeft(remotePath, "/")
		}
		return strings.TrimRight(e.URL, "/")
	}
	domain := firstNonEmpty(e.Domain, e.Host, e.IP)
	if domain == "" {
		return ""
	}
	scheme := e.Scheme
	if scheme == "" {
		scheme = "https"
	}
	path := e.Path
	if path == "" {
		path = remotePath
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if p := portString(e.Port); p != "" {
		return fmt.Sprintf("%s://%s:%s%s", scheme, domain, p, path)
	}
	return fmt.Sprintf("%s://%s%s", scheme, domain, path)
}

// ---------------------------------------------------------------------------
// HTTP client
// ---------------------------------------------------------------------------

// Client makes HTTP calls to slave servers.
type Client struct {
	http         *http.Client
	httpInsecure *http.Client
	remotePath   string
}

// NewClient creates a new Client with the given timeouts.
func NewClient(connectTimeout, requestTimeout time.Duration, remotePath string) *Client {
	transport := &http.Transport{
		ResponseHeaderTimeout: requestTimeout,
		DialContext: (&net.Dialer{
			Timeout: connectTimeout,
		}).DialContext,
	}
	transportInsecure := &http.Transport{
		ResponseHeaderTimeout: requestTimeout,
		DialContext: (&net.Dialer{
			Timeout: connectTimeout,
		}).DialContext,
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	return &Client{
		http:         &http.Client{Transport: transport, Timeout: requestTimeout},
		httpInsecure: &http.Client{Transport: transportInsecure, Timeout: requestTimeout},
		remotePath:   remotePath,
	}
}

// Call sends a POST request to <entry endpoint>/<cmd> with params as JSON body.
// Returns the unwrapped response body or an error.
func (c *Client) Call(entry Entry, cmd string, params map[string]string) (string, error) {
	endpoint := entry.Endpoint(c.remotePath)
	if endpoint == "" {
		return "", fmt.Errorf("cannot determine endpoint for slave server")
	}
	url := endpoint

	// Copy params to prevent concurrent map mutation when called in parallel goroutines
	localParams := make(map[string]string, len(params)+1)
	for k, v := range params {
		localParams[k] = v
	}
	localParams["action"] = cmd

	payload, err := json.Marshal(localParams)
	if err != nil {
		return "", fmt.Errorf("marshaling params: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	applyAuthHeaders(req, entry)

	httpClient := c.http
	if entry.Insecure || entry.AllowInsecure {
		httpClient = c.httpInsecure
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("request to %s: %w", url, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("reading response: %w", err)
	}
	bodyStr := strings.TrimSpace(string(body))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if bodyStr != "" {
			return "", fmt.Errorf("HTTP %d | %s", resp.StatusCode, bodyStr)
		}
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	return unwrapBody(bodyStr)
}

// CallDecode sends a POST request to <entry endpoint>/<cmd> with params as JSON body.
// It decodes the JSON response directly into the provided target structure, avoiding loading the entire response into memory.
func (c *Client) CallDecode(entry Entry, cmd string, params map[string]string, target interface{}) error {
	endpoint := entry.Endpoint(c.remotePath)
	if endpoint == "" {
		return fmt.Errorf("cannot determine endpoint for slave server")
	}
	url := endpoint

	// Copy params to prevent concurrent map mutation when called in parallel goroutines
	localParams := make(map[string]string, len(params)+1)
	for k, v := range params {
		localParams[k] = v
	}
	localParams["action"] = cmd

	payload, err := json.Marshal(localParams)
	if err != nil {
		return fmt.Errorf("marshaling params: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	applyAuthHeaders(req, entry)

	httpClient := c.http
	if entry.Insecure || entry.AllowInsecure {
		httpClient = c.httpInsecure
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request to %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodySnippet, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("HTTP %d | %s", resp.StatusCode, strings.TrimSpace(string(bodySnippet)))
	}

	if err := json.NewDecoder(resp.Body).Decode(target); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}

	return nil
}

// ---------------------------------------------------------------------------
// Response unwrapper
// ---------------------------------------------------------------------------

// unwrapBody parses the response body. Handles both raw JSON and the
// {"status":"success","output":"..."} envelope used by some PHP proxies.
func unwrapBody(body string) (string, error) {
	if body == "" {
		return "", nil
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		// Not JSON — return as plain text.
		return body, nil
	}

	// Check for explicit error flags.
	if statusRaw, ok := obj["status"]; ok {
		var status string
		if json.Unmarshal(statusRaw, &status) == nil {
			if status == "error" || status == "false" {
				return "", fmt.Errorf("%s", body)
			}
		}
	}
	if okRaw, ok := obj["ok"]; ok {
		var ok bool
		if json.Unmarshal(okRaw, &ok) == nil && !ok {
			return "", fmt.Errorf("%s", body)
		}
	}

	// Unwrap PHP envelope: {"status":"success","output":"<raw payload>"}
	if outputRaw, ok := obj["output"]; ok {
		var output string
		if json.Unmarshal(outputRaw, &output) == nil {
			return output, nil
		}
	}

	return body, nil
}

// ---------------------------------------------------------------------------
// Auth helpers
// ---------------------------------------------------------------------------

func applyAuthHeaders(req *http.Request, e Entry) {
	if key := firstNonEmpty(e.APIKey, e.APIKeyCamel, e.XAPIKey, e.XAPIKeyCamel, e.Token, e.APIToken); key != "" {
		req.Header.Set("X-API-Key", key)
	}
	if bearer := firstNonEmpty(e.Bearer, e.BearerToken, e.BearerTokenCamel); bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	for _, h := range []string{e.AuthHeader, e.Authorization} {
		if h == "" || h == "null" {
			continue
		}
		if idx := strings.Index(h, ":"); idx > 0 {
			req.Header.Set(strings.TrimSpace(h[:idx]), strings.TrimSpace(h[idx+1:]))
		}
	}
}

// ---------------------------------------------------------------------------
// Misc helpers
// ---------------------------------------------------------------------------

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" && v != "null" {
			return v
		}
	}
	return ""
}

func portString(port interface{}) string {
	if port == nil {
		return ""
	}
	s := fmt.Sprintf("%v", port)
	if s == "0" || s == "" || s == "<nil>" {
		return ""
	}
	return s
}
