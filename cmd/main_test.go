package cmd

import (
	"bytes"
	"io"
	"os"
	"testing"
)

var exitCalled bool

func setupTest(t *testing.T) {
	exitCalled = false
	osExit = func(code int) {
		exitCalled = true
		panic("exit")
	}
}

func teardownTest() {
	osExit = os.Exit
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
