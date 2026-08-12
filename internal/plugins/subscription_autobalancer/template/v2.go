// Package template is the private compiler implementation of the
// subscription_autobalancer plugin. The source format deliberately stays
// separate from the JSON delivered to clients: its v2-specific fields never
// reach Xray.
package template

import (
	"crypto/sha256"
	stdjson "encoding/json"
	"fmt"
	"net"
	"regexp"
	"strings"
)

const Version = 2

var identifierRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

// Result is the fully compiled client-facing form of a subscription template.
// JSON is an array of native Xray configuration profiles.
type Result struct {
	IsV2          bool
	JSON          string
	ExportJSON    string
	ProfileCount  int
	BalancerCount int
}

type document struct {
	Version      int                `json:"version"`
	Servers      map[string]server  `json:"servers"`
	Subscription []subscriptionItem `json:"subscription"`
}

type server struct {
	ID          string             `json:"id"`
	Name        string             `json:"name"`
	Outbound    stdjson.RawMessage `json:"outbound"`
	Config      stdjson.RawMessage `json:"config"`
	OutboundTag string             `json:"outbound_tag"`
}

type subscriptionItem struct {
	Type     string           `json:"type"`
	Ref      string           `json:"ref"`
	ID       string           `json:"id"`
	Name     string           `json:"name"`
	Members  []member         `json:"members"`
	Probe    probe            `json:"probe"`
	Strategy balancerStrategy `json:"strategy"`
	Fallback string           `json:"fallback"`
}

type member struct {
	Ref    string  `json:"ref"`
	Server *server `json:"server"`
}

type probe struct {
	URL      string `json:"url"`
	Interval string `json:"interval"`
	Timeout  string `json:"timeout"`
	Sampling int    `json:"sampling"`
}

type balancerStrategy struct {
	Type      string   `json:"type"`
	Baselines []string `json:"baselines"`
	Expected  int      `json:"expected"`
	MaxRTT    string   `json:"max_rtt"`
	Tolerance *float64 `json:"tolerance"`
}

