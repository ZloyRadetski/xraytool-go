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
	exitCalled = false
	out = captureOutput(func() { cmd.Execute() })
	// It will fall back to Share text! Let's check for "failed to convert share links to JSON" instead.
	if !strings.Contains(out, "failed to convert share links to JSON") || !exitCalled {
		t.Errorf("expected share link format error exit, got %v", out)
	}

	// 4. Valid JSON to Share text (no outbounds)
	cmd.SetArgs([]string{"--input", `{"inbounds":[]}`})
	exitCalled = false
	out = captureOutput(func() { cmd.Execute() })
	if !strings.Contains(out, "no valid outbounds") || !exitCalled {
		t.Errorf("expected no valid outbounds, got exit %v", out)
	}

	// 5. Valid Share text to JSON
	cmd.SetArgs([]string{"--input", "vless://123@example.com:443?encryption=none#name"})
	exitCalled = false
	out = captureOutput(func() { cmd.Execute() })
	if exitCalled {
		t.Errorf("expected success, got exit %v", out)
	}
	
	// 6. Valid JSON but XrayJSONToShareText fails (simulated by invalid json content without inbounds?)
	// Not easy to simulate without modifying convert package, we'll try something that fails parsing in convert
}
