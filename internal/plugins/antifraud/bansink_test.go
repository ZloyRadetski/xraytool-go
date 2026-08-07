package antifraud_plugin

import (
	"sync"
	"testing"
	"time"
)

type recordingBanSink struct {
	mu     sync.Mutex
	bans   map[string]time.Time
	unbans []string
}

func (s *recordingBanSink) PushBanUpdate(email string, until time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bans == nil {
		s.bans = make(map[string]time.Time)
	}
	s.bans[email] = until
}

func (s *recordingBanSink) PushUnban(email string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.bans, email)
	s.unbans = append(s.unbans, email)
}

func TestBanStoreMirrorsUpdatesToSink(t *testing.T) {
	store := newBanStore()
	sink := &recordingBanSink{}
	store.setSink(sink)

	expiresAt := time.Now().Add(time.Hour)
	store.setBan("user@example.com", expiresAt)
	sink.mu.Lock()
	got := sink.bans["user@example.com"]
	sink.mu.Unlock()
	if !got.Equal(expiresAt) {
		t.Fatalf("sink received expiry %v, want %v", got, expiresAt)
	}

	store.clearBan("user@example.com")
	sink.mu.Lock()
	_, stillBanned := sink.bans["user@example.com"]
	unbans := append([]string(nil), sink.unbans...)
	sink.mu.Unlock()
	if stillBanned || len(unbans) != 1 || unbans[0] != "user@example.com" {
		t.Fatalf("sink did not receive unban, bans=%v unbans=%v", sink.bans, unbans)
	}
}

func TestBanStoreReplaysActiveBansWhenSinkInstalled(t *testing.T) {
	store := newBanStore()
	expiresAt := time.Now().Add(time.Hour)
	store.setBan("user@example.com", expiresAt)

	sink := &recordingBanSink{}
	store.setSink(sink)
	sink.mu.Lock()
	got := sink.bans["user@example.com"]
	sink.mu.Unlock()
	if !got.Equal(expiresAt) {
		t.Fatalf("sink replay expiry %v, want %v", got, expiresAt)
	}
}