// Compile converts a v2 source document to the ordinary Xray profile array
// that clients consume.  Non-v2 JSON is intentionally returned untouched so
// legacy templates remain fully compatible.
func Compile(input string) (Result, error) {
	if !strings.HasPrefix(strings.TrimSpace(input), "{") {
		return Result{}, nil
	}
	var discriminator struct {
		Version      int                `json:"version"`
		Servers      stdjson.RawMessage `json:"servers"`
		Subscription stdjson.RawMessage `json:"subscription"`
	}
	if err := stdjson.Unmarshal([]byte(input), &discriminator); err != nil {
		return Result{}, fmt.Errorf("parse subscription JSON: %w", err)
	}
	if discriminator.Version != Version || (len(discriminator.Servers) == 0 && len(discriminator.Subscription) == 0) {
		return Result{}, nil
	}

	var doc document
	if err := stdjson.Unmarshal([]byte(input), &doc); err != nil {
		return Result{}, fmt.Errorf("parse subscription template v%d: %w", Version, err)
	}
	if len(doc.Subscription) == 0 {
		return Result{}, fmt.Errorf("subscription template v%d: subscription must not be empty", Version)
	}

	servers := make(map[string]server, len(doc.Servers))
	for id, value := range doc.Servers {
		if err := validateIdentifier("servers."+id, id); err != nil {
			return Result{}, err
		}
		if value.ID != "" && value.ID != id {
			return Result{}, fmt.Errorf("servers.%s.id must be omitted or equal to %q", id, id)
		}
		value.ID = id
		if err := validateServer("servers."+id, value); err != nil {
			return Result{}, err
		}
		servers[id] = value
	}

	profiles := make([]map[string]any, 0, len(doc.Subscription))
	exportProfiles := make([]map[string]any, 0, len(doc.Subscription))
	externalMembers := make([]server, 0)
	usedSubscriptionServers := make(map[string]struct{})
	balancerIDs := make(map[string]struct{})
	balancers := 0

	for index, item := range doc.Subscription {
		path := fmt.Sprintf("subscription[%d]", index)
		switch item.Type {
		case "server":
			if item.Ref == "" {
				return Result{}, fmt.Errorf("%s.ref is required for a server item", path)
			}
			if item.ID != "" || item.Name != "" || len(item.Members) != 0 {
				return Result{}, fmt.Errorf("%s server item only supports type and ref", path)
			}
			value, ok := servers[item.Ref]
			if !ok {
				return Result{}, fmt.Errorf("%s.ref references unknown server %q", path, item.Ref)
			}
			if _, exists := usedSubscriptionServers[item.Ref]; exists {
				return Result{}, fmt.Errorf("%s.ref duplicates subscription server %q", path, item.Ref)
			}
			usedSubscriptionServers[item.Ref] = struct{}{}
			profile, err := profileForServer(value)
			if err != nil {
				return Result{}, fmt.Errorf("%s: %w", path, err)
			}
			profiles = append(profiles, profile)
			exportProfiles = append(exportProfiles, profile)

		case "auto_balancer":
			if item.Ref != "" {
				return Result{}, fmt.Errorf("%s auto_balancer must not have ref", path)
			}
			if err := validateIdentifier(path+".id", item.ID); err != nil {
				return Result{}, err
			}
			if _, exists := balancerIDs[item.ID]; exists {
				return Result{}, fmt.Errorf("%s.id duplicates auto balancer %q", path, item.ID)
			}
			balancerIDs[item.ID] = struct{}{}
			profile, members, err := compileBalancer(path, item, servers)
			if err != nil {
				return Result{}, err
			}
			profiles = append(profiles, profile)
			externalMembers = append(externalMembers, members...)
			balancers++

		default:
			return Result{}, fmt.Errorf("%s.type must be \"server\" or \"auto_balancer\", got %q", path, item.Type)
		}
	}

	encoded, err := stdjson.MarshalIndent(profiles, "", "  ")
	if err != nil {
		return Result{}, fmt.Errorf("encode compiled subscription template: %w", err)
	}
	seenEndpoints := make(map[string]struct{})
	for _, profile := range exportProfiles {
		for _, endpoint := range profileProxyOutbounds(profile) {
			if fingerprint, err := EndpointFingerprint(endpoint); err == nil {
				seenEndpoints[fingerprint] = struct{}{}
			}
		}
	}
	for _, value := range externalMembers {
		if _, isRegularProfile := usedSubscriptionServers[value.ID]; isRegularProfile {
			continue
		}
		endpoint, err := endpointForServer("auto-balancer member "+value.ID, value)
		if err != nil {
			return Result{}, err
		}
		fingerprint, err := EndpointFingerprint(endpoint)
		if err == nil {
			if _, exists := seenEndpoints[fingerprint]; exists {
				continue
			}
			seenEndpoints[fingerprint] = struct{}{}
		}
		exportProfiles = append(exportProfiles, map[string]any{
			"remarks":   value.Name,
			"outbounds": []any{endpoint},
		})
	}
	exportEncoded, err := stdjson.MarshalIndent(exportProfiles, "", "  ")
	if err != nil {
		return Result{}, fmt.Errorf("encode portable subscription endpoints: %w", err)
	}
	return Result{
		IsV2:          true,
		JSON:          string(encoded),
		ExportJSON:    string(exportEncoded),
		ProfileCount:  len(profiles),
		BalancerCount: balancers,
	}, nil
}

func validateIdentifier(path, value string) error {
	if !identifierRE.MatchString(value) {
		return fmt.Errorf("%s must match %s", path, identifierRE.String())
	}
	return nil
}

func validateServer(path string, value server) error {
	if strings.TrimSpace(value.Name) == "" {
		return fmt.Errorf("%s.name is required", path)
	}
	if len(value.Outbound) != 0 && len(value.Config) != 0 {
		return fmt.Errorf("%s must contain either outbound or config, not both", path)
	}
	if len(value.Outbound) == 0 && len(value.Config) == 0 {
		return fmt.Errorf("%s.outbound or %s.config is required", path, path)
	}
	if len(value.Outbound) != 0 {
		_, err := decodeProxyOutbound(path+".outbound", value.Outbound)
		return err
	}

	profile, err := decodeObject(path+".config", value.Config)
	if err != nil {
		return err
	}
	_, err = selectedProxyOutbound(path+".config", profile, value.OutboundTag)
	return err
}

