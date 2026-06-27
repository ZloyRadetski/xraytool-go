package antifraud

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// parseLine tests (Zero-Allocation Parser)
// ---------------------------------------------------------------------------

func TestParseLine_AcceptedIPv4(t *testing.T) {
	line := []byte("2024/01/15 12:34:56 1.2.3.4:56789 accepted tcp:8.8.8.8:443 [inbound] user@example.com")
	ip, email := parseLine(line)
	require.NotNil(t, ip, "ip should not be nil")
	require.NotNil(t, email, "email should not be nil")
	assert.Equal(t, "1.2.3.4", string(ip))
	assert.Equal(t, "user@example.com", string(email))
}

func TestParseLine_AcceptedIPv6(t *testing.T) {
	line := []byte("2024/01/15 12:34:56 [::1]:56789 accepted tcp:8.8.8.8:443 [inbound] user2@example.com")
	ip, email := parseLine(line)
	require.NotNil(t, ip)
	require.NotNil(t, email)
	assert.Equal(t, "::1", string(ip))
	assert.Equal(t, "user2@example.com", string(email))
}

func TestParseLine_AcceptedUDP(t *testing.T) {
	line := []byte("2024/01/15 12:34:56 5.6.7.8:56789 accepted udp:8.8.8.8:53 [inbound] vpnuser")
	ip, email := parseLine(line)
	require.NotNil(t, ip)
	assert.Equal(t, "5.6.7.8", string(ip))
	assert.Equal(t, "vpnuser", string(email))
}

func TestParseLine_NoAcceptedTag_ReturnsNil(t *testing.T) {
	line := []byte("2024/01/15 12:34:56 rejected tcp:1.2.3.4:12345 [inbound] user@example.com")
	ip, email := parseLine(line)
	assert.Nil(t, ip)
	assert.Nil(t, email)
}

func TestParseLine_EmptyLine_ReturnsNil(t *testing.T) {
	ip, email := parseLine([]byte{})
	assert.Nil(t, ip)
	assert.Nil(t, email)
}

func TestParseLine_MissingEmail_ReturnsNil(t *testing.T) {
	// Only address field, no trailing token for email
	line := []byte("2024/01/15 12:34:56 1.2.3.4:56789 accepted tcp:8.8.8.8:443")
	ip, email := parseLine(line)
	// spaceIdx will be -1 after the address, so parseLine returns nil
	assert.Nil(t, ip)
	assert.Nil(t, email)

	// Incomplete line
	line = []byte("2024/01/15 12:34:56 accepted tcp:1.2.3.4:12345")
	ip, email = parseLine(line)
	assert.Nil(t, ip)
	assert.Nil(t, email)
}

func TestParseLine_RealXrayLogs(t *testing.T) {
	line1 := []byte("2026/06/27 21:09:15.937557 from 188.65.244.206:60770 accepted tcp:www.google.com:80 [hy2 -> warp] email: bot_client_7912770979")
	ip, email := parseLine(line1)
	assert.Equal(t, "188.65.244.206", string(ip))
	assert.Equal(t, "bot_client_7912770979", string(email))

	line2 := []byte("2026/06/27 21:09:16.926077 from 62.33.196.224:59925 accepted tcp:sf16-teko2.tiktokcdn.com:443 [reality-in-1 -> direct] email: bot_client_5086550015")
	ip, email = parseLine(line2)
	assert.Equal(t, "62.33.196.224", string(ip))
	assert.Equal(t, "bot_client_5086550015", string(email))
}

// ---------------------------------------------------------------------------
// stripPort tests
// ---------------------------------------------------------------------------

func TestStripPort_IPv4(t *testing.T) {
	assert.Equal(t, []byte("192.168.1.1"), stripPort([]byte("192.168.1.1:8080")))
}

func TestStripPort_IPv6Brackets(t *testing.T) {
	assert.Equal(t, []byte("::1"), stripPort([]byte("[::1]:443")))
}

func TestStripPort_IPv6WithPort(t *testing.T) {
	// Xray logs IPv6 addresses in [host]:port format. Bare IPv6 without
	// brackets is not produced by Xray but we verify graceful handling:
	// last-colon strip gives a truncated result, which the caller will
	// discard as an invalid IP — acceptable behavior.
	result := stripPort([]byte("2001:db8::1:443"))
	assert.Equal(t, []byte("2001:db8::1"), result)
}

func TestStripPort_Empty(t *testing.T) {
	assert.Nil(t, stripPort([]byte{}))
}

// ---------------------------------------------------------------------------
// State tests (thread-safety checked via -race in go test)
// ---------------------------------------------------------------------------

