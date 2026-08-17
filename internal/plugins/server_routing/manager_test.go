package server_routing

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	json "github.com/goccy/go-json"
	"xraytool/internal/appconfig"
)

func createTestManager(t *testing.T) (*Manager, string) {
	t.Helper()
	tmpDir, err := os.MkdirTemp("", "server_routing_test_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmpDir) })

	outboundsDir := filepath.Join(tmpDir, "outbounds")
	if err := os.MkdirAll(outboundsDir, 0o755); err != nil {
		t.Fatalf("failed to create outbounds dir: %v", err)
	}

	cfg := &appconfig.Config{
		Mode: "master",
		Server: appconfig.ServerConf{
			Domain: "master.test.com",
			IP:     "1.2.3.4",
		},
		Replication: appconfig.ReplicationConf{
			AllowedNodes: []string{"msk-slave", "de-slave"},
		},
	}

	mgr := NewManager(tmpDir, cfg, nil)
	mgr.SetRestartFunc(func(ctx context.Context) error { return nil })
	return mgr, tmpDir
}

func TestManager_LoadTopology_Empty(t *testing.T) {
	mgr, _ := createTestManager(t)
	ctx := context.Background()

	topo, err := mgr.LoadTopology(ctx)
	if err != nil {
		t.Fatalf("LoadTopology failed: %v", err)
	}

	if len(topo.Servers) != 3 {
		t.Errorf("expected 3 servers, got %d", len(topo.Servers))
	}
	if len(topo.SpecialNodes) != 3 {
		t.Errorf("expected 3 special nodes, got %d", len(topo.SpecialNodes))
	}
	if len(topo.Routing) != 3 {
		t.Errorf("expected 3 routing configs, got %d", len(topo.Routing))
	}
}

func TestManager_SaveAndLoadRouting(t *testing.T) {
	mgr, tmpDir := createTestManager(t)
	ctx := context.Background()

	// Write an outbound template for msk-slave
	mskOutbound := `{"protocol": "vless", "tag": "relay_msk-slave", "settings": {}}`
	if err := os.WriteFile(filepath.Join(tmpDir, "outbounds", "msk-slave.json"), []byte(mskOutbound), 0o644); err != nil {
		t.Fatalf("failed to write outbound template: %v", err)
	}

	configs := []ServerRoutingConfig{
		{
			Server: "master.test.com",
			Rules: []RoutingRule{
				{
					ID:           "rule-1",
					Name:         "Telegram to MSK",
					SourceServer: "master.test.com",
					TargetServer: "msk-slave",
					Domain:       []string{"geosite:telegram"},
					Priority:     1,
					Enabled:      true,
				},
				{
					ID:           "rule-2",
					Name:         "Direct Russian traffic",
					SourceServer: "master.test.com",
					TargetServer: "direct",
					Domain:       []string{"geosite:ru"},
					IP:           []string{"geoip:ru"},
					Priority:     2,
					Enabled:      true,
				},
			},
		},
		{
			Server: "msk-slave",
			Rules:  []RoutingRule{},
		},
		{
			Server: "de-slave",
			Rules:  []RoutingRule{},
		},
	}

	if err := mgr.SaveRouting(ctx, configs); err != nil {
		t.Fatalf("SaveRouting failed: %v", err)
	}

	topo, err := mgr.LoadTopology(ctx)
	if err != nil {
		t.Fatalf("LoadTopology failed: %v", err)
	}

	if len(topo.Outbounds) != 1 || topo.Outbounds[0] != "msk-slave" {
		t.Errorf("expected outbounds [msk-slave], got %v", topo.Outbounds)
	}

	var masterCfg *ServerRoutingConfig
	for i := range topo.Routing {
		if topo.Routing[i].Server == "master.test.com" {
			masterCfg = &topo.Routing[i]
			break
		}
	}
	if masterCfg == nil || len(masterCfg.Rules) != 2 {
		t.Fatalf("expected 2 rules for master, got %v", masterCfg)
	}
	if masterCfg.Rules[0].Name != "Telegram to MSK" {
		t.Errorf("unexpected first rule name: %s", masterCfg.Rules[0].Name)
	}

	// Test GenerateXrayRouting
	xrayRules, err := mgr.GenerateXrayRouting("master.test.com", masterCfg.Rules)
	if err != nil {
		t.Fatalf("GenerateXrayRouting failed: %v", err)
	}
	if len(xrayRules) != 2 {
		t.Fatalf("expected 2 xray rules, got %d", len(xrayRules))
	}
	if xrayRules[0]["outboundTag"] != "relay_msk-slave" {
		t.Errorf("expected outboundTag relay_msk-slave, got %v", xrayRules[0]["outboundTag"])
	}
	if xrayRules[1]["outboundTag"] != "direct" {
		t.Errorf("expected outboundTag direct, got %v", xrayRules[1]["outboundTag"])
	}

	// Test GetRequiredOutbounds
	outbounds, err := mgr.GetRequiredOutbounds("master.test.com", masterCfg.Rules)
	if err != nil {
		t.Fatalf("GetRequiredOutbounds failed: %v", err)
	}
	if len(outbounds) != 1 {
		t.Fatalf("expected 1 required outbound, got %d", len(outbounds))
	}
}

func TestManager_ValidateRules_Dangling(t *testing.T) {
	mgr, _ := createTestManager(t)
	ctx := context.Background()

	badConfig := []ServerRoutingConfig{
		{
			Server: "master.test.com",
			Rules: []RoutingRule{
				{
					ID:           "rule-dangling",
					Name:         "Dangling rule",
					SourceServer: "master.test.com",
					TargetServer: "", // dangling!
					Domain:       []string{"example.com"},
					Priority:     1,
					Enabled:      true,
				},
			},
		},
	}

	if err := mgr.SaveRouting(ctx, badConfig); err == nil {
		t.Errorf("expected SaveRouting to fail on dangling rule, but it succeeded")
	}
}

func TestManager_ValidateRules_UnknownTarget(t *testing.T) {
	mgr, _ := createTestManager(t)
	ctx := context.Background()

	badConfig := []ServerRoutingConfig{
		{
			Server: "master.test.com",
			Rules: []RoutingRule{
				{
					ID:           "rule-unknown",
					Name:         "Targeting non-existent server",
					SourceServer: "master.test.com",
					TargetServer: "nonexistent-server",
					Domain:       []string{"example.com"},
					Priority:     1,
					Enabled:      true,
				},
			},
		},
	}

	if err := mgr.SaveRouting(ctx, badConfig); err == nil {
		t.Errorf("expected SaveRouting to fail on unknown target, but it succeeded")
	}
}

func TestManager_ValidateRules_SelfLoop(t *testing.T) {
	mgr, _ := createTestManager(t)
	ctx := context.Background()

	badConfig := []ServerRoutingConfig{
		{
			Server: "master.test.com",
			Rules: []RoutingRule{
				{
					ID:           "rule-self",
					Name:         "Self loop",
					SourceServer: "master.test.com",
					TargetServer: "master.test.com", // self-target loop!
					Domain:       []string{"example.com"},
					Priority:     1,
					Enabled:      true,
				},
			},
		},
	}

	if err := mgr.SaveRouting(ctx, badConfig); err == nil {
		t.Errorf("expected SaveRouting to fail on self-targeting loop, but it succeeded")
	}
}

func TestManager_ValidateRules_DuplicateRuleID(t *testing.T) {
	mgr, _ := createTestManager(t)
	ctx := context.Background()

	badConfig := []ServerRoutingConfig{
		{
			Server: "master.test.com",
			Rules: []RoutingRule{
				{
					ID:           "dup-id",
					Name:         "Rule 1",
					SourceServer: "master.test.com",
					TargetServer: "direct",
					Domain:       []string{"example.com"},
					Priority:     1,
					Enabled:      true,
				},
			},
		},
		{
			Server: "msk-slave",
			Rules: []RoutingRule{
				{
					ID:           "dup-id", // duplicate across servers!
					Name:         "Rule 2",
					SourceServer: "msk-slave",
					TargetServer: "direct",
					Domain:       []string{"example.org"},
					Priority:     1,
					Enabled:      true,
				},
			},
		},
	}

	if err := mgr.SaveRouting(ctx, badConfig); err == nil {
		t.Errorf("expected SaveRouting to fail on duplicate rule ID across servers, but it succeeded")
	}
}

func TestManager_ValidateRules_DuplicateServerConfig(t *testing.T) {
	mgr, _ := createTestManager(t)
	ctx := context.Background()

	badConfig := []ServerRoutingConfig{
		{
			Server: "master.test.com",
			Rules:  []RoutingRule{},
		},
		{
			Server: "master.test.com", // duplicate server entry!
			Rules:  []RoutingRule{},
		},
	}

	if err := mgr.SaveRouting(ctx, badConfig); err == nil {
		t.Errorf("expected SaveRouting to fail on duplicate server entry, but it succeeded")
	}
}

func TestManager_ValidateRules_NegativeAndDuplicatePriorities(t *testing.T) {
	mgr, _ := createTestManager(t)
	ctx := context.Background()

	// 1. Negative priority
	negPriorityConfig := []ServerRoutingConfig{
		{
			Server: "master.test.com",
			Rules: []RoutingRule{
				{
					ID:           "neg-1",
					Name:         "Negative priority",
					SourceServer: "master.test.com",
					TargetServer: "direct",
					Domain:       []string{"example.com"},
					Priority:     -1,
					Enabled:      true,
				},
			},
		},
	}
	if err := mgr.SaveRouting(ctx, negPriorityConfig); err == nil {
		t.Errorf("expected SaveRouting to fail on negative priority, but it succeeded")
	}

	// 2. Duplicate priority on same server
	dupPriorityConfig := []ServerRoutingConfig{
		{
			Server: "master.test.com",
			Rules: []RoutingRule{
				{
					ID:           "rule-p1",
					Name:         "First p1",
					SourceServer: "master.test.com",
					TargetServer: "direct",
					Domain:       []string{"example.com"},
					Priority:     5,
					Enabled:      true,
				},
				{
					ID:           "rule-p2",
					Name:         "Second p1",
					SourceServer: "master.test.com",
					TargetServer: "block",
					Domain:       []string{"ads.com"},
					Priority:     5, // duplicate priority on same server!
					Enabled:      true,
				},
			},
		},
	}
	if err := mgr.SaveRouting(ctx, dupPriorityConfig); err == nil {
		t.Errorf("expected SaveRouting to fail on duplicate priority, but it succeeded")
	}
}

func TestManager_ValidateRules_InvalidUTF8_And_ExcessiveLength(t *testing.T) {
	mgr, _ := createTestManager(t)
	ctx := context.Background()

	invalidUTF8 := "\xff\xfe\xfd"

	// Invalid UTF-8 in rule ID
	badIDConfig := []ServerRoutingConfig{
		{
			Server: "master.test.com",
			Rules: []RoutingRule{
				{
					ID:           invalidUTF8,
					Name:         "Valid Name",
					SourceServer: "master.test.com",
					TargetServer: "direct",
					Domain:       []string{"example.com"},
					Priority:     1,
					Enabled:      true,
				},
			},
		},
	}
	if err := mgr.SaveRouting(ctx, badIDConfig); err == nil {
		t.Errorf("expected failure on invalid UTF-8 in ID")
	}

	// Excessive length in rule name (>256)
	longName := strings.Repeat("A", 300)
	longNameConfig := []ServerRoutingConfig{
		{
			Server: "master.test.com",
			Rules: []RoutingRule{
				{
					ID:           "valid-id",
					Name:         longName,
					SourceServer: "master.test.com",
					TargetServer: "direct",
					Domain:       []string{"example.com"},
					Priority:     1,
					Enabled:      true,
				},
			},
		},
	}
	if err := mgr.SaveRouting(ctx, longNameConfig); err == nil {
		t.Errorf("expected failure on excessive name length")
	}

	// Empty domain and empty IP
	emptyCriteriaConfig := []ServerRoutingConfig{
		{
			Server: "master.test.com",
			Rules: []RoutingRule{
				{
					ID:           "empty-crit",
					Name:         "Empty domains and IPs",
					SourceServer: "master.test.com",
					TargetServer: "direct",
					Domain:       []string{"   ", ""},
					IP:           []string{" "},
					Priority:     1,
					Enabled:      true,
				},
			},
		},
	}
	if err := mgr.SaveRouting(ctx, emptyCriteriaConfig); err == nil {
		t.Errorf("expected failure on empty domain and IP slices")
	}
}

func TestManager_ValidateRules_MismatchedSourceServer(t *testing.T) {
	mgr, _ := createTestManager(t)
	ctx := context.Background()

	badConfig := []ServerRoutingConfig{
		{
			Server: "master.test.com",
			Rules: []RoutingRule{
				{
					ID:           "rule-mismatch",
					Name:         "Mismatch source",
					SourceServer: "msk-slave", // mismatched!
					TargetServer: "direct",
					Domain:       []string{"example.com"},
					Priority:     1,
					Enabled:      true,
				},
			},
		},
	}

	if err := mgr.SaveRouting(ctx, badConfig); err == nil {
		t.Errorf("expected SaveRouting to fail on mismatched source server")
	}
}

func TestManager_OutboundTemplates_MissingAndCorrupt(t *testing.T) {
	mgr, tmpDir := createTestManager(t)

	rules := []RoutingRule{
		{
			ID:           "rule-1",
			TargetServer: "msk-slave",
			Domain:       []string{"google.com"},
			Enabled:      true,
		},
	}

	// Missing template
	_, err := mgr.GetRequiredOutbounds("master.test.com", rules)
	if err == nil || !strings.Contains(err.Error(), "outbound template missing") {
		t.Fatalf("expected missing outbound template error, got %v", err)
	}

	// Corrupted JSON template
	corruptedFile := filepath.Join(tmpDir, "outbounds", "msk-slave.json")
	if err := os.WriteFile(corruptedFile, []byte("{invalid json"), 0o644); err != nil {
		t.Fatalf("write file failed: %v", err)
	}

	_, err = mgr.GetRequiredOutbounds("master.test.com", rules)
	if err == nil || !strings.Contains(err.Error(), "invalid JSON") {
		t.Fatalf("expected invalid JSON template error, got %v", err)
	}
}

func TestManager_ApplyToLocalXray_Success(t *testing.T) {
	mgr, tmpDir := createTestManager(t)
	ctx := context.Background()

	// Create valid outbound template for de-slave
	deOutbound := `{"protocol": "freedom", "tag": "relay_de-slave"}`
	if err := os.WriteFile(filepath.Join(tmpDir, "outbounds", "de-slave.json"), []byte(deOutbound), 0o644); err != nil {
		t.Fatalf("failed to write outbound template: %v", err)
	}

	// Create existing local xray config with non-relay outbounds and custom routing options
	initialXray := `{
		"routing": {
			"domainStrategy": "IPIfNonMatch",
			"rules": [
				{"type": "field", "outboundTag": "direct", "ip": ["geoip:private"]}
			]
		},
		"outbounds": [
			{"protocol": "freedom", "tag": "direct"},
			{"protocol": "blackhole", "tag": "block"},
			{"protocol": "vless", "tag": "relay_old-node"}
		]
	}`
	xrayPath := filepath.Join(tmpDir, "xray-config.json")
	if err := os.WriteFile(xrayPath, []byte(initialXray), 0o644); err != nil {
		t.Fatalf("failed to write xray config: %v", err)
	}

	rules := []RoutingRule{
		{
			ID:           "rule-relay-de",
			Name:         "Route to DE",
			TargetServer: "de-slave",
			Domain:       []string{"geosite:netflix"},
			Priority:     1,
			Enabled:      true,
		},
	}

	if err := mgr.ApplyToLocalXray(ctx, xrayPath, rules); err != nil {
		t.Fatalf("ApplyToLocalXray failed: %v", err)
	}

	// Read updated config and verify contents
	updatedData, err := os.ReadFile(xrayPath)
	if err != nil {
		t.Fatalf("failed to read updated xray config: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(updatedData, &parsed); err != nil {
		t.Fatalf("unmarshal updated config failed: %v", err)
	}

	// Verify routing.domainStrategy was preserved
	routingMap := parsed["routing"].(map[string]any)
	if routingMap["domainStrategy"] != "IPIfNonMatch" {
		t.Errorf("expected domainStrategy IPIfNonMatch to be preserved, got %v", routingMap["domainStrategy"])
	}

	// Verify outbounds: old relay_old-node should be purged, direct and block preserved, relay_de-slave appended
	outboundsList := parsed["outbounds"].([]any)
	tags := make([]string, 0, len(outboundsList))
	for _, ob := range outboundsList {
		obMap := ob.(map[string]any)
		tags = append(tags, obMap["tag"].(string))
	}

	expectedTags := []string{"direct", "block", "relay_de-slave"}
	if len(tags) != len(expectedTags) {
		t.Fatalf("expected tags %v, got %v", expectedTags, tags)
	}
	for i, tag := range expectedTags {
		if tags[i] != tag {
			t.Errorf("tag[%d] expected %s, got %s", i, tag, tags[i])
		}
	}
}

func TestManager_ApplyToLocalXray_CorruptConfig(t *testing.T) {
	mgr, tmpDir := createTestManager(t)
	ctx := context.Background()

	corruptXrayPath := filepath.Join(tmpDir, "corrupt-xray.json")
	if err := os.WriteFile(corruptXrayPath, []byte("NOT_JSON"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	err := mgr.ApplyToLocalXray(ctx, corruptXrayPath, []RoutingRule{})
	if err == nil || !strings.Contains(err.Error(), "parse xray config") {
		t.Fatalf("expected parse xray config error, got %v", err)
	}
}

func TestManager_Concurrency_HighLoad(t *testing.T) {
	mgr, tmpDir := createTestManager(t)
	ctx := context.Background()

	// Write outbound templates
	_ = os.WriteFile(filepath.Join(tmpDir, "outbounds", "msk-slave.json"), []byte(`{"tag": "relay_msk-slave"}`), 0o644)
	_ = os.WriteFile(filepath.Join(tmpDir, "outbounds", "de-slave.json"), []byte(`{"tag": "relay_de-slave"}`), 0o644)

	xrayPath := filepath.Join(tmpDir, "xray.json")
	_ = os.WriteFile(xrayPath, []byte(`{"routing":{}, "outbounds":[]}`), 0o644)

	var wg sync.WaitGroup
	workers := 16
	iterations := 25

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				// 1. Save
				cfgs := []ServerRoutingConfig{
					{
						Server: "master.test.com",
						Rules: []RoutingRule{
							{
								ID:           fmt.Sprintf("w%d-r1", workerID),
								Name:         "Concurrent rule",
								SourceServer: "master.test.com",
								TargetServer: "msk-slave",
								Domain:       []string{"example.com"},
								Priority:     workerID + 1,
								Enabled:      true,
							},
						},
					},
				}
				_ = mgr.SaveRouting(ctx, cfgs)

				// 2. Load
				_, _ = mgr.LoadTopology(ctx)

				// 3. Apply
				_ = mgr.ApplyToLocalXray(ctx, xrayPath, cfgs[0].Rules)
			}
		}(w)
	}

	wg.Wait()
}

func TestManager_NilSafety(t *testing.T) {
	var mgr *Manager
	ctx := context.Background()

	if mgr.OutboundsDir() != "" {
		t.Errorf("expected empty string for nil manager OutboundsDir")
	}
	if mgr.ServerConfigPath("master") != "" {
		t.Errorf("expected empty string for nil manager ServerConfigPath")
	}
	if mgr.OutboundTemplatePath("master") != "" {
		t.Errorf("expected empty string for nil manager OutboundTemplatePath")
	}
	if mgr.MasterName() != "master" {
		t.Errorf("expected 'master' for nil manager MasterName")
	}
	if servers := mgr.GetKnownServers(); len(servers) != 0 {
		t.Errorf("expected empty map for nil manager GetKnownServers")
	}
	if _, err := mgr.LoadTopology(ctx); err == nil {
		t.Errorf("expected error calling LoadTopology on nil manager")
	}
	if err := mgr.SaveRouting(ctx, nil); err == nil {
		t.Errorf("expected error calling SaveRouting on nil manager")
	}
	if _, err := mgr.GetRequiredOutbounds("master", nil); err == nil {
		t.Errorf("expected error calling GetRequiredOutbounds on nil manager")
	}
	if err := mgr.ApplyToLocalXray(ctx, "", nil); err == nil {
		t.Errorf("expected error calling ApplyToLocalXray on nil manager")
	}
}

func TestManager_DeterministicSorting(t *testing.T) {
	mgr, _ := createTestManager(t)

	// Rules inserted in random order with same priority or different priorities
	rules := []RoutingRule{
		{ID: "c-rule", TargetServer: "direct", Domain: []string{"c.com"}, Priority: 2, Enabled: true},
		{ID: "a-rule", TargetServer: "direct", Domain: []string{"a.com"}, Priority: 1, Enabled: true},
		{ID: "b-rule", TargetServer: "direct", Domain: []string{"b.com"}, Priority: 1, Enabled: true},
	}

	generated, err := mgr.GenerateXrayRouting("master.test.com", rules)
	if err != nil {
		t.Fatalf("GenerateXrayRouting failed: %v", err)
	}

	// a-rule (P=1, ID=a-rule) should be first, then b-rule (P=1, ID=b-rule), then c-rule (P=2)
	if len(generated) != 3 {
		t.Fatalf("expected 3 generated rules, got %d", len(generated))
	}
	domainsA := generated[0]["domain"].([]string)
	domainsB := generated[1]["domain"].([]string)
	domainsC := generated[2]["domain"].([]string)

	if domainsA[0] != "a.com" || domainsB[0] != "b.com" || domainsC[0] != "c.com" {
		t.Errorf("ordering is not deterministic: %v, %v, %v", domainsA, domainsB, domainsC)
	}
}

func TestManager_ValidateRules_MultiHopCycle(t *testing.T) {
	mgr, _ := createTestManager(t)

	// 1. Disjoint traffic: Master -> MSK (YouTube) and MSK -> Master (Yandex) -> VALID
	validBidirectional := []ServerRoutingConfig{
		{
			Server: "master.test.com",
			Rules: []RoutingRule{
				{ID: "r1", TargetServer: "msk-slave", Domain: []string{"geosite:youtube"}, Priority: 1, Enabled: true},
			},
		},
		{
			Server: "msk-slave",
			Rules: []RoutingRule{
				{ID: "r2", TargetServer: "master.test.com", Domain: []string{"geosite:yandex"}, Priority: 1, Enabled: true},
			},
		},
	}
	if err := mgr.ValidateRules(validBidirectional); err != nil {
		t.Errorf("expected disjoint bidirectional rules to be valid, got: %v", err)
	}

	// 2. Overlapping loop: Master -> MSK -> DE -> Master (all for geosite:youtube) -> MUST FAIL
	cyclicConfigs := []ServerRoutingConfig{
		{
			Server: "master.test.com",
			Rules: []RoutingRule{
				{ID: "r1", TargetServer: "msk-slave", Domain: []string{"geosite:youtube"}, Priority: 1, Enabled: true},
			},
		},
		{
			Server: "msk-slave",
			Rules: []RoutingRule{
				{ID: "r2", TargetServer: "de-slave", Domain: []string{"geosite:youtube"}, Priority: 1, Enabled: true},
			},
		},
		{
			Server: "de-slave",
			Rules: []RoutingRule{
				{ID: "r3", TargetServer: "master.test.com", Domain: []string{"geosite:youtube"}, Priority: 1, Enabled: true},
			},
		},
	}
	if err := mgr.ValidateRules(cyclicConfigs); err == nil {
		t.Errorf("expected multi-hop cycle to be rejected, but passed")
	} else if !strings.Contains(err.Error(), "routing cycle detected") {
		t.Errorf("expected 'routing cycle detected' error, got: %v", err)
	}
}

func TestManager_ValidateRules_OverlappingSubnetAndDomainCycles(t *testing.T) {
	mgr, _ := createTestManager(t)

	// 1. Subnet overlap cycle: master routes 10.0.0.0/16 to msk-slave, msk-slave routes 10.0.1.0/24 to master
	subnetCycle := []ServerRoutingConfig{
		{
			Server: "master.test.com",
			Rules: []RoutingRule{
				{ID: "r1", TargetServer: "msk-slave", IP: []string{"10.0.0.0/16"}, Priority: 1, Enabled: true},
			},
		},
		{
			Server: "msk-slave",
			Rules: []RoutingRule{
				{ID: "r2", TargetServer: "master.test.com", IP: []string{"10.0.1.0/24"}, Priority: 1, Enabled: true},
			},
		},
	}
	if err := mgr.ValidateRules(subnetCycle); err == nil {
		t.Errorf("expected overlapping subnet cycle (10.0.0.0/16 vs 10.0.1.0/24) to be rejected, but got nil")
	} else if !strings.Contains(err.Error(), "routing cycle detected") {
		t.Errorf("expected 'routing cycle detected' error, got: %v", err)
	}

	// 2. Subdomain overlap cycle: master routes example.com to msk-slave, msk-slave routes api.example.com to master
	domainCycle := []ServerRoutingConfig{
		{
			Server: "master.test.com",
			Rules: []RoutingRule{
				{ID: "r1", TargetServer: "msk-slave", Domain: []string{"domain:example.com"}, Priority: 1, Enabled: true},
			},
		},
		{
			Server: "msk-slave",
			Rules: []RoutingRule{
				{ID: "r2", TargetServer: "master.test.com", Domain: []string{"api.example.com"}, Priority: 1, Enabled: true},
			},
		},
	}
	if err := mgr.ValidateRules(domainCycle); err == nil {
		t.Errorf("expected overlapping domain cycle (example.com vs api.example.com) to be rejected, but got nil")
	} else if !strings.Contains(err.Error(), "routing cycle detected") {
		t.Errorf("expected 'routing cycle detected' error, got: %v", err)
	}
}

func TestManager_ApplyToLocalXray_RollbackOnFailure(t *testing.T) {
	mgr, tmpDir := createTestManager(t)
	ctx := context.Background()

	cfgPath := filepath.Join(tmpDir, "xray-config.json")
	originalConfig := `{"routing": {"rules": [{"type": "field", "outboundTag": "direct"}]}}`
	if err := os.WriteFile(cfgPath, []byte(originalConfig), 0o644); err != nil {
		t.Fatalf("failed to write original config: %v", err)
	}

	// Mock restart function to fail
	mgr.SetRestartFunc(func(ctx context.Context) error {
		return fmt.Errorf("systemd failed to start xray")
	})

	rules := []RoutingRule{
		{ID: "rule-1", TargetServer: "direct", Domain: []string{"google.com"}, Priority: 1, Enabled: true},
	}

	err := mgr.ApplyToLocalXray(ctx, cfgPath, rules)
	if err == nil {
		t.Fatalf("expected ApplyToLocalXray to fail when restart fails, but got nil")
	}

	if !strings.Contains(err.Error(), "rolled back") {
		t.Errorf("expected error to mention rollback, got: %v", err)
	}

	// Verify that the file on disk was reverted to original
	data, readErr := os.ReadFile(cfgPath)
	if readErr != nil {
		t.Fatalf("failed to read config after rollback: %v", readErr)
	}
	if string(data) != originalConfig {
		t.Errorf("config was not rolled back properly, content: %s", string(data))
	}
}

func TestManager_ApplyToLocalXray_PreservesStaticAndCascadeRules(t *testing.T) {
	mgr, tmpDir := createTestManager(t)
	ctx := context.Background()

	cfgPath := filepath.Join(tmpDir, "xray-config.json")
	originalConfig := `{
  "routing": {
    "domainStrategy": "IPIfNonMatch",
    "rules": [
      {
        "type": "field",
        "inboundTag": ["api-in"],
        "outboundTag": "api"
      },
      {
        "type": "field",
        "inboundTag": ["xhttp-in-relay-1", "xhttp-in-relay-2", "wg-inbound", "xhttp-cdn-in-NLD"],
        "outboundTag": "relay-NLD"
      },
      {
        "type": "field",
        "protocol": ["bittorrent"],
        "outboundTag": "block"
      }
    ]
  },
  "outbounds": [
    {
      "protocol": "freedom",
      "tag": "direct"
    },
    {
      "protocol": "freedom",
      "tag": "relay-NLD"
    }
  ]
}`
	if err := os.WriteFile(cfgPath, []byte(originalConfig), 0o644); err != nil {
		t.Fatalf("failed to write original config: %v", err)
	}

	rules := []RoutingRule{
		{ID: "rule-1", TargetServer: "direct", Domain: []string{"geosite:youtube"}, Priority: 1, Enabled: true},
	}

	if err := mgr.ApplyToLocalXray(ctx, cfgPath, rules); err != nil {
		t.Fatalf("ApplyToLocalXray failed: %v", err)
	}

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("failed to read updated config: %v", err)
	}

	var parsed struct {
		Routing struct {
			Rules []map[string]any `json:"rules"`
		} `json:"routing"`
		Outbounds []map[string]any `json:"outbounds"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal updated config: %v", err)
	}

	// Should have 4 rules: 1 API + 1 cascade relay + 1 bittorrent + 1 dynamic youtube
	if len(parsed.Routing.Rules) != 4 {
		t.Fatalf("expected 4 routing rules, got %d: %v", len(parsed.Routing.Rules), parsed.Routing.Rules)
	}

	// Rule 0: API rule
	if inbounds, ok := parsed.Routing.Rules[0]["inboundTag"].([]any); !ok || inbounds[0] != "api-in" {
		t.Errorf("expected rule 0 to be api-in, got: %v", parsed.Routing.Rules[0])
	}

	// Rule 1: Cascade relay rule
	if inbounds, ok := parsed.Routing.Rules[1]["inboundTag"].([]any); !ok || inbounds[0] != "xhttp-in-relay-1" {
		t.Errorf("expected rule 1 to be cascade relay, got: %v", parsed.Routing.Rules[1])
	}
	if parsed.Routing.Rules[1]["outboundTag"] != "relay-NLD" {
		t.Errorf("expected rule 1 outboundTag to be relay-NLD, got: %v", parsed.Routing.Rules[1]["outboundTag"])
	}

	// Rule 2: Bittorrent block rule
	if proto, ok := parsed.Routing.Rules[2]["protocol"].([]any); !ok || proto[0] != "bittorrent" {
		t.Errorf("expected rule 2 to be bittorrent, got: %v", parsed.Routing.Rules[2])
	}

	// Rule 3: Dynamic YouTube rule
	if parsed.Routing.Rules[3]["outboundTag"] != "direct" {
		t.Errorf("expected rule 3 outboundTag to be direct, got: %v", parsed.Routing.Rules[3])
	}

	// Outbounds: relay-NLD must be preserved
	foundRelayNLD := false
	for _, ob := range parsed.Outbounds {
		if ob["tag"] == "relay-NLD" {
			foundRelayNLD = true
			break
		}
	}
	if !foundRelayNLD {
		t.Errorf("expected relay-NLD outbound to be preserved in outbounds array")
	}
}