func profileForServer(value server) (map[string]any, error) {
	if len(value.Config) != 0 {
		profile, err := decodeObject("server "+value.ID+" config", value.Config)
		if err != nil {
			return nil, err
		}
		profile["remarks"] = value.Name
		return profile, nil
	}
	outbound, err := decodeProxyOutbound("server "+value.ID+" outbound", value.Outbound)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"remarks":   value.Name,
		"outbounds": []any{outbound},
	}, nil
}

func compileBalancer(path string, item subscriptionItem, servers map[string]server) (map[string]any, []server, error) {
	if strings.TrimSpace(item.Name) == "" {
		return nil, nil, fmt.Errorf("%s.name is required", path)
	}
	if len(item.Members) < 2 {
		return nil, nil, fmt.Errorf("%s.members must contain at least two servers", path)
	}
	if item.Fallback != "" && item.Fallback != "direct" {
		return nil, nil, fmt.Errorf("%s.fallback currently supports only \"direct\"", path)
	}

	memberIDs := make(map[string]struct{}, len(item.Members))
	memberTags := make([]string, 0, len(item.Members))
	resolvedMembers := make([]server, 0, len(item.Members))
	outbounds := make([]any, 0, len(item.Members)+4)
	for index, itemMember := range item.Members {
		memberPath := fmt.Sprintf("%s.members[%d]", path, index)
		value, err := resolveMember(memberPath, itemMember, servers)
		if err != nil {
			return nil, nil, err
		}
		if _, exists := memberIDs[value.ID]; exists {
			return nil, nil, fmt.Errorf("%s duplicates server %q", memberPath, value.ID)
		}
		memberIDs[value.ID] = struct{}{}

		outbound, err := endpointForServer(memberPath, value)
		if err != nil {
			return nil, nil, err
		}
		tag := generatedTag(item.ID, value.ID, index)
		outbound["tag"] = tag
		memberTags = append(memberTags, tag)
		outbounds = append(outbounds, outbound)
		resolvedMembers = append(resolvedMembers, value)
	}

	probe := item.Probe
	if probe.URL == "" {
		probe.URL = "https://www.gstatic.com/generate_204"
	}
	if probe.Interval == "" {
		probe.Interval = "1m"
	}
	if probe.Timeout == "" {
		probe.Timeout = "3s"
	}
	if probe.Sampling <= 0 {
		probe.Sampling = 1
	}

	strategy, err := compiledStrategy(path+".strategy", item.Strategy)
	if err != nil {
		return nil, nil, err
	}
	profile, err := baseProfileForBalancer(path, resolvedMembers)
	if err != nil {
		return nil, nil, err
	}
	balancerTag := "autobalancer_" + item.ID
	for _, outbound := range supportingOutbounds(profile) {
		outbounds = append(outbounds, outbound)
	}
	if !hasOutboundTag(outbounds, "direct") {
		outbounds = append(outbounds, map[string]any{"protocol": "freedom", "tag": "direct"})
	}
	routing, err := routingForBalancer(path, profile, balancerTag, memberTags, strategy)
	if err != nil {
		return nil, nil, err
	}
	ensureFullConfigCompatibility(profile, routing)
	// Auto-balancers must be complete Xray client configurations.  In
	// particular, clients such as INCY recognize the profile as a full
	// configuration only when both inbounds and outbounds are present.  Reuse
	// the first member's client settings where possible so DNS, policy and
	// inbounds stay aligned with the source server profile.
	profile["remarks"] = item.Name
	profile["burstObservatory"] = map[string]any{
		"subjectSelector": memberTags,
		"pingConfig": map[string]any{
			"destination": probe.URL,
			"interval":    probe.Interval,
			"timeout":     probe.Timeout,
			"sampling":    probe.Sampling,
		},
	}
	profile["outbounds"] = outbounds
	profile["routing"] = routing
	return profile, resolvedMembers, nil
}

