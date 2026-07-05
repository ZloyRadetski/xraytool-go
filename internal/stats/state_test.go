package stats

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"xraytool/internal/domain"
)

func TestHumanBytes(t *testing.T) {
	tests := []struct {
		bytes    int64
		expected string
	}{
		{0, "0B"},
		{500, "500B"},
		{1024, "1.0K"},
		{1048576, "1.0M"},
		{1073741824, "1.00G"},
		{1500000000, "1.40G"},
	}

	for _, tt := range tests {
		actual := HumanBytes(tt.bytes)
		if actual != tt.expected {
			t.Errorf("HumanBytes(%d) expected %s, got %s", tt.bytes, tt.expected, actual)
		}
	}
}

func TestStateLoadSave(t *testing.T) {
	tmpFile := filepath.Join(t.TempDir(), "stats.json")

	// Load non-existent -> default state
	s, err := Load(tmpFile, 3600)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if s == nil || s.Users == nil {
		t.Fatal("Expected default state to be initialized")
	}

	// Save and Load
	s.Users["test@example.com"] = &UserState{
		CumulativeUp:   1000,
		CumulativeDown: 2000,
		LastRawUp:      1000,
		LastRawDown:    2000,
	}
	err = Save(tmpFile, s)
	if err != nil {
		t.Fatalf("Save error: %v", err)
	}

	s2, err := Load(tmpFile, 3600)
	if err != nil {
		t.Fatalf("Load existing error: %v", err)
	}
	if s2.Users["test@example.com"].CumulativeUp != 1000 {
		t.Errorf("Loaded state didn't match saved state")
	}
}

func TestUpdate_Normal(t *testing.T) {
	s := defaultState()

	samples := []LiveSample{
		{Email: "user1", Up: 100, Down: 200},
		{Email: "user2", Up: 500, Down: 600},
	}

	// First update (from 0)
	Update(s, samples, 3600)
	if len(s.Users) != 2 {
		t.Errorf("Expected 2 users, got %d", len(s.Users))
	}
	if s.Users["user1"].CumulativeUp != 100 {
		t.Errorf("Expected user1 up 100, got %d", s.Users["user1"].CumulativeUp)
	}

	// Second update (increase)
	samples2 := []LiveSample{
		{Email: "user1", Up: 150, Down: 250}, // delta = 50, 50
		{Email: "user2", Up: 500, Down: 600}, // no change
	}
	Update(s, samples2, 3600)

	if s.Users["user1"].CumulativeUp != 150 { // 100 + 50
		t.Errorf("Expected user1 up 150, got %d", s.Users["user1"].CumulativeUp)
	}
	if s.Users["user1"].LastRawUp != 150 {
		t.Errorf("Expected user1 LastRawUp 150, got %d", s.Users["user1"].LastRawUp)
	}

	// user2 had no change in raw stats -> cumulative should not change
	if s.Users["user2"].CumulativeUp != 500 {
		t.Errorf("Expected user2 cumulative up 500, got %d", s.Users["user2"].CumulativeUp)
	}
}

func TestUpdate_XrayReset(t *testing.T) {
	s := defaultState()

	// Initial stats
	samples := []LiveSample{
		{Email: "user1", Up: 1000, Down: 2000},
	}
	Update(s, samples, 3600)

	// Xray restarts, stats start from 0 and reach 10, 20
	samplesReset := []LiveSample{
		{Email: "user1", Up: 10, Down: 20}, // reset -> delta = 10, 20
	}
	Update(s, samplesReset, 3600)

	if s.Users["user1"].CumulativeUp != 1010 { // 1000 + 10
		t.Errorf("Expected user1 cumulative up 1010 after reset, got %d", s.Users["user1"].CumulativeUp)
	}
	if s.Users["user1"].LastRawUp != 10 {
		t.Errorf("Expected user1 LastRawUp 10 after reset, got %d", s.Users["user1"].LastRawUp)
	}
}

