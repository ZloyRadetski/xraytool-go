// package antifraud_plugin implements a multi-IP fraud detection system.
// It monitors Xray access logs to detect account sharing and applies
// temporary soft-bans without modifying the on-disk Xray configuration.
package antifraud_plugin

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/netip"
	"strings"
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
	mu         sync.Mutex
	hashSecret []byte
	users      map[string]*userIPState
}

// newState allocates an empty State. hashSecret must be shared by every node
// in a cluster. It is deliberately stable for the full IP-limit window: a
// daily salt made one address become two different identities at UTC midnight.
func newState(hashSecret string) *State {
	return &State{
		hashSecret: []byte(hashSecret),
		users:      make(map[string]*userIPState, 64),
	}
}

// HashIP canonicalizes an address and returns its cluster-stable HMAC. The
// source address never leaves a slave, while IPv4-mapped IPv6 and alternate
// IPv6 spellings resolve to one identity on every node.
//
// An empty result means ip was not a valid IP literal and must be ignored.
func (s *State) HashIP(ip string) string {
	canonical, ok := canonicalIP(ip)
	if !ok {
		return ""
	}
	mac := hmac.New(sha256.New, s.hashSecret)
	_, _ = mac.Write([]byte(canonical))
	return hex.EncodeToString(mac.Sum(nil))
}

// HashKeyID returns a non-secret identifier that operators can compare across
// master and slaves. It intentionally hashes a fixed label rather than an IP.
func (s *State) HashKeyID() string {
	mac := hmac.New(sha256.New, s.hashSecret)
	_, _ = mac.Write([]byte("xraytool-antifraud-key-id-v2"))
	return hex.EncodeToString(mac.Sum(nil))[:16]
}

func canonicalIP(raw string) (string, bool) {
	address, err := netip.ParseAddr(strings.TrimSpace(raw))
	if err != nil || !address.IsValid() {
		return "", false
	}
	return address.Unmap().String(), true
}

// AddEvent records a connection event for the given email+IP pair (raw IP).
// Callers: Analyzer.handleEvent (on Master)
func (s *State) AddEvent(email, ip string, ttl time.Duration, now time.Time) int {
	ipHash := s.HashIP(ip)
	if ipHash == "" {
		return s.ActiveIPCount(email)
	}
	return s.AddHashedEvent(email, ipHash, ttl, now)
}

// AddHashedEvent records an event where the IP is already hashed (from Slave).
func (s *State) AddHashedEvent(email, ipHash string, ttl time.Duration, now time.Time) int {
	if ipHash == "" {
		return s.ActiveIPCount(email)
	}
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

	// Out-of-order replay must not make a newer observation look older.
	if previous, exists := u.ips[ipHash]; !exists || previous.lastSeen.Before(now) {
		u.ips[ipHash] = ipEntry{lastSeen: now}
	}

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
