package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDownloadGeoAtomicallyInvalidURLKeepsExistingFile(t *testing.T) {
	dir := t.TempDir()
	destination := filepath.Join(dir, "geoip.dat")
	const original = "known-good-geo-data"
	if err := os.WriteFile(destination, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := downloadGeoAtomically(t.Context(), "http://127.0.0.1:1/unavailable", destination); err == nil {
		t.Fatal("expected download failure")
	}
	data, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != original {
		t.Fatalf("destination changed after failed download: got %q want %q", data, original)
	}
}
