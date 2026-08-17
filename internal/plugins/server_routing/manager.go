package server_routing

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	json "github.com/goccy/go-json"
	"xraytool/internal/appconfig"
	"xraytool/internal/pluginapi"
	"xraytool/internal/safeio"
)

type nopLogger struct{}

func (nopLogger) Debug(msg string, args ...any) {}
func (nopLogger) Info(msg string, args ...any)  {}
func (nopLogger) Warn(msg string, args ...any)  {}
func (nopLogger) Error(msg string, args ...any) {}

// Manager manages JSON-based routing files and outbounds templates on disk.
type Manager struct {
	routingDir string
	cfg        *appconfig.Config
	log        pluginapi.Logger
	restartFn  func(ctx context.Context) error
	mu         sync.RWMutex
}

// NewManager creates a new Manager instance.
func NewManager(routingDir string, cfg *appconfig.Config, log pluginapi.Logger) *Manager {
	if log == nil {
		log = nopLogger{}
	}
	return &Manager{
		routingDir: filepath.Clean(routingDir),
		cfg:        cfg,
		log:        log,
	}
}

// SetRestartFunc configures a custom service restart callback (primarily for testing).
func (m *Manager) SetRestartFunc(fn func(ctx context.Context) error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.restartFn = fn
}

// OutboundsDir returns the absolute path to the outbounds template directory.
func (m *Manager) OutboundsDir() string {
	if m == nil {
		return ""
	}
	return filepath.Join(m.routingDir, "outbounds")
}

// ServerConfigPath returns the file path for a specific server's routing config JSON.
func (m *Manager) ServerConfigPath(serverName string) string {
	if m == nil {
		return ""
	}
	return filepath.Join(m.routingDir, fmt.Sprintf("%s.json", serverName))
}

// OutboundTemplatePath returns the file path for an outbound template JSON.
func (m *Manager) OutboundTemplatePath(serverName string) string {
	if m == nil {
		return ""
	}
	return filepath.Join(m.OutboundsDir(), fmt.Sprintf("%s.json", serverName))
}

// MasterName returns the name identifier of the master node.
func (m *Manager) MasterName() string {
	if m != nil && m.cfg != nil {
		if strings.TrimSpace(m.cfg.Replication.NodeID) != "" {
			return strings.TrimSpace(m.cfg.Replication.NodeID)
		}
		if strings.TrimSpace(m.cfg.Server.Domain) != "" {
			return strings.TrimSpace(m.cfg.Server.Domain)
		}
	}
	return "master"
}

// LocalNodeName returns the identity name of this node (master domain/name or slave node_id).
func (m *Manager) LocalNodeName() string {
	if m != nil && m.cfg != nil {
		if m.cfg.Mode == "slave" && strings.TrimSpace(m.cfg.Replication.NodeID) != "" {
			return strings.TrimSpace(m.cfg.Replication.NodeID)
		}
		if strings.TrimSpace(m.cfg.Replication.NodeID) != "" {
			return strings.TrimSpace(m.cfg.Replication.NodeID)
		}
		if strings.TrimSpace(m.cfg.Server.Domain) != "" {
			return strings.TrimSpace(m.cfg.Server.Domain)
		}
	}
	return "master"
}

// GetKnownServers returns the full map of server names to ServerNode objects.
func (m *Manager) GetKnownServers() map[string]ServerNode {
	nodes := make(map[string]ServerNode)
	if m == nil {
		return nodes
	}

	// Master identity
	masterName := m.MasterName()
	masterDomain := ""
	masterIP := ""
	if m.cfg != nil {
		masterDomain = strings.TrimSpace(m.cfg.Server.Domain)
		masterIP = strings.TrimSpace(m.cfg.Server.IP)
	}
	nodes[masterName] = ServerNode{
		Name:     masterName,
		IsMaster: true,
		IP:       masterIP,
		Domain:   masterDomain,
		Online:   true,
	}

	// Self slave identity if in slave mode
	if m.cfg != nil && m.cfg.Mode == "slave" && strings.TrimSpace(m.cfg.Replication.NodeID) != "" {
		slaveID := strings.TrimSpace(m.cfg.Replication.NodeID)
		nodes[slaveID] = ServerNode{
			Name:     slaveID,
			IsMaster: false,
			Online:   true,
		}
	}

	// Slaves from Replication.AllowedNodes
	if m.cfg != nil {
		for _, rawNode := range m.cfg.Replication.AllowedNodes {
			nodeName := strings.TrimSpace(rawNode)
			if nodeName == "" || nodeName == masterName {
				continue
			}
			nodes[nodeName] = ServerNode{
				Name:     nodeName,
				IsMaster: false,
				IP:       "",
				Domain:   "",
				Online:   true,
			}
		}
	}

	// Always discover any additional servers from valid routing config files in routingDir
	if m.routingDir != "" {
		if entries, err := os.ReadDir(m.routingDir); err == nil {
			for _, e := range entries {
				if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
					data, readErr := os.ReadFile(filepath.Join(m.routingDir, e.Name()))
					if readErr == nil {
						var parsed ServerRoutingConfig
						if err := json.Unmarshal(data, &parsed); err == nil && parsed.Server != "" {
							if _, exists := nodes[parsed.Server]; !exists {
								nodes[parsed.Server] = ServerNode{
									Name:     parsed.Server,
									IsMaster: parsed.Server == masterName,
									Online:   true,
								}
							}
						}
					}
				}
			}
		}
	}

	// Also recognize servers that have outbound templates in OutboundsDir
	if m.routingDir != "" {
		if entries, err := os.ReadDir(m.OutboundsDir()); err == nil {
			for _, e := range entries {
				if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
					targetName := strings.TrimSuffix(e.Name(), ".json")
					if targetName != "" && targetName != "direct" && targetName != "block" {
						if _, exists := nodes[targetName]; !exists {
							nodes[targetName] = ServerNode{
								Name:     targetName,
								IsMaster: targetName == masterName,
								Online:   true,
							}
						}
					}
				}
			}
		}
	}

	return nodes
}

