package generate

import (
	"strings"
	"testing"
)

func TestSecret(t *testing.T) {
	lengths := []int{1, 10, 16, 32, 64}
	for _, l := range lengths {
		s := Secret(l)
		if len(s) != l {
			t.Errorf("Secret(%d) returned string of length %d", l, len(s))
		}
		for _, c := range s {
			if !strings.ContainsRune(alphanumChars, c) {
				t.Errorf("Secret(%d) returned string with invalid char '%c'", l, c)
			}
		}
	}
}

func TestSubfile(t *testing.T) {
	s := Subfile()
	if !strings.HasSuffix(s, ".txt") {
		t.Errorf("Subfile() does not end with .txt: %s", s)
	}
	if len(s) != 16+4 {
		t.Errorf("Subfile() unexpected length: expected 20, got %d", len(s))
	}
}

func TestUUID(t *testing.T) {
	u, err := UUID()
	if err != nil {
		t.Fatalf("UUID() returned error: %v", err)
	}
	if !isValidUUID(u) {
		t.Errorf("UUID() returned invalid UUID: %s", u)
	}
}

func TestIsValidUUID(t *testing.T) {
	valid := []string{
		"12345678-1234-1234-1234-123456789012",
		"00000000-0000-0000-0000-000000000000",
		"ffffffff-ffff-ffff-ffff-ffffffffffff",
	}
	for _, v := range valid {
		if !isValidUUID(v) {
			t.Errorf("isValidUUID(%q) returned false, expected true", v)
		}
	}

	invalid := []string{
		"",
		"12345678-1234-1234-1234-12345678901",
		"12345678-1234-1234-1234-1234567890123",
		"12345678_1234_1234_1234_123456789012",
		"12345678-123451234-1234-123456789012",
	}
	for _, iv := range invalid {
		if isValidUUID(iv) {
			t.Errorf("isValidUUID(%q) returned true, expected false", iv)
		}
	}
}