// ensureFullConfigCompatibility adds the small pieces of a complete Xray
// profile that INCY otherwise patches at import time. Keeping them in the
// subscription makes the balancer work in clients that pass full configs to
// Xray unchanged, including Happ.
func ensureFullConfigCompatibility(profile map[string]any, routing map[string]any) {
	if _, exists := profile["stats"]; !exists {
		profile["stats"] = map[string]any{}
	}

	dnsIPs := dnsServerIPs(profile)
	if len(dnsIPs) == 0 || hasDirectDNSRule(routing, dnsIPs) {
		return
	}
	rules, _ := routing["rules"].([]any)
	directDNSRule := map[string]any{
		"type":        "field",
		"ip":          dnsIPs,
		"outboundTag": "direct",
	}
	routing["rules"] = append([]any{directDNSRule}, rules...)
}

func dnsServerIPs(profile map[string]any) []string {
	dns, _ := profile["dns"].(map[string]any)
	servers, _ := dns["servers"].([]any)
	seen := make(map[string]struct{}, len(servers))
	result := make([]string, 0, len(servers))
	for _, server := range servers {
		address := ""
		switch value := server.(type) {
		case string:
			address = value
		case map[string]any:
			address, _ = value["address"].(string)
		}
		if ip := net.ParseIP(strings.TrimSpace(address)); ip != nil {
			canonical := ip.String()
			if _, exists := seen[canonical]; !exists {
				seen[canonical] = struct{}{}
				result = append(result, canonical)
			}
		}
	}
	return result
}

func hasDirectDNSRule(routing map[string]any, dnsIPs []string) bool {
	wanted := make(map[string]struct{}, len(dnsIPs))
	for _, ip := range dnsIPs {
		wanted[ip] = struct{}{}
	}
	rawRules, _ := routing["rules"].([]any)
	for _, rawRule := range rawRules {
		rule, ok := rawRule.(map[string]any)
		if !ok || rule["outboundTag"] != "direct" {
			continue
		}
		rawIPs, ok := rule["ip"].([]any)
		if !ok {
			// A regular direct rule (for example, geosite:private) is not a
			// DNS bypass rule. Keep looking instead of assuming every direct
			// rule has an ip list.
			continue
		}
		for _, rawIP := range rawIPs {
			ip, _ := rawIP.(string)
			delete(wanted, ip)
		}
		if len(wanted) == 0 {
			return true
		}
	}
	return false
}

// baseProfileForBalancer produces a full Xray configuration for an
// auto-balancer.  Referenced servers usually carry a complete config, which
// lets the balancer preserve the source DNS, policy and inbound definitions.
// Inline outbounds have no such base, so use the standard client inbounds.
func baseProfileForBalancer(path string, members []server) (map[string]any, error) {
	for _, value := range members {
		if len(value.Config) == 0 {
			continue
		}
		profile, err := decodeObject(path+" base config for member "+value.ID, value.Config)
		if err != nil {
			return nil, err
		}
		if !hasInbounds(profile) {
			profile["inbounds"] = defaultBalancerInbounds()
		}
		return profile, nil
	}
	return map[string]any{"inbounds": defaultBalancerInbounds()}, nil
}

func hasInbounds(profile map[string]any) bool {
	inbounds, ok := profile["inbounds"].([]any)
	return ok && len(inbounds) > 0
}

func defaultBalancerInbounds() []any {
	return []any{
		map[string]any{
			"listen":   "127.0.0.1",
			"port":     10808,
			"protocol": "socks",
			"settings": map[string]any{
				"auth": "noauth",
				"udp":  true,
			},
			"tag": "socks-in",
		},
		map[string]any{
			"listen":   "127.0.0.1",
			"port":     10809,
			"protocol": "http",
			"settings": map[string]any{
				"auth": "noauth",
				"udp":  true,
			},
			"tag": "http-in",
		},
	}
}

// supportingOutbounds retains the direct, block and DNS outbounds from the
// base profile.  Routing rules may refer to those tags, while the source
// proxy outbound is deliberately replaced by the isolated balancer members.
func supportingOutbounds(profile map[string]any) []any {
	rawOutbounds, _ := profile["outbounds"].([]any)
	result := make([]any, 0, len(rawOutbounds))
	for _, raw := range rawOutbounds {
		outbound, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		protocol, _ := outbound["protocol"].(string)
		if !isNonProxyProtocol(protocol) {
			continue
		}
		copy, err := cloneObject(outbound)
		if err == nil {
			result = append(result, copy)
		}
	}
	return result
}

