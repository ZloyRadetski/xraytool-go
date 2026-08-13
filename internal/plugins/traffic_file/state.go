package traffic_file

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	json "github.com/goccy/go-json"

	"xraytool/internal/safeio"
)

const (
	stateLockAttempts   = 50
	stateLockRetry      = 100 * time.Millisecond
	stateLockStaleAfter = 2 * time.Minute
)

// State is the file-backed, cumulative traffic state owned by traffic_file.
type State struct {
	Version      int                   `json:"version"`
	LastSampleTS int64                 `json:"last_sample_ts"`
	Users        map[string]*UserState `json:"users"`
}

type UserState struct {
	CumulativeUp   int64 `json:"cumulative_up"`
	CumulativeDown int64 `json:"cumulative_down"`
	LastRawUp      int64 `json:"last_raw_up"`
	LastRawDown    int64 `json:"last_raw_down"`
}

type XrayCounters struct {
	Up    int64 `json:"up"`
	Down  int64 `json:"down"`
	Total int64 `json:"total"`
}

type TotalCounters struct {
	Up       int64 `json:"up"`
	Down     int64 `json:"down"`
	Combined int64 `json:"combined"`
}

type CumulativeUser struct {
	Email string
	Xray  XrayCounters
	Total TotalCounters
}

type LiveSample struct {
	Email string
	Up    int64
	Down  int64
}

// Load accepts the legacy v1 data shape as well as the current compact state.
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
	state := defaultState()
	if version, ok := raw["version"].(float64); ok && version == 2 {
		if err := json.Unmarshal(data, state); err != nil {
			return defaultState(), nil
		}
		if state.Users == nil {
			state.Users = make(map[string]*UserState)
		}
		return state, nil
	}
	if users, ok := raw["users"].(map[string]interface{}); ok {
		for email, rawUser := range users {
			userMap, ok := rawUser.(map[string]interface{})
			if !ok {
				continue
			}
			user := &UserState{}
			if cumulative, ok := userMap["cumulative"].(map[string]interface{}); ok {
				if xray, ok := cumulative["xray"].(map[string]interface{}); ok {
					if up, ok := xray["up"].(float64); ok {
						user.CumulativeUp = int64(up)
					}
					if down, ok := xray["down"].(float64); ok {
						user.CumulativeDown = int64(down)
					}
				}
			}
			if rawNode, ok := userMap["raw"].(map[string]interface{}); ok {
				if xray, ok := rawNode["xray"].(map[string]interface{}); ok {
					if up, ok := xray["up"].(float64); ok {
						user.LastRawUp = int64(up)
					}
					if down, ok := xray["down"].(float64); ok {
						user.LastRawDown = int64(down)
					}
				}
			}
			state.Users[email] = user
		}
	}
	return state, nil
}

func Save(path string, state *State) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating stats dir: %w", err)
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling stats: %w", err)
	}
	if err := safeio.WriteToFile(path, data, 0o644); err != nil {
		return fmt.Errorf("writing stats file: %w", err)
	}
	return nil
}

// withStateLock serializes one complete read-modify-write operation across
// goroutines and processes. State files are written atomically, but atomic
// replacement alone cannot protect a concurrent Load -> Update -> Save cycle.
func withStateLock(ctx context.Context, statePath string, fn func() error) error {
	if statePath == "" {
		return fmt.Errorf("stats state path is empty")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	lockPath := statePath + ".lock"
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return fmt.Errorf("creating stats lock directory: %w", err)
	}

	for attempt := 0; attempt < stateLockAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			if closeErr := file.Close(); closeErr != nil {
				_ = os.Remove(lockPath)
				return fmt.Errorf("close stats lock file: %w", closeErr)
			}
			defer func() { _ = os.Remove(lockPath) }()
			return fn()
		}
		if !os.IsExist(err) {
			return fmt.Errorf("create stats lock file: %w", err)
		}
		if info, statErr := os.Stat(lockPath); statErr == nil && time.Since(info.ModTime()) > stateLockStaleAfter {
			_ = os.Remove(lockPath)
			continue
		}

		timer := time.NewTimer(stateLockRetry)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
	return fmt.Errorf("timeout waiting for stats lock file")
}

func defaultState() *State { return &State{Version: 2, Users: make(map[string]*UserState)} }

func Update(state *State, samples []LiveSample, _ int64) {
	if state.Users == nil {
		state.Users = make(map[string]*UserState)
	}
	for _, sample := range samples {
		user, ok := state.Users[sample.Email]
		if !ok {
			user = &UserState{}
			state.Users[sample.Email] = user
		}
		if sample.Up >= user.LastRawUp {
			user.CumulativeUp += sample.Up - user.LastRawUp
		} else {
			user.CumulativeUp += sample.Up
		}
		if sample.Down >= user.LastRawDown {
			user.CumulativeDown += sample.Down - user.LastRawDown
		} else {
			user.CumulativeDown += sample.Down
		}
		user.LastRawUp, user.LastRawDown = sample.Up, sample.Down
	}
	state.LastSampleTS, state.Version = time.Now().Unix(), 2
}

func Cumulative(state *State) []CumulativeUser {
	users := make([]CumulativeUser, 0, len(state.Users))
	for email, user := range state.Users {
		total := user.CumulativeUp + user.CumulativeDown
		users = append(users, CumulativeUser{Email: email, Xray: XrayCounters{Up: user.CumulativeUp, Down: user.CumulativeDown, Total: total}, Total: TotalCounters{Up: user.CumulativeUp, Down: user.CumulativeDown, Combined: total}})
	}
	return users
}

func HumanBytes(bytes int64) string {
	const (
		KB = 1 << 10
		MB = 1 << 20
		GB = 1 << 30
	)
	switch {
	case bytes <= 0:
		return "0B"
	case bytes < KB:
		return fmt.Sprintf("%dB", bytes)
	case bytes < MB:
		return fmt.Sprintf("%.1fK", float64(bytes)/KB)
	case bytes < GB:
		return fmt.Sprintf("%.1fM", float64(bytes)/MB)
	default:
		return fmt.Sprintf("%.2fG", float64(bytes)/GB)
	}
}
