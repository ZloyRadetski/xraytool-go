package cmd

import (
	"os"
	"strings"
	"testing"
)

func TestPermissions(t *testing.T) {
	setupTest(t)
	defer teardownTest()

	// It checks paths from config. We will let it check the test paths.
	// Because test paths do not exist, it should report warnings.
	out := captureOutput(func() { checkAndReportPermissions() })
	if !strings.Contains(out, "WARNING:") {
		t.Errorf("expected warnings, got %v", out)
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