func hasOutboundTag(outbounds []any, tag string) bool {
	for _, raw := range outbounds {
		outbound, ok := raw.(map[string]any)
		if ok && outbound["tag"] == tag {
			return true
		}
	}
	return false
}

func routingForBalancer(path string, profile map[string]any, balancerTag string, memberTags []string, strategy map[string]any) (map[string]any, error) {
	routing := make(map[string]any)
	if raw, ok := profile["routing"].(map[string]any); ok {
		copy, err := cloneObject(raw)
		if err != nil {
			return nil, fmt.Errorf("%s base routing: %w", path, err)
		}
		routing = copy
	}

	rawRules, _ := routing["rules"].([]any)
	rules := make([]any, 0, len(rawRules)+1)
	hasBalancerRule := false
	for index, raw := range rawRules {
		rule, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s base routing.rules[%d] must be an object", path, index)
		}
		copy, err := cloneObject(rule)
		if err != nil {
			return nil, fmt.Errorf("%s base routing.rules[%d]: %w", path, index, err)
		}
		if outboundTag, ok := copy["outboundTag"].(string); ok && outboundTag != "direct" && outboundTag != "block" && outboundTag != "dns-out" {
			delete(copy, "outboundTag")
			copy["balancerTag"] = balancerTag
			hasBalancerRule = true
		} else if _, ok := copy["balancerTag"]; ok {
			copy["balancerTag"] = balancerTag
			hasBalancerRule = true
		}
		rules = append(rules, copy)
	}
	if !hasBalancerRule {
		rules = append(rules, map[string]any{
			"type":        "field",
			"network":     "tcp,udp",
			"balancerTag": balancerTag,
		})
	}
	routing["rules"] = rules
	routing["balancers"] = []any{map[string]any{
		"tag":         balancerTag,
		"selector":    memberTags,
		"fallbackTag": "direct",
		"strategy":    strategy,
	}}
	return routing, nil
}

func profileProxyOutbounds(profile map[string]any) []map[string]any {
	rawOutbounds, _ := profile["outbounds"].([]any)
	result := make([]map[string]any, 0, len(rawOutbounds))
	for _, rawOutbound := range rawOutbounds {
		outbound, ok := rawOutbound.(map[string]any)
		if !ok {
			continue
		}
		protocol, _ := outbound["protocol"].(string)
		if isNonProxyProtocol(protocol) {
			continue
		}
		result = append(result, outbound)
	}
	return result
}

func resolveMember(path string, item member, servers map[string]server) (server, error) {
	if item.Ref != "" && item.Server != nil {
		return server{}, fmt.Errorf("%s must contain ref or server, not both", path)
	}
	if item.Ref != "" {
		value, ok := servers[item.Ref]
		if !ok {
			return server{}, fmt.Errorf("%s.ref references unknown server %q", path, item.Ref)
		}
		return value, nil
	}
	if item.Server == nil {
		return server{}, fmt.Errorf("%s requires ref or server", path)
	}
	value := *item.Server
	if err := validateIdentifier(path+".server.id", value.ID); err != nil {
		return server{}, err
	}
	if err := validateServer(path+".server", value); err != nil {
		return server{}, err
	}
	return value, nil
}

func endpointForServer(path string, value server) (map[string]any, error) {
	if len(value.Outbound) != 0 {
		return decodeProxyOutbound(path+".outbound", value.Outbound)
	}
	profile, err := decodeObject(path+".config", value.Config)
	if err != nil {
		return nil, err
	}
	return selectedProxyOutbound(path+".config", profile, value.OutboundTag)
}

func decodeProxyOutbound(path string, raw stdjson.RawMessage) (map[string]any, error) {
	outbound, err := decodeObject(path, raw)
	if err != nil {
		return nil, err
	}
	protocol, _ := outbound["protocol"].(string)
	if isNonProxyProtocol(protocol) || strings.TrimSpace(protocol) == "" {
		return nil, fmt.Errorf("%s must be a proxy outbound with a protocol", path)
	}
	return outbound, nil
}

func decodeObject(path string, raw stdjson.RawMessage) (map[string]any, error) {
	var value map[string]any
	if err := stdjson.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("%s must be a JSON object: %w", path, err)
	}
	if value == nil {
		return nil, fmt.Errorf("%s must be a JSON object", path)
	}
	return value, nil
}

