package server

import (
	"testing"
	"time"
)

func TestRequestOTP(t *testing.T) {
	telegramID := int64(1111)

	// Clean up just in case
	otpCache.Delete(telegramID)

	// 1st request - should succeed
	code1, err := requestOTP(telegramID, 5*time.Minute)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if len(code1) != 6 {
		t.Fatalf("expected 6 digit code, got %s", code1)
	}

	// 2nd request (resend) - should succeed
	code2, err := requestOTP(telegramID, 5*time.Minute)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if code1 == code2 {
		t.Fatalf("expected different codes")
	}

	// 3rd request - should fail (max 2 allowed)
	_, err = requestOTP(telegramID, 5*time.Minute)
	if err != ErrMaxRequestsReached {
		t.Fatalf("expected ErrMaxRequestsReached, got %v", err)
	}
}

func TestVerifyOTP_BruteForce(t *testing.T) {
	telegramID := int64(2222)
	otpCache.Delete(telegramID)

	code, _ := requestOTP(telegramID, 5*time.Minute)

	// Attempt 1 - fail
	ok, err := verifyOTP(telegramID, "000000")
	if ok || err != nil {
		t.Fatalf("expected false, nil; got %v, %v", ok, err)
	}

	// Attempt 2 - fail
	ok, err = verifyOTP(telegramID, "000001")
	if ok || err != nil {
		t.Fatalf("expected false, nil; got %v, %v", ok, err)
	}

	// Attempt 3 - fail, should trigger ErrMaxAttemptsReached
	ok, err = verifyOTP(telegramID, "000002")
	if ok || err != ErrMaxAttemptsReached {
		t.Fatalf("expected false, ErrMaxAttemptsReached; got %v, %v", ok, err)
	}

	// Subsequent attempts should return false, nil (because entry was deleted)
	// Wait, is it false, nil? Yes, because val, ok := otpCache.Load(telegramID) will return false
	ok, err = verifyOTP(telegramID, code) // even the correct code won't work anymore
	if ok || err != nil {
		t.Fatalf("expected false, nil after deletion; got %v, %v", ok, err)
	}
}

func TestVerifyOTP_Success(t *testing.T) {
	telegramID := int64(3333)
	otpCache.Delete(telegramID)

	code, _ := requestOTP(telegramID, 5*time.Minute)

	// Correct attempt
	ok, err := verifyOTP(telegramID, code)
	if !ok || err != nil {
		t.Fatalf("expected true, nil; got %v, %v", ok, err)
	}

	// Code is deleted after success
	_, okLoad := otpCache.Load(telegramID)
	if okLoad {
		t.Fatalf("entry should be deleted after successful verify")
	}
}

func TestVerifyOTP_Expired(t *testing.T) {
	telegramID := int64(4444)
	otpCache.Delete(telegramID)

	// Set a very short TTL
	code, _ := requestOTP(telegramID, 1*time.Millisecond)

	time.Sleep(10 * time.Millisecond)

	ok, err := verifyOTP(telegramID, code)
	if ok || err != nil {
		t.Fatalf("expected false, nil for expired code; got %v, %v", ok, err)
	}
}
