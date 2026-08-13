// Package identity_memory is the default one-time-code provider. It retains
// the original single-process semantics while making the store replaceable by
// a Redis, SSO, or external identity plugin.
package identity_memory

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"fmt"
	"math/big"
	"sync"
	"time"

	"xraytool/internal/pluginapi"
)

const maxEntries = 10_000

type entry struct {
	code         string
	payload      string
	expiresAt    time.Time
	requests     int
	attempts     int
	lastAccessed time.Time
}

type Plugin struct {
	mu      sync.Mutex
	entries map[string]*entry
}

func New() *Plugin { return &Plugin{entries: make(map[string]*entry)} }

func (p *Plugin) Metadata() pluginapi.Metadata {
	return pluginapi.Metadata{
		Name:        "identity_memory",
		Kind:        "identity",
		Version:     "1.0.0",
		APIVersion:  pluginapi.CurrentAPIVersion,
		Description: "Bounded in-memory OTP and session verification provider.",
		Publishes:   []pluginapi.ServiceRef{{Name: pluginapi.ServiceIdentityProvider}},
	}
}

func (p *Plugin) Init(_ context.Context, _ pluginapi.RawConfig, _ pluginapi.ServiceResolver) error {
	return nil
}
func (p *Plugin) Start(ctx context.Context) error { <-ctx.Done(); return nil }
func (p *Plugin) Stop(_ context.Context) error    { return nil }
func (p *Plugin) Health(_ context.Context) error  { return nil }

func (p *Plugin) PublishedServices() map[string]any {
	return map[string]any{pluginapi.ServiceIdentityProvider: pluginapi.IdentityProvider(p)}
}

func (p *Plugin) IssueOTP(_ context.Context, subject, payload string, ttl time.Duration) (string, error) {
	if subject == "" || ttl <= 0 {
		return "", fmt.Errorf("identity_memory: subject and positive ttl are required")
	}
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sweepLocked(now)
	item := p.entries[subject]
	if item == nil {
		p.evictOneLocked()
		item = &entry{}
		p.entries[subject] = item
	}
	if now.After(item.expiresAt) {
		item.requests = 0
		item.attempts = 0
	}
	if item.requests >= 2 {
		return "", pluginapi.ErrIdentityRateLimited
	}
	code, err := generateCode()
	if err != nil {
		return "", err
	}
	item.code = code
	item.payload = payload
	item.expiresAt = now.Add(ttl)
	item.requests++
	item.lastAccessed = now
	return code, nil
}

func (p *Plugin) VerifyOTP(_ context.Context, subject, code string) (bool, string, error) {
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	item := p.entries[subject]
	if item == nil || now.After(item.expiresAt) {
		delete(p.entries, subject)
		return false, "", nil
	}
	item.lastAccessed = now
	if subtle.ConstantTimeCompare([]byte(item.code), []byte(code)) == 1 {
		payload := item.payload
		delete(p.entries, subject)
		return true, payload, nil
	}
	item.attempts++
	if item.attempts >= 3 {
		delete(p.entries, subject)
		return false, "", pluginapi.ErrIdentityAttemptsExceeded
	}
	return false, "", nil
}

func (p *Plugin) sweepLocked(now time.Time) {
	for subject, item := range p.entries {
		if now.After(item.expiresAt) {
			delete(p.entries, subject)
		}
	}
}

func (p *Plugin) evictOneLocked() {
	if len(p.entries) < maxEntries {
		return
	}
	var oldestSubject string
	var oldest time.Time
	for subject, item := range p.entries {
		if oldestSubject == "" || item.lastAccessed.Before(oldest) {
			oldestSubject, oldest = subject, item.lastAccessed
		}
	}
	delete(p.entries, oldestSubject)
}

func generateCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", fmt.Errorf("identity_memory: generate OTP: %w", err)
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

var _ pluginapi.Plugin = (*Plugin)(nil)
var _ pluginapi.ServiceProvider = (*Plugin)(nil)
var _ pluginapi.IdentityProvider = (*Plugin)(nil)