func selectedProxyOutbound(path string, profile map[string]any, tag string) (map[string]any, error) {
	rawOutbounds, ok := profile["outbounds"].([]any)
	if !ok || len(rawOutbounds) == 0 {
		return nil, fmt.Errorf("%s.outbounds must contain a proxy outbound", path)
	}
	var candidates []map[string]any
	for _, raw := range rawOutbounds {
		outbound, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		protocol, _ := outbound["protocol"].(string)
		if isNonProxyProtocol(protocol) || strings.TrimSpace(protocol) == "" {
			continue
		}
		if tag != "" && outbound["tag"] != tag {
			continue
		}
		candidates = append(candidates, outbound)
	}
	if len(candidates) == 0 {
		if tag != "" {
			return nil, fmt.Errorf("%s.outbound_tag %q does not select a proxy outbound", path, tag)
		}
		return nil, fmt.Errorf("%s.outbounds must contain a proxy outbound", path)
	}
	if len(candidates) > 1 {
		return nil, fmt.Errorf("%s has multiple proxy outbounds; set outbound_tag", path)
	}
	return cloneObject(candidates[0])
}

func compiledStrategy(path string, value balancerStrategy) (map[string]any, error) {
	if value.Type == "" {
		value.Type = "leastPing"
	}
	switch value.Type {
	case "leastPing":
		// Xray uses the observation results directly and selects the member
		// with the lowest RTT. Strategy settings belong to leastLoad only.
		if len(value.Baselines) != 0 || value.Expected != 0 || value.MaxRTT != "" || value.Tolerance != nil {
			return nil, fmt.Errorf("%s leastPing does not support strategy settings", path)
		}
		return map[string]any{"type": value.Type}, nil
	case "leastLoad":
		// leastLoad remains available for templates that explicitly need
		// stability-based rather than minimum-latency selection.
	default:
		return nil, fmt.Errorf("%s.type must be \"leastPing\" or \"leastLoad\", got %q", path, value.Type)
	}
	if len(value.Baselines) == 0 {
		value.Baselines = []string{"1s"}
	}
	if value.Expected <= 0 {
		value.Expected = 2
	}
	if value.MaxRTT == "" {
		value.MaxRTT = "1s"
	}
	tolerance := 0.1
	if value.Tolerance != nil {
		if *value.Tolerance < 0 {
			return nil, fmt.Errorf("%s.tolerance must not be negative", path)
		}
		tolerance = *value.Tolerance
	}
	return map[string]any{
		"type": value.Type,
		"settings": map[string]any{
			"baselines": value.Baselines,
			"expected":  value.Expected,
			"maxRTT":    value.MaxRTT,
			"tolerance": tolerance,
		},
	}, nil
}

func generatedTag(balancerID, serverID string, index int) string {
	base := "ab_" + sanitizeTagPart(balancerID) + "_" + sanitizeTagPart(serverID)
	return fmt.Sprintf("%s_%d", base, index+1)
}

func sanitizeTagPart(value string) string {
	var builder strings.Builder
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') {
			builder.WriteRune(char)
		} else {
			builder.WriteByte('_')
		}
	}
	return builder.String()
}

func isNonProxyProtocol(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "freedom", "blackhole", "dns":
		return true
	default:
		return false
	}
}

func cloneObject(value map[string]any) (map[string]any, error) {
	encoded, err := stdjson.Marshal(value)
	if err != nil {
		return nil, err
	}
	var clone map[string]any
	if err := stdjson.Unmarshal(encoded, &clone); err != nil {
		return nil, err
	}
	return clone, nil
}

// EndpointFingerprint is the tag-independent identity used when a converter
// encounters an already compiled native Xray balancer.  It is exported for
// converter tests and deliberately retains every connection setting except the
// per-profile Xray tag.
func EndpointFingerprint(outbound map[string]any) (string, error) {
	copy, err := cloneObject(outbound)
	if err != nil {
		return "", err
	}
	delete(copy, "tag")
	encoded, err := stdjson.Marshal(copy)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return fmt.Sprintf("%x", sum[:]), nil
}
