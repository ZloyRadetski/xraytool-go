package safeio

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteToFile_RenameFallback(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "bind_mount.json")

	// 1. Write initial file so it exists
	err := WriteToFile(filePath, []byte("initial"), 0o644)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	// 2. Mock osRename to fail with "device or resource busy"
	originalRename := osRename
	defer func() { osRename = originalRename }()
	
	osRename = func(oldpath, newpath string) error {
		return errors.New("device or resource busy")
	}

	// 3. Write again. Rename should fail, but it should fallback to WriteFile!
	err = WriteToFile(filePath, []byte("fallback_success"), 0o644)
	if err != nil {
		t.Fatalf("expected fallback to succeed, got error: %v", err)
	}

	// 4. Verify content
	data, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("failed to read file: %v", err)
	}
	if string(data) != "fallback_success" {
		t.Fatalf("expected 'fallback_success', got '%s'", string(data))
	}
}

