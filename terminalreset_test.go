package main

import (
	"strings"
	"testing"
	"time"
)

func TestTerminalResetDisablesMouseReporting(t *testing.T) {
	for _, clean := range []bool{true, false} {
		reset := terminalReset(clean)
		for _, mode := range []string{"?1000l", "?1002l", "?1003l", "?1006l", "?2004l", "?25h"} {
			if !strings.Contains(reset, "\x1b["+mode) {
				t.Fatalf("clean=%v: reset lacks %s", clean, mode)
			}
		}
		if strings.Contains(reset, terminalScreenReset) == clean {
			t.Fatalf("clean=%v: unexpected alternate screen handling", clean)
		}
	}
}

func TestStallInterrupt(t *testing.T) {
	aborted := 0
	s := &stallInterrupt{abort: func() { aborted++ }}
	start := time.Unix(1000, 0)

	// A responsive remote prints after every press.
	for i := 0; i < 5; i++ {
		s.input([]byte{3}, start.Add(time.Duration(i)*time.Second))
		s.output()
	}
	if aborted != 0 {
		t.Fatal("aborted a responsive session")
	}

	// Fast repeats do not outrun the answer of a slow but working link.
	s.input([]byte{3, 3, 3, 3}, start)
	if aborted != 0 {
		t.Fatal("aborted before the remote could answer")
	}
	s.output()

	// Ordinary typing and stale presses do not count.
	s.input([]byte("abc"), start)
	s.input([]byte{3}, start)
	s.input([]byte{3}, start.Add(time.Minute))
	s.input([]byte{3}, start.Add(time.Minute+2*time.Second))
	if aborted != 0 {
		t.Fatal("counted a press outside the window")
	}

	s.input([]byte{3}, start.Add(time.Minute+3*time.Second))
	if aborted != 1 {
		t.Fatalf("stalled session aborted %d times", aborted)
	}
}
