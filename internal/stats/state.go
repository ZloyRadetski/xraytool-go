// Package stats manages per-user cumulative traffic statistics.
//
// Algorithm (Simplified & Bulletproof):
// - We fetch raw stats from Xray.
// - If CurrentRaw >= LastRaw, Delta = CurrentRaw - LastRaw
// - If CurrentRaw < LastRaw (Xray restarted/reset counter), Delta = CurrentRaw
// - We add Delta to Cumulative (which never decreases).
// - We save State to JSON.
package stats

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"xraytool/internal/safeio"
)

// State is the persisted stats snapshot.
type State struct {
	Version      int                   `json:"version"`
	LastSampleTS int64                 `json:"last_sample_ts"`
	Users        map[string]*UserState `json:"users"`
}

// UserState holds traffic counters for one user.
type UserState struct {
	CumulativeUp   int64 `json:"cumulative_up"`
	CumulativeDown int64 `json:"cumulative_down"`
	LastRawUp      int64 `json:"last_raw_up"`
	LastRawDown    int64 `json:"last_raw_down"`
}

// XrayCounters are the raw xray up/down/total bytes (kept for API compatibility).
type XrayCounters struct {
	Up    int64 `json:"up"`
	Down  int64 `json:"down"`
	Total int64 `json:"total"`
}

// TotalCounters are the derived totals (kept for API compatibility).
type TotalCounters struct {
	Up       int64 `json:"up"`
	Down     int64 `json:"down"`
	Combined int64 `json:"combined"`
}

// CumulativeUser is a summary view used by the stats command and API.
type CumulativeUser struct {
	Email        string
	Xray         XrayCounters
	Total        TotalCounters
	Slave        int64
	ClusterTotal int64
}

// LiveSample is a single traffic snapshot from xray API for one user.
type LiveSample struct {
	Email string
	Up    int64
	Down  int64
}

// ---------------------------------------------------------------------------
// Load / Save
// ---------------------------------------------------------------------------

// Load reads the state file, or returns a fresh default state on any error.
// The retentionSeconds argument is ignored in this simplified engine but kept for API compatibility.
func Load(path string, _ int64) (*State, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return defaultState(), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading stats state: %w", err)
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return defaultState(), nil
	}

	s := defaultState()
	if v, ok := raw["version"].(float64); ok && v == 2 {
		if err := json.Unmarshal(data, s); err != nil {
			return defaultState(), nil
		}
		if s.Users == nil {
			s.Users = make(map[string]*UserState)
		}
		return s, nil
	}

	// Migrate from v1
	if s.Users == nil {
		s.Users = make(map[string]*UserState)
	}
	if users, ok := raw["users"].(map[string]interface{}); ok {
		for email, uData := range users {
			uMap, ok := uData.(map[string]interface{})
			if !ok {
				continue
			}
			user := &UserState{}

			if cum, ok := uMap["cumulative"].(map[string]interface{}); ok {
				if xray, ok := cum["xray"].(map[string]interface{}); ok {
					if up, ok := xray["up"].(float64); ok {
						user.CumulativeUp = int64(up)
					}
					if down, ok := xray["down"].(float64); ok {
						user.CumulativeDown = int64(down)
					}
				}
			}

			if rawNode, ok := uMap["raw"].(map[string]interface{}); ok {
				if xray, ok := rawNode["xray"].(map[string]interface{}); ok {
					if up, ok := xray["up"].(float64); ok {
						user.LastRawUp = int64(up)
					}
					if down, ok := xray["down"].(float64); ok {
						user.LastRawDown = int64(down)
					}
				}
			}
			s.Users[email] = user
		}
	}
	return s, nil
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

func defaultState() *State {
	return &State{
		Version: 2, // version 2 indicates the new ultra-lightweight engine
		Users:   make(map[string]*UserState),
	}
}

// ---------------------------------------------------------------------------
// Update
// ---------------------------------------------------------------------------

// Update incorporates new live samples into the state.
// The retentionSeconds argument is ignored.
func Update(s *State, samples []LiveSample, _ int64) {
	now := time.Now().Unix()

	if s.Users == nil {
		s.Users = make(map[string]*UserState)
	}

	for _, ls := range samples {
		user, exists := s.Users[ls.Email]
		if !exists {
			user = &UserState{}
			s.Users[ls.Email] = user
		}

		// Calculate Delta Up
		var deltaUp int64
		if ls.Up >= user.LastRawUp {
			deltaUp = ls.Up - user.LastRawUp
		} else {
			// Xray counter was reset (or started from 0 for an inactive user)
			deltaUp = ls.Up
		}

		// Calculate Delta Down
		var deltaDown int64
		if ls.Down >= user.LastRawDown {
			deltaDown = ls.Down - user.LastRawDown
		} else {
			// Xray counter was reset
			deltaDown = ls.Down
		}

		// Add to Cumulative (this never goes down)
		user.CumulativeUp += deltaUp
		user.CumulativeDown += deltaDown

		// Update Last Raw for the next poll
		user.LastRawUp = ls.Up
		user.LastRawDown = ls.Down
	}

	s.LastSampleTS = now
	s.Version = 2
}

// ---------------------------------------------------------------------------
// Read helpers
// ---------------------------------------------------------------------------

// Cumulative returns a flat list of cumulative traffic per user.
func Cumulative(s *State) []CumulativeUser {
	out := make([]CumulativeUser, 0, len(s.Users))
	for email, us := range s.Users {
		tot := us.CumulativeUp + us.CumulativeDown
		out = append(out, CumulativeUser{
			Email: email,
			Xray: XrayCounters{
				Up:    us.CumulativeUp,
				Down:  us.CumulativeDown,
				Total: tot,
			},
			Total: TotalCounters{
				Up:       us.CumulativeUp,
				Down:     us.CumulativeDown,
				Combined: tot,
			},
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
