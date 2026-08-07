package payment_platega

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"xraytool/internal/pluginapi"
)

const (
	// ServiceName is the method-qualified service published by this plugin.
	// Dynamic consumers should normally use Host.PaymentProviders instead.
	ServiceName     = "payment_provider.platega"
	maxCallbackSize = 1 << 20 // 1 MiB, matching the legacy router limit.
)

// Config is the typed configuration accepted by the Platega plugin.
//
// Example:
//
//	plugins:
//	  payment_platega:
//	    enabled: true
//	    source: builtin
//	    config:
//	      merchant_id: "merchant"
//	      secret: "shared-secret"
//	      return_url: "https://example.test/payment/success"
//	      failed_url: "https://example.test/payment/failed"
type Config struct {
	MerchantID string
	Secret     string
	ReturnURL  string
	FailedURL  string
	Currency   string
}

// Plugin implements pluginapi.PaymentProvider for Platega.
//
// It deliberately owns only gateway work: intent creation, callback
// authentication/parsing, and refunds. Payment persistence, balance crediting,
// subscription extension, and referral rewards remain in the core payment
// service, which consumes the normalized callback result.
type Plugin struct {
	mu sync.RWMutex

	gateway  Gateway
	cfg      Config
	recorder any

	initialized bool
}

// New creates a plugin using the existing internal/payment/platega client.
func New() *Plugin {
	return NewWithGateway(legacyGateway{})
}

// NewWithGateway creates a plugin with a provider-specific gateway adapter.
// It is primarily useful for tests and for replacing the legacy client with an
// official SDK while preserving the public plugin contract.
func NewWithGateway(gateway Gateway) *Plugin {
	if gateway == nil {
		gateway = legacyGateway{}
	}
	return &Plugin{gateway: gateway}
}

func (p *Plugin) Metadata() pluginapi.Metadata {
	return pluginapi.Metadata{
		Name:        "payment_platega",
		Kind:        "payment",
		Version:     "1.0.0",
		APIVersion:  pluginapi.CurrentAPIVersion,
		Description: "Payment intents and verified callbacks through the Platega gateway.",
		Requires: []pluginapi.ServiceRef{
			{Name: "payment_recorder"},
		},
		Publishes: []pluginapi.ServiceRef{
			// The provider is also discoverable through Host.PaymentProviders().
			// A method-qualified name avoids collisions when several gateways are
			// enabled at the same time.
			{Name: ServiceName},
		},
	}
}

// Init validates plugin configuration and resolves the core-owned payment
// recorder. The recorder is intentionally not used to mutate payments here;
// retaining it only verifies the explicit dependency and preserves the service
// boundary required by the plugin graph.
func (p *Plugin) Init(_ context.Context, rawCfg pluginapi.RawConfig, reg pluginapi.ServiceResolver) error {
	cfg, err := parseConfig(rawCfg)
	if err != nil {
		return err
	}

	var recorder any
	if reg != nil {
		recorder, err = reg.Resolve("payment_recorder")
		if err != nil {
			return fmt.Errorf("payment_platega: resolve payment_recorder: %w", err)
		}
		if recorder == nil {
			return errors.New("payment_platega: payment_recorder is nil")
		}
	}

	p.mu.Lock()
	p.cfg = cfg
	p.recorder = recorder
	p.initialized = true
	p.mu.Unlock()

	if reg != nil && reg.Logger() != nil {
		reg.Logger().Info("payment_platega: initialised", "merchant_id_configured", cfg.MerchantID != "")
	}
	return nil
}

// PublishedServices exposes this concrete provider by a method-qualified
// service name. Consumers selecting a method dynamically should prefer
// pluginhost.Host.PaymentProviders().
func (p *Plugin) PublishedServices() map[string]any {
	if !p.isInitialized() {
		return nil
	}
	return map[string]any{ServiceName: p}
}

