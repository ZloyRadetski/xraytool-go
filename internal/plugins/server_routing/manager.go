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
			if target == "" || strings.EqualFold(target, "direct") || strings.EqualFold(target, "block") {
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

			// Target must be a known server, "direct", or "block"
			if !strings.EqualFold(target, "direct") && !strings.EqualFold(target, "block") {
				if _, exists := knownServers[target]; !exists {
					return &ValidationError{Message: fmt.Sprintf("rule %q targets unknown server %q", rule.Name, target)}
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
	outRules := make([]map[string]any, 0, len(rules))

	sortedRules := make([]RoutingRule, len(rules))
	copy(sortedRules, rules)
	sort.Slice(sortedRules, func(i, j int) bool {
		if sortedRules[i].Priority != sortedRules[j].Priority {
			return sortedRules[i].Priority < sortedRules[j].Priority
		}
		return sortedRules[i].ID < sortedRules[j].ID
	})

	for _, r := range sortedRules {
		if !r.Enabled {
			continue
		}

		tgt := strings.TrimSpace(r.TargetServer)
		outboundTag := ""
		switch strings.ToLower(tgt) {
		case "direct":
			outboundTag = "direct"
		case "block":
			outboundTag = "block"
		default:
			outboundTag = fmt.Sprintf("relay_%s", tgt)
		}

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
func (m *Manager) getRequiredOutboundsLocked(serverName string, rules []RoutingRule) ([]json.RawMessage, error) {
	neededTargets := make(map[string]bool)
	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		tgt := strings.TrimSpace(r.TargetServer)
		if tgt != "" && !strings.EqualFold(tgt, "direct") && !strings.EqualFold(tgt, "block") && tgt != serverName {
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

	return m.getRequiredOutboundsLocked(serverName, rules)
}

// preflightCheckXray tests candidate config against xray binary if available.
func preflightCheckXray(candidateConfig []byte) error {
	xrayBin, err := exec.LookPath("xray")
	if err != nil || xrayBin == "" {
		return nil
	}

	tmpFile, err := os.CreateTemp("", "xray-preflight-*.json")
	if err != nil {
		return fmt.Errorf("create temp preflight config: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	if _, err := tmpFile.Write(candidateConfig); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("write temp preflight config: %w", err)
	}
	_ = tmpFile.Close()

	ctxTest, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctxTest, xrayBin, "run", "-test", "-c", tmpPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("xray configuration test failed: %s (err: %w)", strings.TrimSpace(string(out)), err)
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
	if err := m.ValidateRules(configs); err != nil {
		return err
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
		} else {
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

		xrayRules, err := m.GenerateXrayRouting(matchedNode, nodeRules)
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

		relayOutbounds, err := m.getRequiredOutboundsLocked(matchedNode, nodeRules)
		if err != nil {
			return fmt.Errorf("get required outbounds: %w", err)
		}

		var existingOutbounds []map[string]json.RawMessage
		if rawOutbounds, ok := root["outbounds"]; ok {
			_ = json.Unmarshal(rawOutbounds, &existingOutbounds)
		}

		cleanOutbounds := make([]map[string]json.RawMessage, 0, len(existingOutbounds))
		for _, ob := range existingOutbounds {
			if rawTag, ok := ob["tag"]; ok {
				var tag string
				if err := json.Unmarshal(rawTag, &tag); err == nil && strings.HasPrefix(tag, "relay_") {
					continue
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
			return fmt.Errorf("pre-flight validation failed: %w", err)
		}
	}

	// 7. Write all server routing rule files to disk
	if err := m.saveRoutingLocked(configs); err != nil {
		rollbackAll()
		return fmt.Errorf("save routing files failed: %w", err)
	}

	// 8. Write candidate Xray config and restart service
	if hasNodeRules && xrayConfigExists && len(candidateXrayConfig) > 0 {
		if err := safeio.WriteToFile(configPath, candidateXrayConfig, 0o644); err != nil {
			rollbackAll()
			return fmt.Errorf("write updated xray config failed: %w", err)
		}

		restartCtx, cancelRestart := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancelRestart()

		var serviceErr error
		if m.restartFn != nil {
			serviceErr = m.restartFn(restartCtx)
		} else {
			cmd := exec.CommandContext(restartCtx, "systemctl", "restart", "xray")
			out, execErr := cmd.CombinedOutput()
			if execErr != nil {
				serviceErr = fmt.Errorf("systemctl restart xray failed: %s (%w)", strings.TrimSpace(string(out)), execErr)
			}
		}

		if serviceErr != nil {
			rollbackAll()
			return fmt.Errorf("xray restart failed (%w), transaction rolled back to previous state", serviceErr)
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
		routingDir = "/root/xraytool/data/routing"
	}
	if strings.TrimSpace(xrayConfigPath) == "" {
		xrayConfigPath = "/usr/local/etc/xray/config.json"
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
