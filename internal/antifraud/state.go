// Package antifraud implements a multi-IP fraud detection system.
// It monitors Xray access logs to detect account sharing and applies
// temporary soft-bans without modifying the on-disk Xray configuration.
package antifraud

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

// ipEntry holds the last-seen timestamp for a single IP address.
type ipEntry struct {
	lastSeen time.Time
}

// userIPState stores the set of active IP addresses for one email.
// The map key is the hashed IP string to protect user privacy.
type userIPState struct {
	ips map[string]ipEntry
}

// State is the thread-safe in-memory store for all active IP windows.
//
// Layout:  map[email] → map[ip] → lastSeen
//
// Write path (Analyzer): RWMutex.Lock → update lastSeen → check limit → Unlock
// Read path  (Cleaner):  RWMutex.Lock → delete stale IPs → Unlock
// Read path  (IsBanned): handled separately by the ban store in module.go
//
// Dry-run edge cases verified:
//   - Concurrent AddEvent + Clean: protected by a single mu lock; no deadlock possible
//     because Clean never calls back into State methods.
//   - Empty email/ip: guarded by callers (parser.go guarantees non-empty values).
//   - Large IP churn: Clean runs every 15s, preventing unbounded map growth.
type State struct {
	mu       sync.Mutex
	apiKey   string
	saltDate string
	salt     []byte
	users    map[string]*userIPState
}

// newState allocates an empty State and initializes the salt.
func newState(apiKey string) *State {
	s := &State{
		apiKey: apiKey,
		users:  make(map[string]*userIPState, 64),
	}
	s.updateSaltLocked(time.Now().UTC())
	return s
}

// updateSaltLocked recalculates the salt if the UTC date has changed.
func (s *State) updateSaltLocked(now time.Time) {
	dateStr := now.Format("2006-01-02")
	if s.saltDate == dateStr && s.salt != nil {
		return
	}
	// Deterministic salt: HMAC-SHA256(Date, APIKey)
	mac := hmac.New(sha256.New, []byte(s.apiKey))
	mac.Write([]byte(dateStr))
	s.salt = mac.Sum(nil)
	s.saltDate = dateStr
}

// HashIP computes a salted SHA-256 hash of the IP address.
func (s *State) HashIP(ip string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.updateSaltLocked(time.Now().UTC())
	return s.hashIPLocked(ip)
}

// hashIPLocked computes the hash while the lock is already held.
func (s *State) hashIPLocked(ip string) string {
	h := sha256.New()
	h.Write([]byte(ip))
	h.Write(s.salt)
	return hex.EncodeToString(h.Sum(nil))
}

// AddEvent records a connection event for the given email+IP pair (raw IP).
// Callers: Analyzer.handleEvent (on Master)
func (s *State) AddEvent(email, ip string, ttl time.Duration, now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.updateSaltLocked(now.UTC())
	ipHash := s.hashIPLocked(ip)
	return s.addHashedEventLocked(email, ipHash, ttl, now)
}

// AddHashedEvent records an event where the IP is already hashed (from Slave).
func (s *State) AddHashedEvent(email, ipHash string, ttl time.Duration, now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addHashedEventLocked(email, ipHash, ttl, now)
}

// addHashedEventLocked handles the insertion and pruning.
func (s *State) addHashedEventLocked(email, ipHash string, ttl time.Duration, now time.Time) int {
	u, ok := s.users[email]
	if !ok {
		u = &userIPState{ips: make(map[string]ipEntry, 4)}
		s.users[email] = u
	}

	u.ips[ipHash] = ipEntry{lastSeen: now}

	// Inline TTL pruning on every write to keep the hot path O(active IPs),
	// not O(all ever-seen IPs). This avoids the Clean goroutine having to
	// lock a huge map in bulk.
	cutoff := now.Add(-ttl)
	for addr, e := range u.ips {
		if e.lastSeen.Before(cutoff) {
			delete(u.ips, addr)
		}
	}

	return len(u.ips)
}

// Clean removes IP entries older than ttl for ALL users.
// It is called periodically by the Cleaner goroutine to prevent memory leaks.
// Empty user entries are also pruned.
func (s *State) Clean(ttl time.Duration) {
	cutoff := time.Now().Add(-ttl)

	s.mu.Lock()
	defer s.mu.Unlock()

	for email, u := range s.users {
		for ip, e := range u.ips {
			if e.lastSeen.Before(cutoff) {
				delete(u.ips, ip)
			}
		}
		if len(u.ips) == 0 {
			delete(s.users, email)
		}
	}
}

// ActiveIPCount returns the number of IPs currently tracked for an email.
// Used only in tests / diagnostics.
func (s *State) ActiveIPCount(email string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[email]
	if !ok {
		return 0
	}
	return len(u.ips)
}

// Snapshot returns a debug snapshot of all tracked emails and their IP counts.
// Not used in the hot path — only for diagnostics.
func (s *State) Snapshot() map[string][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	res := make(map[string][]string, len(s.users))
	for email, u := range s.users {
		if len(u.ips) > 0 {
			var hashes []string
			for h := range u.ips {
				hashes = append(hashes, h)
			}
			res[email] = hashes
		}
	}
	return res
}