// LoadTopology returns the complete topology containing all known servers, special nodes,
// loaded routing rules per server, and available outbound templates.
func (m *Manager) LoadTopology(ctx context.Context) (*TopologyResponse, error) {
	if m == nil {
		return nil, fmt.Errorf("server_routing: manager is nil")
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	knownServers := m.GetKnownServers()

	serverList := make([]ServerNode, 0, len(knownServers))
	var masterNode *ServerNode
	var slaveNodes []ServerNode

	for _, srv := range knownServers {
		if srv.IsMaster {
			copyNode := srv
			masterNode = &copyNode
		} else {
			slaveNodes = append(slaveNodes, srv)
		}
	}

	sort.Slice(slaveNodes, func(i, j int) bool {
		return slaveNodes[i].Name < slaveNodes[j].Name
	})

	if masterNode != nil {
		serverList = append(serverList, *masterNode)
	}
	serverList = append(serverList, slaveNodes...)

	specialNodes := []SpecialNode{
		{Name: "direct", Type: "direct"},
		{Name: "block", Type: "block"},
		{Name: "ru-traffic-portal", Type: "portal"},
	}

	// Read all available outbound template names from outbounds directory
	var outbounds []string
	outboundsDir := m.OutboundsDir()
	if entries, err := os.ReadDir(outboundsDir); err == nil {
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
				outbounds = append(outbounds, strings.TrimSuffix(e.Name(), ".json"))
			}
		}
	}
	if outbounds == nil {
		outbounds = []string{}
	} else {
		sort.Strings(outbounds)
	}

	// Read routing rules for each server
	routingConfigs := make([]ServerRoutingConfig, 0, len(serverList))
	for _, srv := range serverList {
		cfgPath := m.ServerConfigPath(srv.Name)
		srvCfg := ServerRoutingConfig{
			Server: srv.Name,
			Rules:  []RoutingRule{},
		}
		if data, err := os.ReadFile(cfgPath); err == nil {
			var loaded ServerRoutingConfig
			if err := json.Unmarshal(data, &loaded); err == nil && loaded.Rules != nil {
				srvCfg.Rules = loaded.Rules
			}
		}
		// Sort rules by Priority ascending, then ID ascending for deterministic output
		sort.Slice(srvCfg.Rules, func(i, j int) bool {
			if srvCfg.Rules[i].Priority != srvCfg.Rules[j].Priority {
				return srvCfg.Rules[i].Priority < srvCfg.Rules[j].Priority
			}
			return srvCfg.Rules[i].ID < srvCfg.Rules[j].ID
		})
		routingConfigs = append(routingConfigs, srvCfg)
	}

	return &TopologyResponse{
		Servers:      serverList,
		SpecialNodes: specialNodes,
		Routing:      routingConfigs,
		Outbounds:    outbounds,
	}, nil
}

type edgeEntry struct {
	from    string
	to      string
	pattern string
}

func parseIPOrCIDR(s string) *net.IPNet {
	s = strings.TrimPrefix(s, "geoip:")
	s = strings.TrimPrefix(s, "ip:")
	if strings.Contains(s, "/") {
		_, ipNet, err := net.ParseCIDR(s)
		if err == nil {
			return ipNet
		}
	} else {
		ip := net.ParseIP(s)
		if ip != nil {
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			mask := net.CIDRMask(bits, bits)
			return &net.IPNet{IP: ip, Mask: mask}
		}
	}
	return nil
}

func stripDomainPrefix(s string) string {
	s = strings.TrimPrefix(s, "domain:")
	s = strings.TrimPrefix(s, "geosite:")
	s = strings.TrimPrefix(s, "full:")
	return s
}

// patternsOverlap returns true if pattern A and pattern B could match overlapping traffic.
func patternsOverlap(a, b string) bool {
	a = strings.ToLower(strings.TrimSpace(a))
	b = strings.ToLower(strings.TrimSpace(b))
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}

	// 1. Check IP / CIDR Subnet Overlap
	netA := parseIPOrCIDR(a)
	netB := parseIPOrCIDR(b)
	if netA != nil && netB != nil {
		if netA.Contains(netB.IP) || netB.Contains(netA.IP) {
			return true
		}
	}

	// 2. Check Domain / Subdomain Overlap
	domA := stripDomainPrefix(a)
	domB := stripDomainPrefix(b)
	if domA != "" && domB != "" {
		if domA == domB {
			return true
		}
		if strings.HasSuffix(domA, "."+domB) || strings.HasSuffix(domB, "."+domA) {
			return true
		}
	}

	return false
}

