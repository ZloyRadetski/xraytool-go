// Package stats manages per-user cumulative traffic statistics.
//
// Algorithm:
//   - Xray counters are monotonically increasing (reset to 0 on xray restart).
//   - Each update computes a delta = current − previous_raw (or current if counter reset).
//   - Deltas are accumulated in:
//   - cumulative  — running total since we started tracking
//   - buckets     — per-minute time buckets (for detailed history)
//   - archived    — sum of buckets older than retention window
//
// The state is stored in a JSON file (traffic_stats_state.json).
package stats

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
	"xraytool/internal/safeio"
)

// ---------------------------------------------------------------------------
// State types
// ---------------------------------------------------------------------------

// State is the persisted stats snapshot.
type State struct {
	Version                  int                   `json:"version"`
	DetailedRetentionSeconds int64                 `json:"detailed_retention_seconds"`
	LastSampleTS             int64                 `json:"last_sample_ts"`
	Users                    map[string]*UserState `json:"users"`
}

// UserState holds all traffic counters for one user.
type UserState struct {
	Raw        TrafficPoint            `json:"raw"`
	Cumulative TrafficPoint            `json:"cumulative"`
	Archived   TrafficPoint            `json:"archived"`
	Buckets    map[string]TrafficPoint `json:"buckets"`
}

// TrafficPoint holds a matched set of counters.
type TrafficPoint struct {
	Xray  XrayCounters  `json:"xray"`
	Total TotalCounters `json:"total"`
}

// XrayCounters are the raw xray up/down/total bytes.
type XrayCounters struct {
	Up    int64 `json:"up"`
	Down  int64 `json:"down"`
	Total int64 `json:"total"`
}

// TotalCounters are the derived totals (same values, named for API compat).
type TotalCounters struct {
	Up       int64 `json:"up"`
	Down     int64 `json:"down"`
	Combined int64 `json:"combined"`
}

// CumulativeUser is a summary view used by the stats command.
type CumulativeUser struct {
	Email        string
	Xray         XrayCounters
	Total        TotalCounters
	Slave        int64
	ClusterTotal int64
}

// ---------------------------------------------------------------------------
// Constructors
// ---------------------------------------------------------------------------

// NewPoint creates a TrafficPoint from raw up/down values.
func NewPoint(up, down int64) TrafficPoint {
	tot := up + down
	return TrafficPoint{
		Xray:  XrayCounters{Up: up, Down: down, Total: tot},
		Total: TotalCounters{Up: up, Down: down, Combined: tot},
	}
}

// AddPoints adds two TrafficPoints.
func AddPoints(a, b TrafficPoint) TrafficPoint {
	return NewPoint(a.Xray.Up+b.Xray.Up, a.Xray.Down+b.Xray.Down)
}

// DiffPoints computes the delta between current and previous, handling counter resets.
func DiffPoints(curr, prev TrafficPoint) TrafficPoint {
	up := curr.Xray.Up - prev.Xray.Up
	down := curr.Xray.Down - prev.Xray.Down
	if up < 0 {
		up = curr.Xray.Up // counter was reset
	}
	if down < 0 {
		down = curr.Xray.Down
	}
	return NewPoint(up, down)
}

func zeroPoint() TrafficPoint { return NewPoint(0, 0) }

// ---------------------------------------------------------------------------
// Load / Save
// ---------------------------------------------------------------------------

// Load reads the state file, or returns a fresh default state on any error.
func Load(path string, retentionSeconds int64) (*State, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return defaultState(retentionSeconds), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading stats state: %w", err)
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil || s.Users == nil {
		return defaultState(retentionSeconds), nil
	}
	return &s, nil
}

// Save atomically writes the state to path.
func Save(path string, s *State) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating stats dir: %w", err)
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling stats: %w", err)
	}
	if err := safeio.WriteToFile(path, data, 0o644); err != nil {
		return fmt.Errorf("writing stats file: %w", err)
	}
	return nil
}

func defaultState(retentionSeconds int64) *State {
	return &State{
		Version:                  1,
		DetailedRetentionSeconds: retentionSeconds,
		LastSampleTS:             0,
		Users:                    make(map[string]*UserState),
	}
}

