package identity_memory

import (
	"context"
	"errors"
	"testing"
	"time"

	"xraytool/internal/pluginapi"
)

func TestPlugin_IssueAndVerifyOTP(t *testing.T) {
	p := New()
	code, err := p.IssueOTP(context.Background(), "user@example.com", "linked-account", time.Minute)
	if err != nil {
		t.Fatalf("IssueOTP: %v", err)
	}
	if len(code) != 6 {
		t.Fatalf("code length = %d, want 6", len(code))
	}

	valid, payload, err := p.VerifyOTP(context.Background(), "user@example.com", code)
	if err != nil || !valid || payload != "linked-account" {
		t.Fatalf("VerifyOTP = (%t, %q, %v), want (true, linked-account, nil)", valid, payload, err)
	}
	valid, _, err = p.VerifyOTP(context.Background(), "user@example.com", code)
	if err != nil || valid {
		t.Fatalf("OTP must be single use, got (%t, %v)", valid, err)
	}
}

func TestPlugin_RateAndAttemptLimits(t *testing.T) {
	p := New()
	for range 2 {
		if _, err := p.IssueOTP(context.Background(), "123", "", time.Minute); err != nil {
			t.Fatalf("IssueOTP: %v", err)
		}
	}
	if _, err := p.IssueOTP(context.Background(), "123", "", time.Minute); !errors.Is(err, pluginapi.ErrIdentityRateLimited) {
		t.Fatalf("third request error = %v, want rate limit", err)
	}

	p = New()
	if _, err := p.IssueOTP(context.Background(), "123", "", time.Minute); err != nil {
		t.Fatalf("IssueOTP: %v", err)
	}
	for range 2 {
		valid, _, err := p.VerifyOTP(context.Background(), "123", "000000")
		if err != nil || valid {
			t.Fatalf("wrong code = (%t, %v), want (false, nil)", valid, err)
		}
	}
	valid, _, err := p.VerifyOTP(context.Background(), "123", "000000")
	if valid || !errors.Is(err, pluginapi.ErrIdentityAttemptsExceeded) {
		t.Fatalf("third wrong code = (%t, %v), want attempts exceeded", valid, err)
	}
}
