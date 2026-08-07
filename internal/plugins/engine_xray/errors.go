package engine_xray

import "errors"

var (
	// ErrEngineUnavailable is returned when the VPN engine cannot be reached or fails to respond.
	ErrEngineUnavailable = errors.New("engine unavailable")

	// ErrUserNotFound is returned when attempting to remove or update a user that doesn't exist.
	ErrUserNotFound = errors.New("user not found")

	// ErrNotSupported is returned when an engine adapter does not support a requested feature.
	ErrNotSupported = errors.New("operation not supported by this engine")
)
