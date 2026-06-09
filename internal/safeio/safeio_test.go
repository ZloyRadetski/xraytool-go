package safeio

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteToFile(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "test.txt")

	// 1. Write new file
	err := WriteToFile(filePath, []byte("hello"), 0o644)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	_, err = os.Stat(filePath)
	if err != nil {
		t.Fatalf("expected file to exist")
	}

	// On Windows, permissions are not fully supported, but it should be readable/writable.
	// We'll skip strict permission checks on Windows if it causes issues, but we test the basic flow.

	// 2. Overwrite file
	err = WriteToFile(filePath, []byte("world"), 0o600)
	if err != nil {
		t.Fatalf("expected no error on overwrite, got %v", err)
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("expected to read file")
	}
	if string(data) != "world" {
		t.Fatalf("expected 'world', got '%s'", string(data))
	}
}
