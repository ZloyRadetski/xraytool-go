package server

import (
	"container/list"
	"crypto/rand"
	"errors"
	"fmt"
	"sync"
	"time"
)

var (
	ErrMaxRequestsReached = errors.New("max requests reached")
	ErrMaxAttemptsReached = errors.New("max attempts reached")
)

// otpEntry holds one pending OTP code for a given identifier (email or telegram_id string).
// The mu guards all fields so concurrent requests for the same identifier are safe.
type otpEntry struct {
	mu           sync.Mutex
	identifier   string
	code         string
	expiresAt    time.Time
	attempts     int
	requestCount int
}

// otpLRUCache is a bounded LRU cache that limits the total number of OTP entries
// to prevent memory exhaustion under a dictionary-attack / OTP-flood attack.
//
// Eviction policy: when the cache is full the oldest (LRU) entry is dropped,
// which is safe because expired entries are harmless and fresh ones take priority.
//
// Access is guarded by a single mutex; contention is acceptable because OTP
// operations are far less frequent than, say, log-line parsing.
type otpLRUCache struct {
	mu      sync.Mutex
	maxSize int
	items   map[string]*list.Element // identifier → list element
	order   *list.List               // front = most recently used, back = LRU
}

func newOTPLRUCache(maxSize int) *otpLRUCache {
	return &otpLRUCache{
		maxSize: maxSize,
		items:   make(map[string]*list.Element, maxSize),
		order:   list.New(),
	}
}

// get retrieves an existing entry and promotes it to the front of the LRU list.
// Returns nil if not found.
func (c *otpLRUCache) get(identifier string) *otpEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[identifier]
	if !ok {
		return nil
	}
	c.order.MoveToFront(el)
	return el.Value.(*otpEntry)
}

// getOrCreate returns an existing entry or creates a new one.
// If the cache is at capacity the least-recently-used entry is evicted first.
func (c *otpLRUCache) getOrCreate(identifier string) *otpEntry {
	c.mu.Lock()
	defer c.mu.Unlock()

	if el, ok := c.items[identifier]; ok {
		c.order.MoveToFront(el)
		return el.Value.(*otpEntry)
	}

	// Evict LRU entry if the cache is full.
	if c.order.Len() >= c.maxSize {
		lru := c.order.Back()
		if lru != nil {
			evicted := lru.Value.(*otpEntry)
			c.order.Remove(lru)
			delete(c.items, evicted.identifier)
		}
	}

	entry := &otpEntry{identifier: identifier}
	el := c.order.PushFront(entry)
	c.items[identifier] = el
	return entry
}

// delete removes an entry from the cache.
func (c *otpLRUCache) delete(identifier string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[identifier]; ok {
		c.order.Remove(el)
		delete(c.items, identifier)
	}
}

// sweepExpired removes all entries whose OTP has expired.
func (c *otpLRUCache) sweepExpired() {
	now := time.Now()
	
	// We need a full lock because we are deleting elements
	c.mu.Lock()
	defer c.mu.Unlock()
	
	for el := c.order.Back(); el != nil; {
		prev := el.Prev()
		entry := el.Value.(*otpEntry)
		
		// The individual entry lock isn't strictly necessary here since we hold the cache-level lock 
		// and we're just reading expiresAt, but it's safe to take it
		entry.mu.Lock()
		expired := now.After(entry.expiresAt)
		entry.mu.Unlock()
		
		if expired {
			c.order.Remove(el)
			delete(c.items, entry.identifier)
		}
		el = prev
	}
}

// otpCache is a process-wide bounded LRU cache: identifier(string) → *otpEntry.
// The limit of 10 000 entries ensures that flooding with fake identifiers cannot
// exhaust memory before the 10-minute sweeper runs.
var otpCache = newOTPLRUCache(10_000)

// generateOTPCode generates a cryptographically random 6-digit code.
func generateOTPCode() string {
	for {
		b := make([]byte, 4)
		rand.Read(b)
		val := int(b[0])<<24 | int(b[1])<<16 | int(b[2])<<8 | int(b[3])
		// Remove modulo bias by only using values below a multiple of 1_000_000
		if val < (0x100000000 - (0x100000000 % 1000000)) {
			return fmt.Sprintf("%06d", val%1000000)
		}
	}
}

// requestOTP generates (or regenerates) an OTP for the given identifier.
//
// identifier can be:
//   - a Telegram ID stringified, e.g. "123456789"
//   - an email address, e.g. "user@example.com"
//
// Rate limits:
//   - at most 2 code requests per TTL window (ErrMaxRequestsReached on violation).
func requestOTP(identifier string, ttl time.Duration) (string, error) {
	entry := otpCache.getOrCreate(identifier)

	entry.mu.Lock()
	defer entry.mu.Unlock()

	now := time.Now()
	// Reset counters when the previous code has expired.
	if now.After(entry.expiresAt) {
		entry.attempts = 0
		entry.requestCount = 0
	}

	if entry.requestCount >= 2 {
		return "", ErrMaxRequestsReached
	}

	entry.code = generateOTPCode()
	entry.expiresAt = now.Add(ttl)
	entry.requestCount++

	return entry.code, nil
}

// verifyOTP checks the code for the given identifier.
//
// Returns:
//   - (true, nil)  — code is correct; entry is deleted immediately (single-use).
//   - (false, nil) — wrong code; attempts counter incremented.
//   - (false, ErrMaxAttemptsReached) — 3rd wrong attempt; entry deleted.
//   - (false, nil) — identifier not found or code expired.
func verifyOTP(identifier, code string) (bool, error) {
	entry := otpCache.get(identifier)
	if entry == nil {
		return false, nil
	}

	entry.mu.Lock()
	defer entry.mu.Unlock()

	if time.Now().After(entry.expiresAt) {
		otpCache.delete(identifier)
		return false, nil
	}

	if entry.code == code {
		otpCache.delete(identifier)
		return true, nil
	}

	entry.attempts++
	if entry.attempts >= 3 {
		otpCache.delete(identifier)
		return false, ErrMaxAttemptsReached
	}

	return false, nil
}

func init() {
	// Background sweeper: removes expired OTPs every 10 minutes to prevent
	// unbounded memory growth without external dependencies.
	// The LRU cap (10 000 entries) is the primary defence; this sweeper is a
	// secondary cleanup to reclaim memory from naturally-expired legitimate codes.
	go func() {
		for {
			time.Sleep(10 * time.Minute)
			otpCache.sweepExpired()
		}
	}()
}