func TestState_AddEvent_CountsUniqueIPs(t *testing.T) {
	s := newState()
	ttl := 5 * time.Minute
	now := time.Now()

	// Add 3 unique IPs for the same email.
	s.AddEvent("user1@x.com", "1.1.1.1", ttl, now)
	s.AddEvent("user1@x.com", "2.2.2.2", ttl, now)
	count := s.AddEvent("user1@x.com", "3.3.3.3", ttl, now)

	assert.Equal(t, 3, count)
	assert.Equal(t, 3, s.ActiveIPCount("user1@x.com"))
}

func TestState_AddEvent_SameIPNotDoubled(t *testing.T) {
	s := newState()
	ttl := 5 * time.Minute
	now := time.Now()

	s.AddEvent("u@x.com", "1.1.1.1", ttl, now)
	s.AddEvent("u@x.com", "1.1.1.1", ttl, now)
	count := s.AddEvent("u@x.com", "1.1.1.1", ttl, now)

	assert.Equal(t, 1, count, "same IP must not inflate the count")
}

func TestState_AddEvent_StaleIPsPruned(t *testing.T) {
	s := newState()
	shortTTL := 50 * time.Millisecond

	past := time.Now().Add(-1 * time.Second) // already expired
	s.AddEvent("u@x.com", "old.ip", shortTTL, past)

	now := time.Now()
	count := s.AddEvent("u@x.com", "new.ip", shortTTL, now)

	// old.ip must have been pruned during the AddEvent call.
	assert.Equal(t, 1, count, "stale IPs should be evicted inline")
}

func TestState_Clean_RemovesExpiredEntries(t *testing.T) {
	s := newState()
	ttl := 50 * time.Millisecond

	past := time.Now().Add(-1 * time.Second)
	s.AddEvent("u@x.com", "1.1.1.1", 10*time.Minute, past) // force insert without ttl pruning

	// Manually insert stale entry by bypassing AddEvent inline pruning.
	// We test Clean in isolation here.
	s.mu.Lock()
	s.users["u@x.com"].ips["1.1.1.1"] = ipEntry{lastSeen: past}
	s.mu.Unlock()

	s.Clean(ttl)
	assert.Equal(t, 0, s.ActiveIPCount("u@x.com"))
}

func TestState_Clean_RemovesEmptyUserEntries(t *testing.T) {
	s := newState()
	ttl := 1 * time.Millisecond

	past := time.Now().Add(-1 * time.Second)
	s.mu.Lock()
	s.users["ghost@x.com"] = &userIPState{ips: map[string]ipEntry{
		"1.1.1.1": {lastSeen: past},
	}}
	s.mu.Unlock()

	time.Sleep(5 * time.Millisecond)
	s.Clean(ttl)

	snap := s.Snapshot()
	_, exists := snap["ghost@x.com"]
	assert.False(t, exists, "empty user entries must be pruned by Clean")
}

func TestState_MultipleUsers_Isolated(t *testing.T) {
	s := newState()
	ttl := 5 * time.Minute
	now := time.Now()

	s.AddEvent("alice@x.com", "1.1.1.1", ttl, now)
	s.AddEvent("alice@x.com", "2.2.2.2", ttl, now)
	s.AddEvent("bob@x.com", "9.9.9.9", ttl, now)

	assert.Equal(t, 2, s.ActiveIPCount("alice@x.com"))
	assert.Equal(t, 1, s.ActiveIPCount("bob@x.com"))
}

// ---------------------------------------------------------------------------
// banStore tests
// ---------------------------------------------------------------------------

func TestBanStore_SetAndIsBanned(t *testing.T) {
	bs := newBanStore()
	bs.setBan("test@x.com", time.Now().Add(10*time.Minute))
	assert.True(t, bs.isBanned("test@x.com"))
}

func TestBanStore_ClearBan(t *testing.T) {
	bs := newBanStore()
	bs.setBan("test@x.com", time.Now().Add(10*time.Minute))
	bs.clearBan("test@x.com")
	assert.False(t, bs.isBanned("test@x.com"))
}

func TestBanStore_ExpiredBanNotBanned(t *testing.T) {
	bs := newBanStore()
	bs.setBan("old@x.com", time.Now().Add(-1*time.Minute)) // already expired
	assert.False(t, bs.isBanned("old@x.com"))
}

func TestBanStore_UnknownEmail_NotBanned(t *testing.T) {
	bs := newBanStore()
	assert.False(t, bs.isBanned("nobody@x.com"))
}

// ---------------------------------------------------------------------------
// Parser benchmark (validates zero-allocation claim)
// ---------------------------------------------------------------------------

func BenchmarkParseLine(b *testing.B) {
	line := []byte("2024/01/15 12:34:56 203.0.113.42:56789 accepted tcp:8.8.8.8:443 [vless-443] client@example.com")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		parseLine(line)
	}
}

func BenchmarkState_AddEvent(b *testing.B) {
	s := newState()
	ttl := 3 * time.Minute
	ips := []string{"1.1.1.1", "2.2.2.2", "3.3.3.3", "4.4.4.4"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.AddEvent("bench@x.com", ips[i%len(ips)], ttl, time.Now())
	}
}