// detectRoutingCycles verifies that exact and overlapping traffic rules do not form closed routing loops.
func detectRoutingCycles(configs []ServerRoutingConfig) error {
	var edges []edgeEntry

	for _, cfg := range configs {
		for _, r := range cfg.Rules {
			if !r.Enabled {
				continue
			}
			target := strings.TrimSpace(r.TargetServer)
			if target == "" || strings.EqualFold(target, "direct") || strings.EqualFold(target, "block") || strings.EqualFold(target, "ru-traffic-portal") {
				continue
			}

			for _, d := range r.Domain {
				pat := strings.ToLower(strings.TrimSpace(d))
				if pat != "" {
					edges = append(edges, edgeEntry{from: cfg.Server, to: target, pattern: pat})
				}
			}

			for _, ip := range r.IP {
				pat := strings.ToLower(strings.TrimSpace(ip))
				if pat != "" {
					edges = append(edges, edgeEntry{from: cfg.Server, to: target, pattern: pat})
				}
			}
		}
	}

	if len(edges) == 0 {
		return nil
	}

	// For each starting edge, perform DFS along overlapping patterns to detect cycles
	for _, startEdge := range edges {
		visited := make(map[string]bool)
		path := []string{startEdge.from}

		var dfs func(currNode string, currPattern string) ([]string, string, bool)
		dfs = func(currNode string, currPattern string) ([]string, string, bool) {
			visited[currNode] = true

			for _, nextEdge := range edges {
				if nextEdge.from != currNode {
					continue
				}
				if !patternsOverlap(currPattern, nextEdge.pattern) {
					continue
				}

				if nextEdge.to == startEdge.from {
					return append(path, nextEdge.to), fmt.Sprintf("%s ~ %s", currPattern, nextEdge.pattern), true
				}

				if !visited[nextEdge.to] {
					path = append(path, nextEdge.to)
					if cPath, cPat, found := dfs(nextEdge.to, currPattern); found {
						return cPath, cPat, true
					}
					path = path[:len(path)-1]
				}
			}

			visited[currNode] = false
			return nil, "", false
		}

		if cyclePath, cyclePat, found := dfs(startEdge.to, startEdge.pattern); found {
			return &ValidationError{
				Message: fmt.Sprintf("routing cycle detected for overlapping pattern %s across servers: %s", cyclePat, strings.Join(cyclePath, " ➜ ")),
			}
		}
	}

	return nil
}

// ValidateRules verifies that all routing rules are well-formed and target existing destinations without cycles.
func (m *Manager) ValidateRules(configs []ServerRoutingConfig) error {
	if m == nil {
		return fmt.Errorf("server_routing: manager is nil")
	}

	knownServers := m.GetKnownServers()
	seenIDs := make(map[string]bool)
	seenServers := make(map[string]bool)

	for configIdx, srvCfg := range configs {
		srvName := strings.TrimSpace(srvCfg.Server)
		if srvName == "" {
			return &ValidationError{Message: fmt.Sprintf("config #%d is missing a server name", configIdx+1)}
		}
		if !utf8.ValidString(srvName) {
			return &ValidationError{Message: fmt.Sprintf("server name at index #%d contains invalid UTF-8", configIdx+1)}
		}
		if len(srvName) > 256 {
			return &ValidationError{Message: fmt.Sprintf("server name %q exceeds max length of 256 bytes", srvName)}
		}
		if seenServers[srvName] {
			return &ValidationError{Message: fmt.Sprintf("duplicate configuration entry for server %q", srvName)}
		}
		seenServers[srvName] = true

		if _, exists := knownServers[srvName]; !exists {
			return &ValidationError{Message: fmt.Sprintf("unknown source server %q", srvName)}
		}

		seenPriorities := make(map[int]bool)

		for idx, rule := range srvCfg.Rules {
			ruleID := strings.TrimSpace(rule.ID)
			if ruleID == "" {
				return &ValidationError{Message: fmt.Sprintf("server %q rule #%d is missing an ID", srvName, idx+1)}
			}
			if !utf8.ValidString(rule.ID) {
				return &ValidationError{Message: fmt.Sprintf("server %q rule #%d ID contains invalid UTF-8", srvName, idx+1)}
			}
			if len(rule.ID) > 128 {
				return &ValidationError{Message: fmt.Sprintf("rule ID %q exceeds max length of 128 bytes", rule.ID)}
			}
			if seenIDs[ruleID] {
				return &ValidationError{Message: fmt.Sprintf("duplicate rule ID %q across configs", ruleID)}
			}
			seenIDs[ruleID] = true

			ruleName := rule.Name
			if !utf8.ValidString(ruleName) {
				return &ValidationError{Message: fmt.Sprintf("rule %q name contains invalid UTF-8", ruleID)}
			}
			if len(ruleName) > 256 {
				return &ValidationError{Message: fmt.Sprintf("rule %q name exceeds max length of 256 bytes", ruleID)}
			}

			if rule.SourceServer != "" {
				trimmedSource := strings.TrimSpace(rule.SourceServer)
				if trimmedSource != srvName {
					return &ValidationError{Message: fmt.Sprintf("rule %q source server %q does not match config server %q", ruleID, rule.SourceServer, srvName)}
				}
			}

			target := strings.TrimSpace(rule.TargetServer)
			if target == "" {
				return &ValidationError{Message: fmt.Sprintf("rule %q (%s) has no target server (dangling)", rule.Name, ruleID)}
			}
			if !utf8.ValidString(target) {
				return &ValidationError{Message: fmt.Sprintf("rule %q target server contains invalid UTF-8", ruleID)}
			}
			if len(target) > 256 {
				return &ValidationError{Message: fmt.Sprintf("rule %q target server exceeds max length of 256 bytes", ruleID)}
			}

			// Prevent routing loops where a server relays to itself
			if strings.EqualFold(target, srvName) {
				return &ValidationError{Message: fmt.Sprintf("rule %q on server %q cannot target itself", ruleID, srvName)}
			}

			// Target must be a known server, "direct", "block", "ru-traffic-portal", or have an outbound template
			if !strings.EqualFold(target, "direct") && !strings.EqualFold(target, "block") && !strings.EqualFold(target, "ru-traffic-portal") {
				if _, exists := knownServers[target]; !exists {
					tplPath := m.OutboundTemplatePath(target)
					if _, err := os.Stat(tplPath); err != nil {
						return &ValidationError{Message: fmt.Sprintf("rule %q targets unknown server %q", rule.Name, target)}
					}
				}
			}

			if rule.Priority < 0 {
				return &ValidationError{Message: fmt.Sprintf("rule %q has invalid negative priority %d", ruleID, rule.Priority)}
			}
			if seenPriorities[rule.Priority] {
				return &ValidationError{Message: fmt.Sprintf("server %q has duplicate rule priority %d", srvName, rule.Priority)}
			}
			seenPriorities[rule.Priority] = true

			validDomains := 0
			for dIdx, d := range rule.Domain {
				if !utf8.ValidString(d) {
					return &ValidationError{Message: fmt.Sprintf("rule %q domain #%d contains invalid UTF-8", ruleID, dIdx+1)}
				}
				if len(d) > 512 {
					return &ValidationError{Message: fmt.Sprintf("rule %q domain entry exceeds max length of 512 bytes", ruleID)}
				}
				if strings.TrimSpace(d) != "" {
					validDomains++
				}
			}

			validIPs := 0
			for ipIdx, ip := range rule.IP {
				if !utf8.ValidString(ip) {
					return &ValidationError{Message: fmt.Sprintf("rule %q IP #%d contains invalid UTF-8", ruleID, ipIdx+1)}
				}
				if len(ip) > 128 {
					return &ValidationError{Message: fmt.Sprintf("rule %q IP entry exceeds max length of 128 bytes", ruleID)}
				}
				if strings.TrimSpace(ip) != "" {
					validIPs++
				}
			}

			if validDomains == 0 && validIPs == 0 {
				return &ValidationError{Message: fmt.Sprintf("rule %q (%s) must specify at least one valid domain or IP", rule.Name, ruleID)}
			}
		}
	}

	// Cluster-wide indirect cycle detection
	if err := detectRoutingCycles(configs); err != nil {
		return err
	}

	return nil
}

