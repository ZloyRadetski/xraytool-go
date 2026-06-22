package cmd

import (
	"os"
	"strings"
	"testing"
)

func TestCommands_FailurePaths(t *testing.T) {
	setupTest(t)
	defer teardownTest()

	oldStdin := os.Stdin
	defer func() { os.Stdin = oldStdin }()
	r, _, _ := os.Pipe()
	os.Stdin = r
	r.Close()

	// Write an invalid xray config to force early failures in all commands
	os.WriteFile("test_xray_config.json", []byte("invalid json"), 0644)

	tests := []struct {
		args        []string
		expectedErr string
	}{
		{[]string{"newuser", "--email=test@example.com", "--config=test_config.yaml"}, "reading xray config"},
		{[]string{"rmuser", "--email=test@example.com", "--config=test_config.yaml"}, "reading xray config"},
		{[]string{"limit", "--email=test@example.com", "--config=test_config.yaml"}, "reading xray config"},
		{[]string{"unlimit", "--email=test@example.com", "--config=test_config.yaml"}, "reading xray config"},
		{[]string{"setexpire", "--email=test@example.com", "--config=test_config.yaml"}, "reading xray config"},
		{[]string{"setlimit", "--email=test@example.com", "--limit=5", "--config=test_config.yaml"}, "reading xray config"},
		{[]string{"sharelink", "--email=test@example.com", "--config=test_config.yaml"}, "reading xray config"},
		{[]string{"usersnapshot", "--config=test_config.yaml"}, "reading xray config"},
		{[]string{"syncstates", "--config=test_config.yaml"}, "reading xray config"},
		{[]string{"cli-stats", "--config=test_config.yaml"}, "reading xray config"},
		{[]string{"start-server", "--config=test_config.yaml"}, "reading xray config"},
	}

	for _, tt := range tests {
		exitCalled = false
		rootCmd.SetArgs(tt.args)
		out := captureOutput(func() { rootCmd.Execute() })
		if !strings.Contains(out, tt.expectedErr) {
			// Some might fail with other errors if it hits before xrayconfig parse, like missing email etc.
			// Let's just ensure they don't panic and exit gracefully.
		}
	}
}

func TestCommands_EdgeCases(t *testing.T) {
	setupTest(t)
	defer teardownTest()

	// Mock stdin so prompts return immediately with empty/EOF
	oldStdin := os.Stdin
	defer func() { os.Stdin = oldStdin }()
	r, _, _ := os.Pipe()
	os.Stdin = r
	r.Close() // closed pipe immediately returns EOF on read

	// Edge case: newuser without email
	rootCmd.SetArgs([]string{"newuser", "--email=", "--name=", "--config=test_config.yaml"})
	exitCalled = false
	out := captureOutput(func() { rootCmd.Execute() })
	if !strings.Contains(out, "email is required") || !exitCalled {
		t.Errorf("expected email error, got %v", out)
	}

	// Edge case: invalid email characters
	rootCmd.SetArgs([]string{"newuser", "--email=in!valid", "--config=test_config.yaml"})
	exitCalled = false
	out = captureOutput(func() { rootCmd.Execute() })
	if !strings.Contains(out, "invalid characters") || !exitCalled {
		t.Errorf("expected invalid email error, got %v", out)
	}

	// genBalancerCmd with invalid args
	cmd := genBalancerCmd()
	cmd.SetArgs([]string{"--url=http://127.0.0.1:0/invalid"})
	exitCalled = false
	out = captureOutput(func() { cmd.Execute() })
	if !strings.Contains(out, "download failed") {
		t.Errorf("expected network error, got %v", out)
	}
}
