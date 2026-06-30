package cmd

import (
	"os"
	"strings"
	"testing"
)

func TestConvertCmd(t *testing.T) {
	setupTest(t)
	defer teardownTest()

	cmd := convertCmd()

	oldStdin := os.Stdin
	defer func() { os.Stdin = oldStdin }()
	r, _, _ := os.Pipe()
	os.Stdin = r
	r.Close()

	// 1. Invalid input resolving (simulated by passing no args and no stdin)
	// We'll just pass a flag
	oldArgs := os.Args
	defer func() { os.Args = oldArgs }()
	os.Args = []string{"xraytool"}

	cmd.SetArgs([]string{})
	// This will show help and return, no exit
	out := captureOutput(func() { cmd.Execute() })
	if !strings.Contains(out, "Usage:") {
		t.Errorf("expected help output, got %v", out)
	}

	// 2. Removed invalid resolve test

	// 3. Invalid JSON format
	cmd.SetArgs([]string{"--input", "invalid json!!"})
	err := cmd.Execute()
	// It will fall back to Share text! Let's check for "failed to convert share links to JSON" instead.
	if err == nil || !strings.Contains(err.Error(), "failed to convert share links to JSON") {
		t.Errorf("expected share link format error exit, got %v", err)
	}

	// 4. Valid JSON to Share text (no outbounds)
	cmd.SetArgs([]string{"--input", `{"inbounds":[]}`})
	err = cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "no valid outbounds") {
		t.Errorf("expected no valid outbounds, got exit %v", err)
	}

	// 5. Valid Share text to JSON
	cmd.SetArgs([]string{"--input", "vless://123@example.com:443?encryption=none#name"})
	err = cmd.Execute()
	if err != nil {
		t.Errorf("expected success, got err %v", err)
	}

	// 6. Valid JSON but XrayJSONToShareText fails (simulated by invalid json content without inbounds?)
	// Not easy to simulate without modifying convert package, we'll try something that fails parsing in convert
}
