package pluginhost

import (
	"testing"
	"time"
)

func TestLocalBanCachePushAndUnban(t *testing.T) {
	cache := NewLocalBanCache()
	cache.PushBanUpdate("user@example.com", time.Now().Add(time.Minute))
	if !cache.IsBanned("user@example.com") {
		t.Fatal("ban update was not visible in local cache")
	}

	cache.PushUnban("user@example.com")
	if cache.IsBanned("user@example.com") {
		t.Fatal("unban update was not visible in local cache")
	}
}

func TestLocalBanCacheExpiresEntries(t *testing.T) {
	now := time.Now()
	cache := NewLocalBanCache()
	cache.now = func() time.Time { return now }
	cache.PushBanUpdate("user@example.com", now.Add(time.Minute))
	if !cache.IsBanned("user@example.com") {
		t.Fatal("future ban should be active")
	}

	now = now.Add(2 * time.Minute)
	if cache.IsBanned("user@example.com") {
		t.Fatal("expired ban should not be active")
	}
}

func TestLocalBanCacheIgnoresInvalidUpdates(t *testing.T) {
	cache := NewLocalBanCache()
	cache.PushBanUpdate("", time.Now().Add(time.Minute))
	cache.PushBanUpdate("user@example.com", time.Now().Add(-time.Minute))
	if cache.IsBanned("user@example.com") {
		t.Fatal("expired ban must not be stored")
	}
}