// ---------------------------------------------------------------------------
// Update
// ---------------------------------------------------------------------------

// LiveSample is a single traffic snapshot from xray API for one user.
type LiveSample struct {
	Email string
	Up    int64
	Down  int64
}

// Update incorporates new live samples into the state.
func Update(s *State, samples []LiveSample, retentionSeconds int64) {
	now := time.Now().Unix()
	bucketSec := int64(60)
	bucketStart := (now / bucketSec) * bucketSec
	cutoff := now - retentionSeconds

	// Build email→sample lookup.
	live := make(map[string]LiveSample, len(samples))
	for _, ls := range samples {
		live[ls.Email] = ls
	}

	// Collect all known emails.
	all := make(map[string]bool, len(s.Users)+len(live))
	for e := range s.Users {
		all[e] = true
	}
	for e := range live {
		all[e] = true
	}

	if s.Users == nil {
		s.Users = make(map[string]*UserState, len(all))
	}

	bucketKey := fmt.Sprintf("%d", bucketStart)

	for email := range all {
		prev := s.Users[email]
		if prev == nil {
			prev = &UserState{Buckets: make(map[string]TrafficPoint)}
		}
		if prev.Buckets == nil {
			prev.Buckets = make(map[string]TrafficPoint)
		}

		// Prune old buckets → archive.
		recent := make(map[string]TrafficPoint)
		archivedSum := zeroPoint()
		for k, v := range prev.Buckets {
			if ts := parseBucketKey(k); ts < cutoff {
				archivedSum = AddPoints(archivedSum, v)
			} else {
				recent[k] = v
			}
		}
		archived := AddPoints(prev.Archived, archivedSum)

		ls, hasLive := live[email]
		if !hasLive {
			// User not in current live sample (maybe removed from xray).
			s.Users[email] = &UserState{
				Raw:        prev.Raw,
				Cumulative: prev.Cumulative,
				Archived:   archived,
				Buckets:    recent,
			}
			continue
		}

		curr := NewPoint(ls.Up, ls.Down)
		delta := DiffPoints(curr, prev.Raw)

		existing := recent[bucketKey]
		recent[bucketKey] = AddPoints(existing, delta)

		s.Users[email] = &UserState{
			Raw:        curr,
			Cumulative: AddPoints(prev.Cumulative, delta),
			Archived:   archived,
			Buckets:    recent,
		}
	}

	s.LastSampleTS = now
	s.DetailedRetentionSeconds = retentionSeconds
	s.Version = 1
}

// ---------------------------------------------------------------------------
// Read helpers
// ---------------------------------------------------------------------------

// Cumulative returns a flat list of cumulative traffic per user.
func Cumulative(s *State) []CumulativeUser {
	out := make([]CumulativeUser, 0, len(s.Users))
	for email, us := range s.Users {
		cum := us.Cumulative
		out = append(out, CumulativeUser{
			Email: email,
			Xray:  cum.Xray,
			Total: cum.Total,
		})
	}
	return out
}

// ---------------------------------------------------------------------------
// Formatting helpers
// ---------------------------------------------------------------------------

// HumanBytes formats a byte count into a human-readable string.
func HumanBytes(b int64) string {
	const (
		KB = 1 << 10
		MB = 1 << 20
		GB = 1 << 30
	)
	switch {
	case b <= 0:
		return "0B"
	case b < KB:
		return fmt.Sprintf("%dB", b)
	case b < MB:
		return fmt.Sprintf("%.1fK", float64(b)/KB)
	case b < GB:
		return fmt.Sprintf("%.1fM", float64(b)/MB)
	default:
		return fmt.Sprintf("%.2fG", float64(b)/GB)
	}
}

// ---------------------------------------------------------------------------
// Internal
// ---------------------------------------------------------------------------

func parseBucketKey(key string) int64 {
	var ts int64
	fmt.Sscanf(key, "%d", &ts)
	if ts > 100_000_000_000 { // milliseconds → seconds
		ts /= 1000
	}
	return ts
}
