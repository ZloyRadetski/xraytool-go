package cmd

import (
	"os"
	"strings"
	"testing"
)

func TestPermissions(t *testing.T) {
	setupTest(t)
	defer teardownTest()

	// We'll check output. With our new logic, if the files are missing
	// but the parent directory (current dir) is writable, it should not report warnings for them.
	out := captureOutput(func() { checkAndReportPermissions() })
	if strings.Contains(out, "Missing") && !strings.Contains(out, "OK") {
		t.Errorf("expected 'OK' or nothing for missing files in writable dir, got %v", out)
	}

	// Create paths so they exist
	os.WriteFile("test_xray_config.json", []byte{}, 0644)
	os.WriteFile("test_limited.db", []byte{}, 0644)
	os.MkdirAll("test_templates", 0755)

	// Test checkPath
	// checkPath is private, but checkAndReportPermissions calls it.
	out = captureOutput(func() { checkAndReportPermissions() })
	// Should be fewer warnings
}
