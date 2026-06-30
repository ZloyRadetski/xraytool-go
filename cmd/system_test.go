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
	err := rootCmd.Execute()
	if err == nil || (!strings.Contains(err.Error(), "Update failed") && !strings.Contains(err.Error(), "Failed to download script")) {
		t.Errorf("expected update to fail on windows, got %v", err)
	}

	rootCmd.SetArgs([]string{"update-geo", "--config=test_config.yaml"})
	err = rootCmd.Execute()
	if err != nil {
		t.Errorf("expected geo update success, got error %v", err)
	}

	// Test migrate failure (this covers the migration start and error handling)

	// Test migrate failure
	os.WriteFile("test_xray_config.json", []byte("invalid json"), 0644)
	rootCmd.SetArgs([]string{"migrate", "--config=test_config.yaml"})
	err = rootCmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "ERROR|migrate:") {
		t.Errorf("expected migrate to fail, got %v", err)
	}
}
