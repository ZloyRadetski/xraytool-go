package cmd

import (
	"strings"
	"testing"
)

func TestHelpers_Printer(t *testing.T) {
	setupTest(t)
	defer teardownTest()

	// Batch Mode
	pb := newPrinter(true)
	out := captureOutput(func() { pb.Success("A", "B") })
	if !strings.Contains(out, "SUCCESS|A|B") {
		t.Errorf("expected SUCCESS|A|B, got %v", out)
	}

	out = captureOutput(func() { pb.Error("some error") })
	if !strings.Contains(out, "ERROR|some error") || !exitCalled {
		t.Errorf("expected error exit, got %v", out)
	}
	exitCalled = false

	// Interactive Mode
	pi := newPrinter(false)
	out = captureOutput(func() { pi.Info("hello") })
	if !strings.Contains(out, "[INFO] hello") {
		t.Errorf("expected INFO, got %v", out)
	}

	out = captureOutput(func() { pi.OK("done") })
	if !strings.Contains(out, "[OK] done") {
		t.Errorf("expected OK, got %v", out)
	}

	out = captureOutput(func() { pi.Warn("warning") })
	if !strings.Contains(out, "[WARN] warning") {
		t.Errorf("expected WARN, got %v", out)
	}

	out = captureOutput(func() { pi.Println("plain") })
	if !strings.Contains(out, "plain") {
		t.Errorf("expected plain, got %v", out)
	}

	out = captureOutput(func() { pi.Errorf("bad %s", "thing") })
	if !strings.Contains(out, "[ERROR] bad thing") || !exitCalled {
		t.Errorf("expected ERROR exit, got %v", out)
	}
}

func TestHelpers_ValidEmail(t *testing.T) {
	if !validEmail("test@example.com") {
		t.Error("expected valid")
	}
	if validEmail("test spaces") {
		t.Error("expected invalid")
	}
}

func TestHelpers_DefaultExpireDate(t *testing.T) {
	d := defaultExpireDate()
	if len(d) != 10 {
		t.Error("expected date string")
	}
}

func TestHelpers_SubfileID(t *testing.T) {
	if subfileID("test.txt") != "test" {
		t.Error("expected test")
	}
	if subfileID("test") != "test" {
		t.Error("expected test")
	}
}

func TestHelpers_LimitPtrFromStr(t *testing.T) {
	p, err := limitPtrFromStr("")
	if err != nil || p != nil {
		t.Error("expected nil, nil")
	}
	p, err = limitPtrFromStr("5")
	if err != nil || *p != 5 {
		t.Error("expected 5")
	}
	_, err = limitPtrFromStr("invalid")
	if err == nil {
		t.Error("expected error")
	}
	_, err = limitPtrFromStr("-1")
	if err == nil {
		t.Error("expected error")
	}
}

func TestRoot_RequireRoot(t *testing.T) {
	setupTest(t)
	defer teardownTest()

	oldGOOS := currentGOOS
	oldGeteuid := geteuid
	defer func() {
		currentGOOS = oldGOOS
		geteuid = oldGeteuid
	}()

	// Windows (default)
	currentGOOS = "windows"
	captureOutput(func() { requireRoot() })
	if exitCalled {
		t.Error("did not expect exit on windows")
	}

	// Non-windows, root
	currentGOOS = "linux"
	geteuid = func() int { return 0 }
	captureOutput(func() { requireRoot() })
	if exitCalled {
		t.Error("did not expect exit for root")
	}

	// Non-windows, non-root
	geteuid = func() int { return 1000 }
	out := captureOutput(func() { requireRoot() })
	if !exitCalled {
		t.Error("expected exit for non-root")
	}
	if !strings.Contains(out, "Script must be run as root") {
		t.Errorf("expected error message, got %s", out)
	}
}
