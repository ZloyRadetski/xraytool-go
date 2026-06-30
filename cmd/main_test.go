package cmd

import (
	"bytes"
	"io"
	"os"
	"testing"
	"xraytool/internal/database"
)

func setupTest(t *testing.T) {
	// Ensure global DB state is reset before each test
	database.Close()
	os.WriteFile("test_config.yaml", []byte("database:\n  driver: sqlite\n  sqlite_path: test.db\npaths:\n  xray_config: test_xray_config.json\n"), 0644)
}

func teardownTest() {
	os.Remove("test_config.yaml")
	os.Remove("test.db")
	os.Remove("test_xray_config.json")
}

func captureOutput(f func()) string {
	oldStdout := os.Stdout
	oldStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stdout = w
	os.Stderr = w

	func() {
		defer func() { recover() }()
		f()
	}()

	w.Close()
	os.Stdout = oldStdout
	os.Stderr = oldStderr

	var buf bytes.Buffer
	io.Copy(&buf, r)
	return buf.String()
}