func TestUpdate_UserMissing(t *testing.T) {
	s := defaultState()

	// Initial stats for user1 and user2
	samples := []LiveSample{
		{Email: "user1", Up: 100, Down: 200},
		{Email: "user2", Up: 100, Down: 200},
	}
	Update(s, samples, 3600)

	// Xray reports only user1 (user2 was pruned from Xray's memory due to inactivity)
	samples2 := []LiveSample{
		{Email: "user1", Up: 150, Down: 250}, // user1 uses 50,50
	}
	Update(s, samples2, 3600)

	if s.Users["user2"].CumulativeUp != 100 {
		t.Errorf("Missing user2 should freeze cumulative stats at 100, got %d", s.Users["user2"].CumulativeUp)
	}
	if s.Users["user2"].LastRawUp != 100 {
		t.Errorf("Missing user2 should freeze LastRawUp at 100, got %d", s.Users["user2"].LastRawUp)
	}

	// user2 reconnects and Xray starts counting them from 0 again (because they were pruned)
	samples3 := []LiveSample{
		{Email: "user1", Up: 150, Down: 250},
		{Email: "user2", Up: 10, Down: 20}, // delta = 10, 20
	}
	Update(s, samples3, 3600)

	if s.Users["user2"].CumulativeUp != 110 { // 100 + 10
		t.Errorf("Expected user2 cumulative up 110, got %d", s.Users["user2"].CumulativeUp)
	}
}

type mockEngine struct {
	domain.Engine
}

func (m *mockEngine) QueryStats(ctx context.Context) ([]domain.TrafficStat, error) {
	return nil, nil
}
func (m *mockEngine) ListUsers(ctx context.Context) ([]domain.VPNUserConfig, error) {
	return nil, nil
}

type mockClusterProvider struct {
	totals []domain.SlaveUserTotal
}

func (m *mockClusterProvider) CollectSlaveTotals() ([]domain.SlaveUserTotal, domain.SlaveReport) {
	return m.totals, domain.SlaveReport{Enabled: true, TotalServers: 1, OKServers: 1}
}

func TestService_GenerateClusterStats_SavesInferred(t *testing.T) {
	tmpDir := t.TempDir()
	statsStateFile := filepath.Join(tmpDir, "stats_state.json")
	inferredStatsFile := filepath.Join(tmpDir, "inferred_traffic.json")

	// Pre-create StatsState with master traffic
	state := defaultState()
	state.Users["user1@example.com"] = &UserState{
		CumulativeUp:   100,
		CumulativeDown: 200,
	}
	if err := Save(statsStateFile, state); err != nil {
		t.Fatalf("Failed to save initial state: %v", err)
	}

	engine := &mockEngine{}
	clusterProvider := &mockClusterProvider{
		totals: []domain.SlaveUserTotal{
			{Email: "user1@example.com", Slave: 500},
		},
	}

	svc := NewService(Config{
		IsMaster:                 true,
		StatsStatePath:           statsStateFile,
		InferredStatsPath:        inferredStatsFile,
		DetailedRetentionSeconds: 3600,
	}, engine, clusterProvider)

	// Call GenerateClusterStats (inferredMode = false)
	merged, _, err := svc.GenerateClusterStats(false, statsStateFile)
	if err != nil {
		t.Fatalf("GenerateClusterStats error: %v", err)
	}

	// Verify merged stats returned
	if len(merged) != 1 {
		t.Fatalf("Expected 1 merged user, got %d", len(merged))
	}
	mu := merged[0]
	if mu.Email != "user1@example.com" {
		t.Errorf("Expected user1@example.com, got %s", mu.Email)
	}
	if mu.Total.Up != 100 || *mu.ClusterTotal != 800 {
		t.Errorf("Expected Total Up=100, Combined=800, got Up=%d, Combined=%d", mu.Total.Up, *mu.ClusterTotal)
	}

	// Verify InferredStats file was written
	if _, err := os.Stat(inferredStatsFile); os.IsNotExist(err) {
		t.Fatalf("Inferred stats file was not created: %s", inferredStatsFile)
	}

	inferredState, err := Load(inferredStatsFile, 3600)
	if err != nil {
		t.Fatalf("Failed to load inferred stats file: %v", err)
	}

	uState, exists := inferredState.Users["user1@example.com"]
	if !exists {
		t.Fatal("Expected user1@example.com in inferred stats file")
	}

	// CumulativeUp should be 100 (master up)
	// CumulativeDown should be 200 + 500 = 700 (master down + slave combined)
	if uState.CumulativeUp != 100 || uState.CumulativeDown != 700 {
		t.Errorf("Expected CumulativeUp=100 and CumulativeDown=700 in inferred file, got %d and %d", uState.CumulativeUp, uState.CumulativeDown)
	}
}
