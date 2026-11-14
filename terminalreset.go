package main

import (
	"io"
	"sync"
	"time"
)

// Claude enables mouse reporting, focus events, bracketed paste and extended
// keyboard modes in the local terminal. A dropped connection leaves nobody to
// turn them off, and the shell then receives mouse reports as typed text.
const terminalModeReset = "\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1005l\x1b[?1006l\x1b[?1015l" +
	"\x1b[?1004l\x1b[?2004l\x1b[<u\x1b[>4;0m\x1b[0m\x1b[?25h"

// Leaving the alternate screen moves the cursor, so it is sent only when the
// remote side did not finish cleanly and could not restore the screen itself.
const terminalScreenReset = "\x1b[?1049l"

func terminalReset(clean bool) string {
	if clean {
		return terminalModeReset
	}
	return terminalScreenReset + terminalModeReset
}

const (
	stallInterruptPresses = 3
	stallInterruptMinimum = time.Second
	stallInterruptWindow  = 5 * time.Second
)

// stallInterrupt ends a session whose connection stopped answering. In raw
// mode Ctrl+C is only a byte for the remote side, so a stalled link would
// otherwise hold the terminal until the keepalive gives up. A working remote
// answers the first press with output, which clears the count.
type stallInterrupt struct {
	mu    sync.Mutex
	first time.Time
	count int
	abort func()
}

func (s *stallInterrupt) output() {
	s.mu.Lock()
	s.count = 0
	s.mu.Unlock()
}

func (s *stallInterrupt) input(b []byte, now time.Time) {
	s.mu.Lock()
	fire := false
	for _, c := range b {
		if c != 3 {
			continue
		}
		if s.count == 0 || now.Sub(s.first) > stallInterruptWindow {
			s.first, s.count = now, 0
		}
		s.count++
		if s.count >= stallInterruptPresses && now.Sub(s.first) >= stallInterruptMinimum {
			s.count, fire = 0, true
		}
	}
	s.mu.Unlock()
	if fire {
		s.abort()
	}
}

type stallInput struct {
	r io.Reader
	s *stallInterrupt
}

func (i stallInput) Read(b []byte) (int, error) {
	n, e := i.r.Read(b)
	i.s.input(b[:n], time.Now())
	return n, e
}

type stallOutput struct {
	w io.Writer
	s *stallInterrupt
}

func (o stallOutput) Write(b []byte) (int, error) {
	o.s.output()
	return o.w.Write(b)
}
