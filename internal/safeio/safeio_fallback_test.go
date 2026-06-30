package safeio

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// TestWriteToFile_RenameError verifies that a rename failure propagates as an error
// instead of silently falling back to a non-atomic write that could corrupt the file.
func TestWriteToFile_RenameError(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "bind_mount.json")

	// Write initial file so it exists.
	if err := WriteToFile(filePath, []byte("initial"), 0o644); err != nil {
		t.Fatalf("initial write: %v", err)
	}

	// Mock osRename to simulate a failure (e.g. EBUSY, though this should not
	// happen in practice because the temp file is always in the same directory).
	original := osRename
	defer func() { osRename = original }()
	osRename = func(_, _ string) error {
		return errors.New("simulated rename failure")
	}

	// WriteToFile must return an error — not silently corrupt the file with a
	// non-atomic O_TRUNC write.
	err := WriteToFile(filePath, []byte("new_content"), 0o644)
	if err == nil {
		t.Fatal("expected error on rename failure, got nil")
	}
	if !strings.Contains(err.Error(), "atomic rename") {
		t.Errorf("unexpected error message: %v", err)
	}
}
