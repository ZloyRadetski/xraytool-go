package cmd

import (
	"os"
	"strings"
	"testing"
)

func TestSystemCmds(t *testing.T) {
	setupTest(t)
	defer teardownTest()

	// Mock exec.Command? we can't easily mock exec.Command without modifying system.go.
	// But updateXrayCmd runs curl. If it fails, it prints error. We can let it run and fail or succeed.
	// We are on Windows so bash doesn't exist, it will fail.
	// Update Xray (fails on windows without bash)
	rootCmd.SetArgs([]string{"update-xray", "--config=test_config.yaml"})
	out := captureOutput(func() { rootCmd.Execute() })
	if !strings.Contains(out, "Update failed") || !exitCalled {
		t.Errorf("expected update to fail on windows, got %v", out)
	}

	exitCalled = false
	rootCmd.SetArgs([]string{"update-geo", "--config=test_config.yaml"})
	out = captureOutput(func() { rootCmd.Execute() })
	if !strings.Contains(out, "Geo databases updated") {
		t.Errorf("expected geo update success, got %v", out)
	}

	exitCalled = false
	// Test migrate failure (this covers the migration start and error handling)

	// Test migrate failure
	os.WriteFile("test_xray_config.json", []byte("invalid json"), 0644)
	exitCalled = false
	rootCmd.SetArgs([]string{"migrate", "--config=test_config.yaml"})
	out = captureOutput(func() { rootCmd.Execute() })
	if !strings.Contains(out, "ERROR|migrate:") || !exitCalled {
		t.Errorf("expected migrate to fail, got %v", out)
	}
}
