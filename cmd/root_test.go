package cmd

import (
	"os"
	"strings"
	"testing"
)

func TestRoot_Execute(t *testing.T) {
	setupTest(t)
	defer teardownTest()

	oldArgs := os.Args
	defer func() { os.Args = oldArgs }()
	os.Args = []string{"xraytool", "--help"}

	out := captureOutput(func() { Execute() })
	if !strings.Contains(out, "Available Commands") {
		t.Errorf("expected help output, got %s", out)
	}

	// Test Execute with error
	os.Args = []string{"xraytool", "--non-existent-flag"}
	out = captureOutput(func() { Execute() })
	if !strings.Contains(out, "unknown flag") || !exitCalled {
		t.Errorf("expected error exit, got %v", out)
	}
}

func TestRoot_LoadConfig(t *testing.T) {
	setupTest(t)
	defer teardownTest()

	oldArgs := os.Args
	defer func() { os.Args = oldArgs }()
	
	// Test skip
	os.Args = []string{"xraytool"}
	loadConfig()

	os.Args = []string{"xraytool", "--help"}
	loadConfig()

	// Test actual config load (fails because path is a directory)
	os.Args = []string{"xraytool", "newuser"}
	cfgFile = "."
	out := captureOutput(func() { loadConfig() })
	if !strings.Contains(out, "failed to load config") || !exitCalled {
		t.Errorf("expected error exit, got %v", out)
	}
	exitCalled = false

	// Test logger.Init failure
	os.Args = []string{"xraytool", "newuser"}
	cfgFile = "test_config_logger_fail.yaml"
	os.WriteFile(cfgFile, []byte("logging:\n  file_path: \"Z:\\\\nonexistent_drive\\\\invalid\\\\file.log\"\n"), 0644)
	out = captureOutput(func() { loadConfig() })
	if !strings.Contains(out, "failed to initialize logger") {
		t.Errorf("expected logger failure, got %v", out)
	}
	os.Remove(cfgFile)
}
