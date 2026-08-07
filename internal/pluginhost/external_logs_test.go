package pluginhost

import "testing"

func TestLogTailKeepsCompleteRecentLines(t *testing.T) {
	tail := newLogTail(10)
	if _, err := tail.Write([]byte("old\nnewest\n")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	lines := tail.lines(0)
	if len(lines) != 1 || lines[0] != "newest" {
		t.Fatalf("lines() = %#v, want newest tail", lines)
	}
}

func TestLogTailLimitsReturnedLines(t *testing.T) {
	tail := newLogTail(64)
	_, _ = tail.Write([]byte("one\ntwo\nthree\n"))
	lines := tail.lines(2)
	if len(lines) != 2 || lines[0] != "two" || lines[1] != "three" {
		t.Fatalf("lines(2) = %#v", lines)
	}
}
