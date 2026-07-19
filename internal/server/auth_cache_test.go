package server

import (
	"testing"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// Tests use string identifiers — both telegram_id strings and email strings.
// ─────────────────────────────────────────────────────────────────────────────

func TestRequestOTP_TelegramID(t *testing.T) {
	id := "1111"
	otpCache.delete(id)

	// 1st request — must succeed
	code1, err := requestOTP(id, 5*time.Minute)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if len(code1) != 6 {
		t.Fatalf("expected 6-digit code, got %q", code1)
	}

	// 2nd request (resend) — must succeed with a different code
	code2, err := requestOTP(id, 5*time.Minute)
	if err != nil {
		t.Fatalf("expected nil error on 2nd request, got %v", err)
	}
	if code1 == code2 {
		t.Fatal("expected different codes for each request")
	}

	// 3rd request — must be rate-limited
	_, err = requestOTP(id, 5*time.Minute)
	if err != ErrMaxRequestsReached {
		t.Fatalf("expected ErrMaxRequestsReached, got %v", err)
	}
}

func TestRequestOTP_Email(t *testing.T) {
	id := "user@example.com"
	otpCache.delete(id)

	code1, err := requestOTP(id, 5*time.Minute)
	if err != nil {
		t.Fatalf("email OTP request 1 failed: %v", err)
	}
	if len(code1) != 6 {
		t.Fatalf("expected 6-digit code, got %q", code1)
	}

	code2, err := requestOTP(id, 5*time.Minute)
	if err != nil {
		t.Fatalf("email OTP request 2 failed: %v", err)
	}
	if code1 == code2 {
		t.Fatal("expected different codes")
	}

	_, err = requestOTP(id, 5*time.Minute)
	if err != ErrMaxRequestsReached {
		t.Fatalf("expected ErrMaxRequestsReached, got %v", err)
	}
}

func TestVerifyOTP_BruteForce(t *testing.T) {
	id := "brute@example.com"
	otpCache.delete(id)

	code, _ := requestOTP(id, 5*time.Minute)

	for i, wrong := range []string{"000000", "000001", "000002"} {
		ok, _, err := verifyOTP(id, wrong)
		if ok {
			t.Fatalf("attempt %d: expected ok=false", i+1)
		}
		if i < 2 && err != nil {
			t.Fatalf("attempt %d: expected nil error before max, got %v", i+1, err)
		}
		if i == 2 && err != ErrMaxAttemptsReached {
			t.Fatalf("attempt 3: expected ErrMaxAttemptsReached, got %v", err)
		}
	}

	// Entry must be deleted — correct code no longer works
	ok, _, err := verifyOTP(id, code)
	if ok || err != nil {
		t.Fatalf("expected false,nil after deletion; got %v,%v", ok, err)
	}
}

func TestVerifyOTP_Success_TelegramID(t *testing.T) {
	id := "3333"
	otpCache.delete(id)

	code, _ := requestOTP(id, 5*time.Minute)

	ok, _, err := verifyOTP(id, code)
	if !ok || err != nil {
		t.Fatalf("expected true,nil; got %v,%v", ok, err)
	}

	// Entry must be deleted after successful verify (single-use)
	stillPresent := otpCache.get(id) != nil
	if stillPresent {
		t.Fatal("entry should be deleted after successful verify")
	}
}

func TestVerifyOTP_Success_Email(t *testing.T) {
	id := "success@example.com"
	otpCache.delete(id)

	code, _ := requestOTP(id, 5*time.Minute)

	ok, _, err := verifyOTP(id, code)
	if !ok || err != nil {
		t.Fatalf("expected true,nil; got %v,%v", ok, err)
	}

	stillPresent := otpCache.get(id) != nil
	if stillPresent {
		t.Fatal("entry should be deleted after successful verify")
	}
}

func TestVerifyOTP_Expired(t *testing.T) {
	id := "expired@example.com"
	otpCache.delete(id)

	code, _ := requestOTP(id, 1*time.Millisecond)
	time.Sleep(10 * time.Millisecond)

	ok, _, err := verifyOTP(id, code)
	if ok || err != nil {
		t.Fatalf("expected false,nil for expired code; got %v,%v", ok, err)
	}
}

func TestVerifyOTP_NotFound(t *testing.T) {
	id := "ghost@example.com"
	otpCache.delete(id) // ensure not present

	ok, _, err := verifyOTP(id, "123456")
	if ok || err != nil {
		t.Fatalf("expected false,nil for unknown identifier; got %v,%v", ok, err)
	}
}

func TestRequestOTP_ResetsAfterExpiry(t *testing.T) {
	id := "reset@example.com"
	otpCache.delete(id)

	// Exhaust the 2-request limit with a very short TTL
	_, _ = requestOTP(id, 1*time.Millisecond)
	_, _ = requestOTP(id, 1*time.Millisecond)
	_, err := requestOTP(id, 1*time.Millisecond)
	if err != ErrMaxRequestsReached {
		t.Fatalf("expected rate limit, got %v", err)
	}

	// After the TTL expires, counters should reset
	time.Sleep(10 * time.Millisecond)

	code, err := requestOTP(id, 5*time.Minute)
	if err != nil {
		t.Fatalf("expected successful OTP after expiry reset, got %v", err)
	}
	if len(code) != 6 {
		t.Fatalf("expected 6-digit code, got %q", code)
	}
}
