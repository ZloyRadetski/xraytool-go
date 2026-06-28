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

type otpEntry struct {
	mu           sync.Mutex
	code         string
	expiresAt    time.Time
	attempts     int
	requestCount int
}

var (
	otpCache sync.Map
)

// generateOTPCode generates a random 6-digit code
func generateOTPCode() string {
	b := make([]byte, 3)
	rand.Read(b)
	val := (int(b[0])<<16 | int(b[1])<<8 | int(b[2])) % 1000000
	return fmt.Sprintf("%06d", val)
}

func requestOTP(telegramID int64, ttl time.Duration) (string, error) {
	val, loaded := otpCache.LoadOrStore(telegramID, &otpEntry{})
	entry := val.(*otpEntry)

	entry.mu.Lock()
	defer entry.mu.Unlock()

	now := time.Now()
	// Reset counters if expired
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

func verifyOTP(telegramID int64, code string) (bool, error) {
	val, ok := otpCache.Load(telegramID)
	if !ok {
		return false, nil
	}

	entry := val.(*otpEntry)

	entry.mu.Lock()
	defer entry.mu.Unlock()

	if time.Now().After(entry.expiresAt) {
		otpCache.Delete(telegramID)
		return false, nil
	}

	if entry.code == code {
		otpCache.Delete(telegramID)
		return true, nil
	}

	entry.attempts++
	if entry.attempts >= 3 {
		otpCache.Delete(telegramID)
		return false, ErrMaxAttemptsReached
	}

	return false, nil
}

func init() {
	// Background sweeper to remove expired OTPs
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