// Start has no background work; it keeps lifecycle behaviour uniform with
// other plugins and returns promptly after host cancellation.
func (p *Plugin) Start(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

func (p *Plugin) Stop(_ context.Context) error { return nil }

func (p *Plugin) Health(_ context.Context) error {
	if !p.isInitialized() {
		return errors.New("payment_platega: plugin is not initialized")
	}
	return nil
}

// MethodID is the payment method persisted in domain.Payment.Method.
func (p *Plugin) MethodID() string { return "platega" }

// CreateIntent opens a payment transaction through Platega. The core caller is
// responsible for persisting the returned ExternalID on its payment record.
func (p *Plugin) CreateIntent(ctx context.Context, req pluginapi.PaymentIntentRequest) (*pluginapi.PaymentIntentResult, error) {
	cfg, gateway, err := p.ready()
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.UserID) == "" {
		return nil, errors.New("payment_platega: user_id is required")
	}
	if req.Amount <= 0 {
		return nil, errors.New("payment_platega: amount must be positive")
	}

	currency := strings.ToUpper(strings.TrimSpace(req.Currency))
	if currency == "" {
		currency = cfg.Currency
	}
	if currency != "RUB" {
		return nil, fmt.Errorf("payment_platega: legacy gateway supports RUB only, got %q", currency)
	}

	returnURL := cfg.ReturnURL
	failedURL := cfg.FailedURL
	userName := ""
	if req.CustomData != nil {
		if value, ok := req.CustomData["return_url"].(string); ok && strings.TrimSpace(value) != "" {
			returnURL = value
		}
		if value, ok := req.CustomData["failed_url"].(string); ok && strings.TrimSpace(value) != "" {
			failedURL = value
		}
		if value, ok := req.CustomData["user_name"].(string); ok {
			userName = value
		}
	}
	if failedURL == "" {
		failedURL = returnURL
	}

	description := strings.TrimSpace(req.Description)
	if description == "" {
		description = "TorvaldsVPN payment"
	}

	result, err := gateway.CreatePayment(ctx, GatewayPaymentRequest{
		MerchantID:  cfg.MerchantID,
		Secret:      cfg.Secret,
		UserID:      req.UserID,
		UserName:    userName,
		Amount:      req.Amount,
		Currency:    currency,
		Description: description,
		ReturnURL:   returnURL,
		FailedURL:   failedURL,
		ExternalRef: req.ExternalRef,
	})
	if err != nil {
		return nil, fmt.Errorf("payment_platega: create intent: %w", err)
	}
	if strings.TrimSpace(result.ExternalID) == "" || strings.TrimSpace(result.PaymentURL) == "" {
		return nil, errors.New("payment_platega: gateway returned an empty transaction ID or payment URL")
	}

	raw := cloneMap(result.RawResponse)
	if raw == nil {
		raw = make(map[string]any)
	}
	// The old API generates its own payload/order ID, so ExternalRef cannot be
	// sent upstream without changing that client. Keep it visible to callers for
	// correlation until a richer SDK adapter is installed.
	if req.ExternalRef != "" {
		raw["external_ref"] = req.ExternalRef
	}

	return &pluginapi.PaymentIntentResult{
		ExternalID:  result.ExternalID,
		PaymentURL:  result.PaymentURL,
		RawResponse: raw,
	}, nil
}

// VerifyCallback authenticates the Platega X-Secret header and normalizes its
// JSON callback to the provider-neutral result consumed by the core service.
func (p *Plugin) VerifyCallback(ctx context.Context, r *http.Request) (*pluginapi.PaymentCallbackResult, error) {
	cfg, _, err := p.ready()
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r == nil || r.Body == nil {
		return nil, errors.New("payment_platega: callback request body is required")
	}

	body, err := readCallbackBody(r.Body)
	if err != nil {
		return nil, err
	}

	// Parse before authenticating to preserve the legacy endpoint's useful
	// malformed-payload response (400) while still performing a constant-time
	// authentication check before any callback data is acted upon.
	secret := r.Header.Get("X-Secret")
	if secret == "" || subtle.ConstantTimeCompare([]byte(secret), []byte(cfg.Secret)) != 1 {
		return nil, errors.New("payment_platega: invalid callback secret")
	}

	externalID := callbackString(body, "id", "transactionId", "transaction_id", "external_id")
	if externalID == "" {
		return nil, errors.New("payment_platega: callback transaction ID is required")
	}
	status := callbackString(body, "status")
	if status == "" {
		return nil, errors.New("payment_platega: callback status is required")
	}

	return &pluginapi.PaymentCallbackResult{
		ExternalID: externalID,
		Status:     normalizeStatus(status),
		Amount:     callbackAmount(body),
		Currency:   callbackCurrency(body, cfg.Currency),
		CustomData: body,
	}, nil
}

// Refund delegates to the configured gateway. The built-in legacy adapter
// returns ErrRefundUnsupported because the old Platega client has no refund API.
func (p *Plugin) Refund(ctx context.Context, externalID string, amount int) error {
	_, gateway, err := p.ready()
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(externalID) == "" {
		return errors.New("payment_platega: external_id is required")
	}
	if amount <= 0 {
		return errors.New("payment_platega: refund amount must be positive")
	}
	if err := gateway.Refund(ctx, externalID, amount); err != nil {
		return fmt.Errorf("payment_platega: refund: %w", err)
	}
	return nil
}

func (p *Plugin) ready() (Config, Gateway, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if !p.initialized {
		return Config{}, nil, errors.New("payment_platega: plugin is not initialized")
	}
	if p.gateway == nil {
		return Config{}, nil, errors.New("payment_platega: gateway is not configured")
	}
	return p.cfg, p.gateway, nil
}

