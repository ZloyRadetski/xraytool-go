package safeio

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteToFile_Errors(t *testing.T) {
	// Bad path to cause MkdirAll to fail (e.g., trying to use a file as a directory)
	tmpFile := filepath.Join(t.TempDir(), "notadir")
	os.WriteFile(tmpFile, []byte(""), 0o644)
	err := WriteToFile(filepath.Join(tmpFile, "file.txt"), []byte("data"), 0o644)
	if err == nil {
		t.Errorf("expected error when MkdirAll fails")
	}

	// Bad dir path to cause CreateTemp to fail (e.g. read-only dir, or invalid path)
	// On Windows, a volume that doesn't exist usually fails.
	err = WriteToFile(`Z:\invalid\path\file.txt`, []byte("data"), 0o644)
	if err == nil {
		t.Errorf("expected error for invalid path")
	}
}
