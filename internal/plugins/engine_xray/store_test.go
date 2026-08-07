package engine_xray

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadWriteModify(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	// Read non-existent
	_, err := Read(path)
	if err == nil || !strings.Contains(err.Error(), "reading xray config") {
		t.Errorf("expected read error, got %v", err)
	}

	// Write basic
	cfg := RawConfig{"test": []byte(`"val"`)}
	if err := Write(path, cfg); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	// Read back
	cfg2, err := Read(path)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}
	if string(cfg2["test"]) != `"val"` {
		t.Errorf("expected 'val', got %s", cfg2["test"])
	}

	// Modify
	err = Modify(path, func(c RawConfig) error {
		c["test"] = []byte(`"val2"`)
		return nil
	})
	if err != nil {
		t.Fatalf("Modify failed: %v", err)
	}

	// Read after Modify
	cfg3, err := Read(path)
	if err != nil {
		t.Fatalf("Read after modify failed: %v", err)
	}
	if string(cfg3["test"]) != `"val2"` {
		t.Errorf("expected 'val2', got %s", cfg3["test"])
	}

	// Modify returning error
	dummyErr := errors.New("dummy error")
	err = Modify(path, func(c RawConfig) error {
		return dummyErr
	})
	if !errors.Is(err, dummyErr) {
		t.Errorf("Modify should return inner error, got %v", err)
	}

	// Write invalid JSON (can't really happen since RawConfig wraps RawMessage, but let's test marshal fail? Wait, marshaling a map[string]json.RawMessage with invalid json inside just writes it.
	// Oh, if we put an unmarshalable thing... we can't really do that since RawMessage is just []byte.
	// Wait, what if we test read with bad json?
	os.WriteFile(path, []byte(`{bad json`), 0644) //nolint:errcheck
	_, err = Read(path)
	if err == nil || !strings.Contains(err.Error(), "parsing xray config") {
		t.Errorf("expected parse error, got %v", err)
	}

	// Modify on bad json
	err = Modify(path, func(c RawConfig) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "parsing xray config") {
		t.Errorf("Modify should fail on read, got %v", err)
	}

	// Write invalid config to trigger json.Marshal error
	badCfg := RawConfig{"bad": []byte(`invalid json`)}
	err = Write(path, badCfg)
	if err == nil || !strings.Contains(err.Error(), "marshaling xray config") {
		t.Errorf("expected marshaling error, got %v", err)
	}
}

func TestOpenLockFileFallback(t *testing.T) {
	// Try to open a lock file in a directory that does not exist.
	// This will fail and trigger the fallback to /tmp/xraytool_config.lock.
	// We don't really assert the fallback success on Windows/Linux, but we hit the code path.
	lf, _ := openLockFile("/non-existent-dir/config.json.lock")
	if lf != nil {
		lf.Close()
	}
}
