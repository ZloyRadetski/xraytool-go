// Package generate provides cryptographically secure generators for UUIDs,
// subscription filenames, and random secrets.
package generate

import (
	"crypto/rand"
	"fmt"
	"math/big"
)

const alphanumChars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// Secret generates a cryptographically secure random alphanumeric string
// of the given length.
func Secret(length int) string {
	b := make([]byte, length)
	charsetLen := big.NewInt(int64(len(alphanumChars)))
	for i := range b {
		n, err := rand.Int(rand.Reader, charsetLen)
		if err != nil {
			panic(fmt.Sprintf("crypto/rand failed: %v", err))
		}
		b[i] = alphanumChars[n.Int64()]
	}
	return string(b)
}

// Subfile generates a random subscription filename (16 random chars + ".txt").
func Subfile() string {
	return Secret(16) + ".txt"
}

// UUID generates a UUID v4. It tries the xray binary first (for maximum
// compatibility with the xray UUID format), then falls back to a pure-Go
// implementation using crypto/rand.
func UUID() (string, error) {
	// Pure-Go implementation: RFC 4122 version 4.
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating UUID: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant bits (RFC 4122)
	return fmt.Sprintf(
		"%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16],
	), nil
}

func isValidUUID(s string) bool {
	// Quick sanity check: 36 chars, dashes at positions 8, 13, 18, 23.
	if len(s) != 36 {
		return false
	}
	for _, i := range []int{8, 13, 18, 23} {
		if s[i] != '-' {
			return false
		}
	}
	return true
}
