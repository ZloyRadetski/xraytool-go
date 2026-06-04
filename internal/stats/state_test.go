package stats

import (
	"path/filepath"
	"testing"
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

func TestPoints(t *testing.T) {
	p1 := NewPoint(100, 200)
	p2 := NewPoint(50, 100)

	// Test AddPoints
	sum := AddPoints(p1, p2)
	if sum.Xray.Up != 150 || sum.Xray.Down != 300 {
		t.Errorf("AddPoints failed, got: %+v", sum)
	}

	// Test DiffPoints (normal)
	diff := DiffPoints(p1, p2)
	if diff.Xray.Up != 50 || diff.Xray.Down != 100 {
		t.Errorf("DiffPoints failed, got: %+v", diff)
	}

	// Test DiffPoints (reset)
	pReset := NewPoint(10, 20)
	diffReset := DiffPoints(pReset, p1) // Current is smaller than prev -> reset
	if diffReset.Xray.Up != 10 || diffReset.Xray.Down != 20 {
		t.Errorf("DiffPoints with reset failed, got: %+v", diffReset)
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
		Cumulative: NewPoint(1000, 2000),
	}
	err = Save(tmpFile, s)
	if err != nil {
		t.Fatalf("Save error: %v", err)
	}

	s2, err := Load(tmpFile, 3600)
	if err != nil {
		t.Fatalf("Load existing error: %v", err)
	}
	if s2.Users["test@example.com"].Cumulative.Xray.Up != 1000 {
		t.Errorf("Loaded state didn't match saved state")
	}
}

func TestUpdate(t *testing.T) {
	s := defaultState(3600)

	samples := []LiveSample{
		{Email: "user1", Up: 100, Down: 200},
		{Email: "user2", Up: 500, Down: 600},
	}

	// First update
	Update(s, samples, 3600)
	if len(s.Users) != 2 {
		t.Errorf("Expected 2 users, got %d", len(s.Users))
	}
	if s.Users["user1"].Cumulative.Xray.Up != 100 {
		t.Errorf("Expected user1 up 100, got %d", s.Users["user1"].Cumulative.Xray.Up)
	}

	// Second update (increase)
	samples2 := []LiveSample{
		{Email: "user1", Up: 150, Down: 250}, // delta = 50, 50
		{Email: "user2", Up: 500, Down: 600}, // no change
	}
	Update(s, samples2, 3600)

	if s.Users["user1"].Cumulative.Xray.Up != 150 {
		t.Errorf("Expected user1 up 150, got %d", s.Users["user1"].Cumulative.Xray.Up)
	}

	// Third update (reset)
	samples3 := []LiveSample{
		{Email: "user1", Up: 10, Down: 20}, // reset -> delta = 10, 20
	}
	Update(s, samples3, 3600)

	if s.Users["user1"].Cumulative.Xray.Up != 160 { // 150 + 10 = 160
		t.Errorf("Expected user1 cumulative up 160 after reset, got %d", s.Users["user1"].Cumulative.Xray.Up)
	}

	// User2 should still exist but without live updates
	if s.Users["user2"].Cumulative.Xray.Up != 500 {
		t.Errorf("Expected user2 cumulative up 500, got %d", s.Users["user2"].Cumulative.Xray.Up)
	}
}
