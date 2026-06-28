package server

import (
	"crypto/rand"
	"fmt"
	"sync"
	"time"
)

type OTP struct {
	Code      string
	ExpiresAt time.Time
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

func setOTP(telegramID int64, code string, ttl time.Duration) {
	otpCache.Store(telegramID, OTP{
		Code:      code,
		ExpiresAt: time.Now().Add(ttl),
	})
}

func verifyOTP(telegramID int64, code string) bool {
	val, ok := otpCache.Load(telegramID)
	if !ok {
		return false
	}
	otp := val.(OTP)
	if time.Now().After(otp.ExpiresAt) {
		otpCache.Delete(telegramID)
		return false
	}
	if otp.Code == code {
		otpCache.Delete(telegramID)
		return true
	}
	return false
}

func init() {
	// Background sweeper to remove expired OTPs
	go func() {
		for {
			time.Sleep(10 * time.Minute)
			now := time.Now()
			otpCache.Range(func(key, value interface{}) bool {
				otp := value.(OTP)
				if now.After(otp.ExpiresAt) {
					otpCache.Delete(key)
				}
				return true
			})
		}
	}()
}