// GenerateXrayRouting converts RoutingRule slice into Xray Core routing.rules JSON objects.
func (m *Manager) GenerateXrayRouting(serverName string, rules []RoutingRule) ([]map[string]any, error) {
	return m.CompileServerRules(rules)
}

// GenerateXrayRoutingWithExisting converts RoutingRule slice into Xray rules using existing live outbound tags.
func (m *Manager) GenerateXrayRoutingWithExisting(serverName string, rules []RoutingRule, existingTags map[string]bool) ([]map[string]any, error) {
	return m.CompileServerRulesWithExisting(rules, existingTags)
}

// ResolveOutboundTag determines the exact Xray outboundTag for a given target server.
// If an outbound template file exists (e.g. outbounds/<target>.json), its declared "tag" is used.
func (m *Manager) ResolveOutboundTag(target string) string {
	return m.ResolveOutboundTagWithExisting(target, nil)
}

// ResolveOutboundTagWithExisting determines the exact Xray outboundTag, checking outbound templates first,
// then matching existing tags in the live Xray config (e.g. matching "relay-NLD" for "nld-master").
func (m *Manager) ResolveOutboundTagWithExisting(target string, existingTags map[string]bool) string {
	tgt := strings.TrimSpace(target)
	switch strings.ToLower(tgt) {
	case "direct":
		return "direct"
	case "block":
		return "block"
	case "ru-traffic-portal":
		return "ru-traffic-portal"
	}

	// 1. Check if an outbound template file exists on disk
	if m != nil && m.routingDir != "" {
		tplPath := m.OutboundTemplatePath(tgt)
		if data, err := os.ReadFile(tplPath); err == nil {
			var parsed struct {
				Tag string `json:"tag"`
			}
			if err := json.Unmarshal(data, &parsed); err == nil && strings.TrimSpace(parsed.Tag) != "" {
				return strings.TrimSpace(parsed.Tag)
			}
		}
	}

	// 2. Check existing tags in live Xray config for exact or fuzzy matches (e.g. "relay-NLD" for "nld-master")
	if len(existingTags) > 0 {
		candidates := []string{
			tgt,
			fmt.Sprintf("relay_%s", tgt),
			fmt.Sprintf("relay-%s", tgt),
			fmt.Sprintf("relay_%s", strings.ToUpper(tgt)),
			fmt.Sprintf("relay-%s", strings.ToUpper(tgt)),
		}
		// If tgt is like "nld-master", also check "relay-NLD", "relay_NLD", "relay-nld", etc.
		parts := strings.FieldsFunc(tgt, func(r rune) bool { return r == '-' || r == '_' })
		if len(parts) > 0 {
			first := parts[0]
			candidates = append(candidates,
				fmt.Sprintf("relay-%s", strings.ToUpper(first)),
				fmt.Sprintf("relay_%s", strings.ToUpper(first)),
				fmt.Sprintf("relay-%s", strings.ToLower(first)),
				fmt.Sprintf("relay_%s", strings.ToLower(first)),
			)
		}

		for _, c := range candidates {
			for existingTag := range existingTags {
				if strings.EqualFold(existingTag, c) {
					return existingTag
				}
			}
		}
	}

	if strings.HasPrefix(tgt, "relay-") || strings.HasPrefix(tgt, "relay_") || strings.HasPrefix(tgt, "portal-") || strings.HasSuffix(tgt, "-portal") {
		return tgt
	}
	return fmt.Sprintf("relay_%s", tgt)
}

// CompileServerRules produces the JSON array of Xray routing rules for a server.
func (m *Manager) CompileServerRules(rules []RoutingRule) ([]map[string]any, error) {
	return m.CompileServerRulesWithExisting(rules, nil)
}

