package subscription

import (
	"testing"
	"github.com/stretchr/testify/assert"
)

func TestResolveClientID(t *testing.T) {
	req := &Request{
		Query: map[string]string{
			"id": "Test-ID_123",
		},
		Headers: make(map[string]string),
	}
	id, file := ResolveClientID(req)
	assert.Equal(t, "Test-ID_123", id)
	assert.Equal(t, "Test-ID_123.txt", file)

	req2 := &Request{
		Query: map[string]string{
			"file": "subFILE-1",
		},
		Headers: make(map[string]string),
	}
	id2, file2 := ResolveClientID(req2)
	assert.Equal(t, "subFILE-1", id2)
	assert.Equal(t, "subFILE-1.txt", file2)

	req3 := &Request{
		Query: make(map[string]string),
		Headers: map[string]string{
			"x-request-path": "/my-uuid-456",
		},
	}
	id3, file3 := ResolveClientID(req3)
	assert.Equal(t, "my-uuid-456", id3)
	assert.Equal(t, "my-uuid-456.txt", file3)
}

func TestGenerateDummyVless(t *testing.T) {
	lines := []string{
		"YOU_ARE_BANNED",
		"YOU_ARE_BANNED_2",
	}
	res := generateDummyVless(lines)
	assert.Contains(t, res, "YOU_ARE_BANNED")
	assert.Contains(t, res, "YOU_ARE_BANNED_2")
	assert.Contains(t, res, "vless://00000000-0000-0000-0000-000000000000") // checks dummy UUID substitution
	assert.Contains(t, res, "127.0.0.1:443")
	assert.Contains(t, res, "127.0.0.1:444") // incremented port
}