func (p *Plugin) isInitialized() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.initialized
}

func parseConfig(raw pluginapi.RawConfig) (Config, error) {
	cfg := Config{Currency: "RUB"}
	if raw == nil {
		return cfg, errors.New("payment_platega: merchant_id is required in plugin config")
	}

	var err error
	if cfg.MerchantID, err = configString(raw, "merchant_id"); err != nil {
		return Config{}, err
	}
	if cfg.Secret, err = configString(raw, "secret"); err != nil {
		return Config{}, err
	}
	if cfg.ReturnURL, err = configString(raw, "return_url"); err != nil {
		return Config{}, err
	}
	if cfg.FailedURL, err = configString(raw, "failed_url"); err != nil {
		return Config{}, err
	}
	if currency, err := configString(raw, "currency"); err != nil {
		return Config{}, err
	} else if strings.TrimSpace(currency) != "" {
		cfg.Currency = strings.ToUpper(strings.TrimSpace(currency))
	}

	if strings.TrimSpace(cfg.MerchantID) == "" {
		return Config{}, errors.New("payment_platega: merchant_id is required in plugin config")
	}
	if strings.TrimSpace(cfg.Secret) == "" {
		return Config{}, errors.New("payment_platega: secret is required in plugin config")
	}
	if cfg.Currency != "RUB" {
		return Config{}, fmt.Errorf("payment_platega: legacy gateway supports RUB only, got %q", cfg.Currency)
	}
	return cfg, nil
}

func configString(raw pluginapi.RawConfig, key string) (string, error) {
	value, exists := raw[key]
	if !exists || value == nil {
		return "", nil
	}
	stringValue, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("payment_platega: config %q must be a string, got %T", key, value)
	}
	return stringValue, nil
}

func readCallbackBody(body io.Reader) (map[string]any, error) {
	raw, err := io.ReadAll(io.LimitReader(body, maxCallbackSize+1))
	if err != nil {
		return nil, fmt.Errorf("payment_platega: read callback: %w", err)
	}
	if len(raw) > maxCallbackSize {
		return nil, errors.New("payment_platega: callback body exceeds 1 MiB")
	}

	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("payment_platega: invalid callback JSON: %w", err)
	}
	if result == nil {
		return nil, errors.New("payment_platega: callback JSON must be an object")
	}
	return result, nil
}

func callbackString(body map[string]any, keys ...string) string {
	for _, source := range callbackSources(body) {
		for _, key := range keys {
			if value, ok := source[key].(string); ok && strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		}
	}
	return ""
}

func callbackAmount(body map[string]any) int {
	for _, source := range callbackSources(body) {
		if amount, ok := numberAsInt(source["amount"]); ok {
			return amount
		}
	}
	return 0
}

func callbackCurrency(body map[string]any, fallback string) string {
	for _, source := range callbackSources(body) {
		if currency, ok := source["currency"].(string); ok && strings.TrimSpace(currency) != "" {
			return strings.ToUpper(strings.TrimSpace(currency))
		}
	}
	return fallback
}

func callbackSources(body map[string]any) []map[string]any {
	sources := []map[string]any{body}
	for _, key := range []string{"data", "paymentDetails", "payment_details"} {
		if nested, ok := body[key].(map[string]any); ok {
			sources = append(sources, nested)
			if details, ok := nested["paymentDetails"].(map[string]any); ok {
				sources = append(sources, details)
			}
		}
	}
	return sources
}

func numberAsInt(value any) (int, bool) {
	switch value := value.(type) {
	case float64:
		return int(value), value == float64(int(value))
	case float32:
		return int(value), value == float32(int(value))
	case int:
		return value, true
	case int64:
		return int(value), int64(int(value)) == value
	case json.Number:
		parsed, err := strconv.Atoi(string(value))
		return parsed, err == nil
	case string:
		parsed, err := strconv.Atoi(value)
		return parsed, err == nil
	default:
		return 0, false
	}
}

func normalizeStatus(value string) string {
	status := strings.ToLower(strings.TrimSpace(value))
	switch status {
	case "success", "confirmed", "completed":
		return "completed"
	case "refund", "refunded":
		return "refunded"
	case "failure", "failed", "cancelled", "canceled", "declined", "rejected", "error":
		return "failed"
	default:
		// Preserve non-terminal statuses such as "pending" for compatibility
		// with the previous payment.Service status mapping.
		return status
	}
}

func cloneMap(input map[string]any) map[string]any {
	if input == nil {
		return nil
	}
	result := make(map[string]any, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}

var _ pluginapi.Plugin = (*Plugin)(nil)
var _ pluginapi.PaymentProvider = (*Plugin)(nil)
var _ pluginapi.ServiceProvider = (*Plugin)(nil)