// CompileServerRulesWithExisting produces the JSON array of Xray routing rules for a server with existing tags context.
func (m *Manager) CompileServerRulesWithExisting(rules []RoutingRule, existingTags map[string]bool) ([]map[string]any, error) {
	sorted := make([]RoutingRule, len(rules))
	copy(sorted, rules)

	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Priority != sorted[j].Priority {
			return sorted[i].Priority < sorted[j].Priority
		}
		return sorted[i].ID < sorted[j].ID
	})

	outRules := make([]map[string]any, 0, len(sorted))
	for _, r := range sorted {
		if !r.Enabled {
			continue
		}

		outboundTag := m.ResolveOutboundTagWithExisting(r.TargetServer, existingTags)

		ruleMap := map[string]any{
			"type":        "field",
			"outboundTag": outboundTag,
		}

		cleanDomains := make([]string, 0, len(r.Domain))
		for _, d := range r.Domain {
			trimmed := strings.TrimSpace(d)
			if trimmed != "" {
				cleanDomains = append(cleanDomains, trimmed)
			}
		}
		if len(cleanDomains) > 0 {
			ruleMap["domain"] = cleanDomains
		}

		cleanIPs := make([]string, 0, len(r.IP))
		for _, ip := range r.IP {
			trimmed := strings.TrimSpace(ip)
			if trimmed != "" {
				cleanIPs = append(cleanIPs, trimmed)
			}
		}
		if len(cleanIPs) > 0 {
			ruleMap["ip"] = cleanIPs
		}

		outRules = append(outRules, ruleMap)
	}

	return outRules, nil
}

// getRequiredOutboundsLocked reads JSON outbound templates assuming lock is held or internal caller.
func (m *Manager) getRequiredOutboundsLocked(serverName string, rules []RoutingRule, existingTags map[string]bool) ([]json.RawMessage, error) {
	neededTargets := make(map[string]bool)
	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		tgt := strings.TrimSpace(r.TargetServer)
		if tgt != "" && !strings.EqualFold(tgt, "direct") && !strings.EqualFold(tgt, "block") && !strings.EqualFold(tgt, "ru-traffic-portal") && tgt != serverName {
			neededTargets[tgt] = true
		}
	}

	targets := make([]string, 0, len(neededTargets))
	for tgt := range neededTargets {
		targets = append(targets, tgt)
	}
	sort.Strings(targets)

	outbounds := make([]json.RawMessage, 0, len(targets))
	for _, tgt := range targets {
		tplPath := m.OutboundTemplatePath(tgt)
		data, err := os.ReadFile(tplPath)
		if err != nil {
			// If template file does not exist, check if live config already has the outbound
			resolvedTag := m.ResolveOutboundTagWithExisting(tgt, existingTags)
			if existingTags != nil && existingTags[resolvedTag] {
				continue
			}
			if os.IsNotExist(err) {
				return nil, fmt.Errorf("outbound template missing for target server %q at %s", tgt, tplPath)
			}
			return nil, fmt.Errorf("reading outbound template for %q: %w", tgt, err)
		}
		if !json.Valid(data) {
			return nil, fmt.Errorf("outbound template for %q contains invalid JSON: %s", tgt, tplPath)
		}
		outbounds = append(outbounds, json.RawMessage(data))
	}

	return outbounds, nil
}

