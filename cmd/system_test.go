package cmd

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
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

func TestDownloadGeoAtomicallySetsCorrectPermissions(t *testing.T) {
	dir := t.TempDir()
	geoDir := filepath.Join(dir, "xray-geo-share")
	destination := filepath.Join(geoDir, "geoip.dat")
	const content = "new-geo-data-payload"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(content))
	}))
	defer server.Close()

	if err := downloadGeoAtomically(t.Context(), server.URL, destination); err != nil {
		t.Fatalf("download failed: %v", err)
	}

	data, err := os.ReadFile(destination)
	if err != nil {
		t.Fatalf("read downloaded file: %v", err)
	}
	if string(data) != content {
		t.Fatalf("downloaded content mismatch: got %q want %q", string(data), content)
	}

	if runtime.GOOS != "windows" {
		fi, err := os.Stat(destination)
		if err != nil {
			t.Fatalf("stat downloaded file: %v", err)
		}
		if perm := fi.Mode().Perm(); perm != 0o644 {
			t.Errorf("expected file permissions 0644, got %04o", perm)
		}

		dirInfo, err := os.Stat(geoDir)
		if err != nil {
			t.Fatalf("stat target dir: %v", err)
		}
		if perm := dirInfo.Mode().Perm(); perm != 0o755 {
			t.Errorf("expected directory permissions 0755, got %04o", perm)
		}
	}
}
