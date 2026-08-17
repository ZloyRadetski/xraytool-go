package safeio

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteToFile_Errors(t *testing.T) {
	// Bad path to cause MkdirAll to fail (e.g., trying to use a file as a directory)
	tmpFile := filepath.Join(t.TempDir(), "notadir")
	os.WriteFile(tmpFile, []byte(""), 0o644) //nolint:errcheck
	err := WriteToFile(filepath.Join(tmpFile, "file.txt"), []byte("data"), 0o644)
	if err == nil {
		t.Errorf("expected error when MkdirAll fails")
	}

	// Read-only directory to cause CreateTemp to fail
	readOnlyDir := filepath.Join(t.TempDir(), "readonly")
	if err := os.MkdirAll(readOnlyDir, 0o555); err == nil {
		err = WriteToFile(filepath.Join(readOnlyDir, "file.txt"), []byte("data"), 0o644)
		if err == nil {
			t.Errorf("expected error for read-only directory")
		}
	}
}