// GetRequiredOutbounds reads JSON outbound templates for each unique target server referenced in rules.
func (m *Manager) GetRequiredOutbounds(serverName string, rules []RoutingRule) ([]json.RawMessage, error) {
	if m == nil {
		return nil, fmt.Errorf("server_routing: manager is nil")
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	return m.getRequiredOutboundsLocked(serverName, rules, nil)
}

// preflightCheckXray tests candidate config against xray binary if available.
func preflightCheckXray(candidateConfig []byte) error {
	xrayBin, err := exec.LookPath("xray")
	if err != nil || xrayBin == "" {
		return nil
	}

	// Sanitize log config in candidate preflight payload to prevent permission denied errors
	// on runtime log files (e.g. /dev/shm/xray_access.log owned by root/live daemon).
	testPayload := candidateConfig
	var rawMap map[string]any
	if err := json.Unmarshal(candidateConfig, &rawMap); err == nil {
		if logVal, ok := rawMap["log"].(map[string]any); ok {
			delete(logVal, "access")
			delete(logVal, "error")
			rawMap["log"] = logVal
			if sanitized, err := json.Marshal(rawMap); err == nil {
				testPayload = sanitized
			}
		}
	}

	tmpFile, err := os.CreateTemp("", "xray-preflight-*.json")
	if err != nil {
		return fmt.Errorf("create temp preflight config: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	if _, err := tmpFile.Write(testPayload); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("write temp preflight config: %w", err)
	}
	_ = tmpFile.Close()

	ctxTest, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctxTest, xrayBin, "run", "-test", "-c", tmpPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		outStr := strings.TrimSpace(string(out))
		if strings.Contains(outStr, "failed to initialize access logger") ||
			strings.Contains(outStr, "failed to initialize error logger") ||
			strings.Contains(outStr, "failed to find available ipv6 table index") ||
			strings.Contains(outStr, "failed to find available table index") ||
			strings.Contains(outStr, "Using kernel TUN") ||
			strings.Contains(outStr, "address already in use") ||
			strings.Contains(outStr, "operation not permitted") {
			return nil
		}
		return fmt.Errorf("xray configuration test failed: %s (err: %w)", outStr, err)
	}
	return nil
}

// SaveRouting validates and atomically writes all ServerRoutingConfig files to disk.
func (m *Manager) SaveRouting(ctx context.Context, configs []ServerRoutingConfig) error {
	if m == nil {
		return fmt.Errorf("server_routing: manager is nil")
	}

	if err := m.ValidateRules(configs); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	return m.saveRoutingLocked(configs)
}

func (m *Manager) saveRoutingLocked(configs []ServerRoutingConfig) error {
	for _, srvCfg := range configs {
		srvName := strings.TrimSpace(srvCfg.Server)
		cleanedRules := make([]RoutingRule, len(srvCfg.Rules))
		for i, r := range srvCfg.Rules {
			var cleanDomains []string
			for _, d := range r.Domain {
				trimmed := strings.TrimSpace(d)
				if trimmed != "" {
					cleanDomains = append(cleanDomains, trimmed)
				}
			}
			if cleanDomains == nil {
				cleanDomains = []string{}
			}

			var cleanIPs []string
			for _, ip := range r.IP {
				trimmed := strings.TrimSpace(ip)
				if trimmed != "" {
					cleanIPs = append(cleanIPs, trimmed)
				}
			}
			if cleanIPs == nil {
				cleanIPs = []string{}
			}

			cleanedRules[i] = RoutingRule{
				ID:           strings.TrimSpace(r.ID),
				Name:         strings.TrimSpace(r.Name),
				SourceServer: srvName,
				TargetServer: strings.TrimSpace(r.TargetServer),
				Domain:       cleanDomains,
				IP:           cleanIPs,
				Priority:     r.Priority,
				Enabled:      r.Enabled,
			}
		}

		sort.Slice(cleanedRules, func(i, j int) bool {
			if cleanedRules[i].Priority != cleanedRules[j].Priority {
				return cleanedRules[i].Priority < cleanedRules[j].Priority
			}
			return cleanedRules[i].ID < cleanedRules[j].ID
		})

		toWrite := ServerRoutingConfig{
			Server: srvName,
			Rules:  cleanedRules,
		}

		data, err := json.MarshalIndent(toWrite, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal config for %q: %w", srvName, err)
		}

		destPath := m.ServerConfigPath(srvName)
		if err := safeio.WriteToFile(destPath, data, 0o644); err != nil {
			return fmt.Errorf("atomic write config to %s: %w", destPath, err)
		}
	}

	return nil
}

// ApplyToLocalXray applies the generated routing rules and required relay outbounds to the local Xray config.
func (m *Manager) ApplyToLocalXray(ctx context.Context, configPath string, rules []RoutingRule) error {
	if m == nil {
		return fmt.Errorf("server_routing: manager is nil")
	}

	localName := m.LocalNodeName()
	singleCfg := []ServerRoutingConfig{
		{
			Server: localName,
			Rules:  rules,
		},
	}

	return m.applyTransactionWithNode(ctx, singleCfg, configPath, localName)
}

// ApplyNodeToLocalXray applies the routing rules of a specific node to a local Xray config file.
func (m *Manager) ApplyNodeToLocalXray(ctx context.Context, nodeName string, configPath string, rules []RoutingRule) error {
	if m == nil {
		return fmt.Errorf("server_routing: manager is nil")
	}

	singleCfg := []ServerRoutingConfig{
		{
			Server: nodeName,
			Rules:  rules,
		},
	}

	return m.applyTransactionWithNode(ctx, singleCfg, configPath, nodeName)
}

// ApplyTransaction atomically validates, saves all server routing rule files, and applies them to local Xray.
// If any step fails (validation, pre-flight, or service restart), ALL files and Xray configs are rolled back to their previous state.
func (m *Manager) ApplyTransaction(ctx context.Context, configs []ServerRoutingConfig, configPath string) error {
	if m == nil {
		return fmt.Errorf("server_routing: manager is nil")
	}

	return m.applyTransactionWithNode(ctx, configs, configPath, m.LocalNodeName())
}

func (m *Manager) applyTransactionWithNode(ctx context.Context, configs []ServerRoutingConfig, configPath string, targetNodeName string) error {
	// 1. Validation (including O(P * (V + E)) cycle check)
	// Skip strict validation on slave nodes where m.cfg is nil: the master
	// has already validated the full rule set with complete cluster context.
	// Re-validating on a slave that lacks GetKnownServers() context would
	// reject valid cross-node targets like "nld-master".
	if m.cfg != nil {
		if err := m.ValidateRules(configs); err != nil {
			return err
		}
	}

	if configPath == "" {
		if m.cfg != nil && m.cfg.Paths.XrayConfig != "" {
			configPath = m.cfg.Paths.XrayConfig
		} else {
			configPath = "/usr/local/etc/xray/config.json"
		}
	}

	// 2. Acquire exclusive transaction lock
	m.mu.Lock()
	defer m.mu.Unlock()

	// 3. Create backup snapshots of existing server routing JSON files
	fileBackups := make(map[string][]byte)
	for _, cfg := range configs {
		p := m.ServerConfigPath(cfg.Server)
		if data, err := os.ReadFile(p); err == nil {
			fileBackups[p] = data
		}
	}

	// 4. Create backup snapshot of existing Xray config
	var xrayBackup []byte
	xrayConfigExists := false
	if data, err := os.ReadFile(configPath); err == nil {
		xrayConfigExists = true
		xrayBackup = make([]byte, len(data))
		copy(xrayBackup, data)
	} else {
		for _, fallbackPath := range []string{
			"/usr/local/etc/xray/config.json",
			"/etc/xraytool/configs/configs_json.json",
			"/etc/xraytool/config.json",
			"/root/xraytool/data/configs/configs_json.json",
		} {
			if fallbackPath == configPath {
				continue
			}
			if data, readErr := os.ReadFile(fallbackPath); readErr == nil {
				xrayConfigExists = true
				xrayBackup = make([]byte, len(data))
				copy(xrayBackup, data)
				configPath = fallbackPath
				break
			}
		}
	}

	// Rollback helper to restore both rule files and Xray config
	rollbackAll := func() {
		for p, data := range fileBackups {
			_ = safeio.WriteToFile(p, data, 0o644)
		}
		if xrayConfigExists && len(xrayBackup) > 0 {
			_ = safeio.WriteToFile(configPath, xrayBackup, 0o644)
		}
		// Attempt service restart with restored config
		restartCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if m.restartFn != nil {
			_ = m.restartFn(restartCtx)
		} else if _, lookErr := exec.LookPath("systemctl"); lookErr == nil {
			_ = exec.CommandContext(restartCtx, "systemctl", "restart", "xray").Run()
		}
	}

	// 5. Generate candidate Xray config if target node rules are present
	var nodeRules []RoutingRule
	hasNodeRules := false
	matchedNode := targetNodeName

	for _, srv := range configs {
		if srv.Server == targetNodeName {
			nodeRules = srv.Rules
			hasNodeRules = true
			matchedNode = srv.Server
			break
		}
	}

	if !hasNodeRules {
		// Try MasterName fallback if targetNodeName was not found directly
		masterName := m.MasterName()
		for _, srv := range configs {
			if srv.Server == masterName {
				nodeRules = srv.Rules
				hasNodeRules = true
				matchedNode = masterName
				break
			}
		}
	}

	var candidateXrayConfig []byte
	if hasNodeRules && xrayConfigExists {
		var root map[string]json.RawMessage
		if err := json.Unmarshal(xrayBackup, &root); err != nil {
			return fmt.Errorf("parse xray config: %w", err)
		}
		if root == nil {
			root = make(map[string]json.RawMessage)
		}

		var existingOutbounds []map[string]json.RawMessage
		if rawOutbounds, ok := root["outbounds"]; ok {
			_ = json.Unmarshal(rawOutbounds, &existingOutbounds)
		}

		// Build a set of outbound tags already present in the live Xray config
		existingOutboundTags := make(map[string]bool, len(existingOutbounds))
		for _, ob := range existingOutbounds {
			if rawTag, ok := ob["tag"]; ok {
				var tag string
				if err := json.Unmarshal(rawTag, &tag); err == nil && tag != "" {
					existingOutboundTags[tag] = true
				}
			}
		}

		xrayRules, err := m.GenerateXrayRoutingWithExisting(matchedNode, nodeRules, existingOutboundTags)
		if err != nil {
			return fmt.Errorf("generate xray routing: %w", err)
		}

		var routingMap map[string]json.RawMessage
		if rawRouting, ok := root["routing"]; ok {
			_ = json.Unmarshal(rawRouting, &routingMap)
		}
		if routingMap == nil {
			routingMap = make(map[string]json.RawMessage)
		}

		var existingRules []map[string]json.RawMessage
		if rawRules, ok := routingMap["rules"]; ok {
			_ = json.Unmarshal(rawRules, &existingRules)
		}

		// Separate and preserve unmanaged static infrastructure rules (inboundTag, protocol, port)
		var apiRules []map[string]json.RawMessage
		var staticRules []map[string]json.RawMessage

		for _, r := range existingRules {
			_, hasInbound := r["inboundTag"]
			_, hasProtocol := r["protocol"]
			_, hasPort := r["port"]

			if hasInbound || hasProtocol || hasPort {
				isAPI := false
				if hasInbound {
					var inbounds []string
					if err := json.Unmarshal(r["inboundTag"], &inbounds); err == nil {
						for _, tag := range inbounds {
							if strings.HasPrefix(strings.ToLower(tag), "api") {
								isAPI = true
								break
							}
						}
					}
				}
				if isAPI {
					apiRules = append(apiRules, r)
				} else {
					staticRules = append(staticRules, r)
				}
			}
		}

		finalRules := make([]any, 0, len(apiRules)+len(staticRules)+len(xrayRules))
		for _, r := range apiRules {
			finalRules = append(finalRules, r)
		}
		for _, r := range staticRules {
			finalRules = append(finalRules, r)
		}
		for _, r := range xrayRules {
			finalRules = append(finalRules, r)
		}

		rulesJSON, err := json.Marshal(finalRules)
		if err != nil {
			return fmt.Errorf("marshal xray rules: %w", err)
		}
		routingMap["rules"] = rulesJSON

		routingJSON, err := json.Marshal(routingMap)
		if err != nil {
			return fmt.Errorf("marshal routing map: %w", err)
		}
		root["routing"] = routingJSON

		// Determine all needed outbound tags by:
		// 1. Dynamic node rules
		neededRelayTags := make(map[string]bool)
		for _, r := range nodeRules {
			if !r.Enabled {
				continue
			}
			tgt := strings.TrimSpace(r.TargetServer)
			if tgt != "" && !strings.EqualFold(tgt, "direct") && !strings.EqualFold(tgt, "block") && !strings.EqualFold(tgt, "ru-traffic-portal") && tgt != matchedNode {
				tag := m.ResolveOutboundTagWithExisting(tgt, existingOutboundTags)
				neededRelayTags[tag] = true
			}
		}

		// 2. Preserved static / cascade / API rules
		for _, r := range append(apiRules, staticRules...) {
			if rawOutbound, ok := r["outboundTag"]; ok {
				var tag string
				if err := json.Unmarshal(rawOutbound, &tag); err == nil && tag != "" {
					neededRelayTags[tag] = true
				}
			}
		}

		relayOutbounds, err := m.getRequiredOutboundsLocked(matchedNode, nodeRules, existingOutboundTags)
		if err != nil {
			return fmt.Errorf("get required outbounds: %w", err)
		}

		// Collect tags of newly required outbounds from templates
		templateOutboundTags := make(map[string]bool)
		for _, rawOb := range relayOutbounds {
			var obMap map[string]json.RawMessage
			if err := json.Unmarshal(rawOb, &obMap); err == nil {
				if rawTag, ok := obMap["tag"]; ok {
					var tag string
					if err := json.Unmarshal(rawTag, &tag); err == nil && tag != "" {
						templateOutboundTags[tag] = true
					}
				}
			}
		}

		cleanOutbounds := make([]map[string]json.RawMessage, 0, len(existingOutbounds)+len(relayOutbounds))
		for _, ob := range existingOutbounds {
			if rawTag, ok := ob["tag"]; ok {
				var tag string
				if err := json.Unmarshal(rawTag, &tag); err == nil {
					// If this tag is being updated/replaced by a template outbound, skip the old copy
					if templateOutboundTags[tag] {
						continue
					}
					// If this is a relay tag (starts with relay_ or relay-)
					if strings.HasPrefix(tag, "relay_") || strings.HasPrefix(tag, "relay-") {
						// Keep it ONLY if it is still actively needed by rules
						if !neededRelayTags[tag] {
							continue
						}
					}
				}
			}
			cleanOutbounds = append(cleanOutbounds, ob)
		}

		for _, rawOb := range relayOutbounds {
			var obMap map[string]json.RawMessage
			if err := json.Unmarshal(rawOb, &obMap); err == nil {
				cleanOutbounds = append(cleanOutbounds, obMap)
			}
		}

		outboundsJSON, err := json.Marshal(cleanOutbounds)
		if err != nil {
			return fmt.Errorf("marshal outbounds: %w", err)
		}
		root["outbounds"] = outboundsJSON

		candidateXrayConfig, err = json.MarshalIndent(root, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal final config: %w", err)
		}

		// 6. Pre-flight check on candidate config
		if err := preflightCheckXray(candidateXrayConfig); err != nil {
			return fmt.Errorf("сервер %q: ошибка валидации конфига Xray: %w", matchedNode, err)
		}
	}

	// 7. Write all server routing rule files to disk
	if err := m.saveRoutingLocked(configs); err != nil {
		rollbackAll()
		return fmt.Errorf("сервер %q: ошибка сохранения файлов маршрутизации: %w", matchedNode, err)
	}

	// 8. Write candidate Xray config and restart service
	if hasNodeRules && xrayConfigExists && len(candidateXrayConfig) > 0 {
		if err := safeio.WriteToFile(configPath, candidateXrayConfig, 0o644); err != nil {
			rollbackAll()
			return fmt.Errorf("сервер %q: запись обновленного config.json не удалась: %w", matchedNode, err)
		}

		// Also update template files if they exist so template and live config stay permanently in sync
		var templateCandidates []string
		if m.cfg != nil && m.cfg.Paths.XrayTemplate != "" {
			templateCandidates = append(templateCandidates, m.cfg.Paths.XrayTemplate)
		}
		templateCandidates = append(templateCandidates,
			"/etc/xraytool/xray_template.json",
			"/etc/xraytool/configs/configs_json.json",
			"/root/xraytool/data/configs/configs_json.json",
			"/etc/xraytool/config.template.json",
		)
		var candRoot map[string]json.RawMessage
		_ = json.Unmarshal(candidateXrayConfig, &candRoot)

		for _, tpl := range templateCandidates {
			if tpl == configPath {
				continue
			}
			if tplData, err := os.ReadFile(tpl); err == nil && len(tplData) > 0 {
				var tplRoot map[string]json.RawMessage
				if err := json.Unmarshal(tplData, &tplRoot); err == nil && tplRoot != nil && candRoot != nil {
					tplRoot["routing"] = candRoot["routing"]
					tplRoot["outbounds"] = candRoot["outbounds"]
					if updatedTpl, err := json.MarshalIndent(tplRoot, "", "  "); err == nil {
						_ = safeio.WriteToFile(tpl, updatedTpl, 0o644)
					}
				}
			}
		}

		restartCtx, cancelRestart := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancelRestart()

		var serviceErr error
		if m.restartFn != nil {
			serviceErr = m.restartFn(restartCtx)
		} else if _, lookErr := exec.LookPath("systemctl"); lookErr != nil {
			if m.log != nil {
				m.log.Warn("systemctl is not available in environment, skipped systemctl restart xray")
			}
		} else {
			cmd := exec.CommandContext(restartCtx, "systemctl", "restart", "xray")
			out, execErr := cmd.CombinedOutput()
			if execErr != nil {
				serviceErr = fmt.Errorf("systemctl restart xray failed: %s (%w)", strings.TrimSpace(string(out)), execErr)
			}
		}

		if serviceErr != nil {
			rollbackAll()
			return fmt.Errorf("сервер %q: перезапуск службы Xray не удался (%w), изменения откатаны назад (rolled back to previous state)", matchedNode, serviceErr)
		}
	}

	return nil
}

// ApplyLocalServerRouting applies routing rules for a specific node from routingDir to xrayConfigPath with preflight & rollback.
func ApplyLocalServerRouting(ctx context.Context, nodeName, routingDir, xrayConfigPath string, restartFn func(context.Context) error) error {
	if strings.TrimSpace(nodeName) == "" {
		return fmt.Errorf("nodeName is required")
	}
	if strings.TrimSpace(routingDir) == "" {
		if _, err := os.Stat("/etc/xraytool/routing"); err == nil {
			routingDir = "/etc/xraytool/routing"
		} else if _, err := os.Stat("/etc/xraytool"); err == nil {
			routingDir = "/etc/xraytool/routing"
		} else {
			routingDir = "/root/xraytool/data/routing"
		}
	}
	if strings.TrimSpace(xrayConfigPath) == "" {
		for _, candidate := range []string{
			"/usr/local/etc/xray/config.json",
			"/etc/xraytool/configs/configs_json.json",
			"/etc/xraytool/config.json",
			"/root/xraytool/data/configs/configs_json.json",
		} {
			if _, err := os.Stat(candidate); err == nil {
				xrayConfigPath = candidate
				break
			}
		}
		if xrayConfigPath == "" {
			xrayConfigPath = "/usr/local/etc/xray/config.json"
		}
	}

	mgr := NewManager(routingDir, nil, nil)
	if restartFn != nil {
		mgr.SetRestartFunc(restartFn)
	}

	cfgFile := mgr.ServerConfigPath(nodeName)
	data, err := os.ReadFile(cfgFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // No rules defined for this node yet
		}
		return fmt.Errorf("read node routing config %s: %w", cfgFile, err)
	}

	var srvCfg ServerRoutingConfig
	if err := json.Unmarshal(data, &srvCfg); err != nil {
		return fmt.Errorf("unmarshal node routing config: %w", err)
	}

	return mgr.ApplyNodeToLocalXray(ctx, nodeName, xrayConfigPath, srvCfg.Rules)
}
