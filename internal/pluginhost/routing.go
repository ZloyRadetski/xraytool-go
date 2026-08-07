package pluginhost

import (
	"fmt"
	"sort"
	"strings"

	"xraytool/internal/pluginapi"
)

// RoutingMode is the declarative engines.routing_mode value. It intentionally
// lives next to MultiEngine rather than appconfig so other composition roots
// (tests, embedded deployments, and future external hosts) use exactly the
// same routing semantics.
type RoutingMode string

const (
	RoutingModeBroadcast              RoutingMode = "broadcast"
	RoutingModeByPlan                 RoutingMode = "by-plan"
	RoutingModeBySubscriptionOverride RoutingMode = "by-subscription-override"
)

// CheckedEngineRouter is an optional stronger form of EngineRouter. The
// original pluginapi.EngineRouter is intentionally error-free for simple
// custom routers; this interface lets the built-in declarative router reject a
// typo such as engine_ids: [sing-box] instead of silently provisioning nobody.
type CheckedEngineRouter interface {
	EngineRouter
	EnginesForChecked(pluginapi.VPNUserConfig) ([]pluginapi.EngineProvider, error)
}

// ConfiguredEngineRouter implements all built-in routing modes.
//
// Selection precedence is intentionally stable and safe:
//
//  1. SubscriptionEngineIDs is an explicit administrator override and always
//     wins when present.
//  2. by-plan selects PlanEngineIDs when the plan supplies a non-empty set.
//  3. Missing routing data falls back to all enabled engines, preserving the
//     historic one-engine/broadcast behaviour while old subscriptions migrate.
//
// The router retains provider order only as a convenience; MultiEngine also
// canonicalises its result to its own configured engine order.
type ConfiguredEngineRouter struct {
	mode      RoutingMode
	providers []pluginapi.EngineProvider
	byID      map[string]pluginapi.EngineProvider
}

// NewConfiguredEngineRouter builds a router for the supplied enabled engine
// providers. mode may be empty and then means the backwards-compatible
// broadcast mode.
func NewConfiguredEngineRouter(mode string, providers []pluginapi.EngineProvider) (*ConfiguredEngineRouter, error) {
	normalizedMode := RoutingMode(strings.TrimSpace(mode))
	if normalizedMode == "" {
		normalizedMode = RoutingModeBroadcast
	}
	switch normalizedMode {
	case RoutingModeBroadcast, RoutingModeByPlan, RoutingModeBySubscriptionOverride:
	default:
		return nil, fmt.Errorf("pluginhost: unsupported engine routing mode %q", mode)
	}

	router := &ConfiguredEngineRouter{
		mode:      normalizedMode,
		providers: append([]pluginapi.EngineProvider(nil), providers...),
		byID:      make(map[string]pluginapi.EngineProvider, len(providers)),
	}
	for index, provider := range providers {
		if isNilService(provider) {
			return nil, fmt.Errorf("pluginhost: routing provider at index %d is nil", index)
		}
		id := normalizeEngineID(provider.ID())
		if id == "" {
			return nil, fmt.Errorf("pluginhost: routing provider at index %d has an empty ID", index)
		}
		if _, exists := router.byID[id]; exists {
			return nil, fmt.Errorf("pluginhost: duplicate routing provider ID %q", id)
		}
		router.byID[id] = provider
	}
	return router, nil
}

// Mode returns the configured routing mode.
func (r *ConfiguredEngineRouter) Mode() RoutingMode {
	if r == nil {
		return RoutingModeBroadcast
	}
	return r.mode
}

// EnginesFor implements the legacy no-error router contract. MultiEngine
// detects ConfiguredEngineRouter through CheckedEngineRouter and uses the
// checked variant, so this method is primarily useful to callers inspecting a
// router outside a mutation path.
func (r *ConfiguredEngineRouter) EnginesFor(user pluginapi.VPNUserConfig) []pluginapi.EngineProvider {
	providers, _ := r.EnginesForChecked(user)
	return providers
}

// EnginesForChecked chooses target engines and reports unknown requested IDs.
func (r *ConfiguredEngineRouter) EnginesForChecked(user pluginapi.VPNUserConfig) ([]pluginapi.EngineProvider, error) {
	if r == nil {
		return nil, fmt.Errorf("pluginhost: configured engine router is nil")
	}

	// An explicit subscription override applies above every base mode. This is
	// what makes an administrator migration possible without changing a plan
	// or globally changing engines.routing_mode.
	if ids := normalizeEngineIDs(user.SubscriptionEngineIDs); len(ids) > 0 {
		return r.selectProviders(ids, user.Email, "subscription override")
	}

	switch r.mode {
	case RoutingModeByPlan:
		if ids := normalizeEngineIDs(user.PlanEngineIDs); len(ids) > 0 {
			return r.selectProviders(ids, user.Email, "plan")
		}
	case RoutingModeBySubscriptionOverride:
		// This mode is normally chosen when per-subscription overrides are
		// used. A plan snapshot remains a useful fallback for subscriptions
		// created before an administrator override was set.
		if ids := normalizeEngineIDs(user.PlanEngineIDs); len(ids) > 0 {
			return r.selectProviders(ids, user.Email, "plan fallback")
		}
	}
	return append([]pluginapi.EngineProvider(nil), r.providers...), nil
}

func (r *ConfiguredEngineRouter) selectProviders(ids []string, email, source string) ([]pluginapi.EngineProvider, error) {
	selected := make(map[string]struct{}, len(ids))
	var unknown []string
	for _, id := range ids {
		if _, exists := r.byID[id]; !exists {
			unknown = append(unknown, id)
			continue
		}
		selected[id] = struct{}{}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, fmt.Errorf("pluginhost: engine routing %s for user %q references unloaded engine IDs: %s", source, email, strings.Join(unknown, ", "))
	}

	providers := make([]pluginapi.EngineProvider, 0, len(selected))
	for _, provider := range r.providers {
		if _, ok := selected[normalizeEngineID(provider.ID())]; ok {
			providers = append(providers, provider)
		}
	}
	return providers, nil
}

func normalizeEngineIDs(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	result := make([]string, 0, len(ids))
	for _, id := range ids {
		id = normalizeEngineID(id)
		if id == "" {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		result = append(result, id)
	}
	return result
}

func normalizeEngineID(id string) string {
	return strings.ToLower(strings.TrimSpace(id))
}

var _ CheckedEngineRouter = (*ConfiguredEngineRouter)(nil)
