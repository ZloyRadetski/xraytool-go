package pluginhost

import (
	"strings"
	"sync"
	"time"
)

// LocalBanCache is the kernel-owned, in-process read model for anti-fraud
// decisions. It implements pluginapi.BanUpdateSink without importing the
// plugin API, so the anti-fraud module and an external provider can both push
// updates into the same hot-path cache.
//
// Reads never perform RPC or database I/O. Expired entries are removed lazily
// to keep the update protocol small and make an unavailable provider fail open
// only after the decision it supplied has expired.
type LocalBanCache struct {
	mu   sync.RWMutex
	bans map[string]time.Time
	now  func() time.Time
}

// NewLocalBanCache creates an empty cache using the system clock.
func NewLocalBanCache() *LocalBanCache {
	return &LocalBanCache{
		bans: make(map[string]time.Time),
		now:  time.Now,
	}
}

// PushBanUpdate records a ban decision supplied by an antifraud provider.
// Empty addresses and already-expired decisions are treated as unbans.
func (c *LocalBanCache) PushBanUpdate(email string, bannedUntil time.Time) {
	if c == nil {
		return
	}
	email = strings.TrimSpace(email)
	if email == "" {
		return
	}
	if !bannedUntil.After(c.clock()) {
		c.PushUnban(email)
		return
	}
	c.mu.Lock()
	c.bans[email] = bannedUntil
	c.mu.Unlock()
}

// PushUnban removes a previously pushed ban decision.
func (c *LocalBanCache) PushUnban(email string) {
	if c == nil {
		return
	}
	email = strings.TrimSpace(email)
	if email == "" {
		return
	}
	c.mu.Lock()
	delete(c.bans, email)
	c.mu.Unlock()
}

// IsBanned performs the subscription hot-path lookup. It is intentionally
// synchronous and local; no provider method is invoked here.
func (c *LocalBanCache) IsBanned(email string) bool {
	if c == nil {
		return false
	}
	email = strings.TrimSpace(email)
	if email == "" {
		return false
	}
	c.mu.RLock()
	expiresAt, ok := c.bans[email]
	c.mu.RUnlock()
	if !ok {
		return false
	}
	if c.clock().Before(expiresAt) {
		return true
	}

	// Best-effort lazy eviction. Do not remove a newer decision that arrived
	// between the RLock above and this write lock.
	c.mu.Lock()
	if current, exists := c.bans[email]; exists && current.Equal(expiresAt) && !c.clock().Before(current) {
		delete(c.bans, email)
	}
	c.mu.Unlock()
	return false
}

func (c *LocalBanCache) clock() time.Time {
	if c.now == nil {
		return time.Now()
	}
	return c.now()
}
