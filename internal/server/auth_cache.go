package server

import (
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
	code         string
	expiresAt    time.Time
	attempts     int
	requestCount int
}

// otpCache is a process-wide map: identifier(string) → *otpEntry.
// Using sync.Map so that different identifiers never contend.
var otpCache sync.Map

// generateOTPCode generates a cryptographically random 6-digit code.
func generateOTPCode() string {
	b := make([]byte, 3)
	rand.Read(b) //nolint:errcheck // Read on crypto/rand never fails
	val := (int(b[0])<<16 | int(b[1])<<8 | int(b[2])) % 1_000_000
	return fmt.Sprintf("%06d", val)
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
	val, loaded := otpCache.LoadOrStore(identifier, &otpEntry{})
	entry := val.(*otpEntry)

	entry.mu.Lock()
	defer entry.mu.Unlock()

	now := time.Now()
	// Reset counters when the previous code has expired.
	if loaded && now.After(entry.expiresAt) {
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
	val, ok := otpCache.Load(identifier)
	if !ok {
		return false, nil
	}

	entry := val.(*otpEntry)

	entry.mu.Lock()
	defer entry.mu.Unlock()

	if time.Now().After(entry.expiresAt) {
		otpCache.Delete(identifier)
		return false, nil
	}

	if entry.code == code {
		otpCache.Delete(identifier)
		return true, nil
	}

	entry.attempts++
	if entry.attempts >= 3 {
		otpCache.Delete(identifier)
		return false, ErrMaxAttemptsReached
	}

	return false, nil
}

func init() {
	// Background sweeper: removes expired OTPs every 10 minutes to prevent
	// unbounded memory growth without external dependencies.
	go func() {
		for {
			time.Sleep(10 * time.Minute)
			now := time.Now()
			otpCache.Range(func(key, value interface{}) bool {
				entry := value.(*otpEntry)
				entry.mu.Lock()
				expired := now.After(entry.expiresAt)
				entry.mu.Unlock()
				if expired {
					otpCache.Delete(key)
				}
				return true
			})
		}
	}()
}
