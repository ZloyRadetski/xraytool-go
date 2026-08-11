package server

import (
	"context"
	"errors"
	"time"

	"xraytool/internal/pluginapi"
)

func (r *Router) issueOTP(ctx context.Context, subject, payload string, ttl time.Duration) (string, error) {
	if r.identityProvider == nil {
		return requestOTPWithPayload(subject, payload, ttl)
	}
	code, err := r.identityProvider.IssueOTP(ctx, subject, payload, ttl)
	if errors.Is(err, pluginapi.ErrIdentityRateLimited) {
		return "", ErrMaxRequestsReached
	}
	return code, err
}

func (r *Router) verifyIdentityOTP(ctx context.Context, subject, code string) (bool, string, error) {
	if r.identityProvider == nil {
		return verifyOTP(subject, code)
	}
	valid, payload, err := r.identityProvider.VerifyOTP(ctx, subject, code)
	if errors.Is(err, pluginapi.ErrIdentityAttemptsExceeded) {
		return false, "", ErrMaxAttemptsReached
	}
	return valid, payload, err
}
