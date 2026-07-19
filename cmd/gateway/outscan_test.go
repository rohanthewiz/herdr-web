package main

import (
	"strings"
	"testing"
)

// The scanner strips CSI colour codes but keeps the wrapped text contiguous, so a
// plain pattern matches and the surrounding line is clean.
func TestOutputScannerStripsCSI(t *testing.T) {
	var s outputScanner
	got := s.feed([]byte("\x1b[32mserver READY\x1b[0m now\r\n"))
	if got != "server READY now\n" {
		t.Fatalf("stripped text = %q, want %q", got, "server READY now\n")
	}
	if !strings.Contains(got, "READY") {
		t.Fatalf("pattern lost after stripping: %q", got)
	}
}

// OSC sequences (both BEL- and ST-terminated) are dropped whole, leaving only the
// surrounding text.
func TestOutputScannerStripsOSC(t *testing.T) {
	var s outputScanner
	// OSC 0 title (BEL-terminated) + OSC 7 cwd (ST-terminated) around real text.
	got := s.feed([]byte("\x1b]0;my title\x07before\x1b]7;file://h/tmp\x1b\\after"))
	if got != "beforeafter" {
		t.Fatalf("stripped text = %q, want %q", got, "beforeafter")
	}
}

// An escape sequence split across two feed() calls is still consumed whole (the
// state machine carries between chunks) — no escape bytes leak into the buffer.
func TestOutputScannerEscapeSplitAcrossChunks(t *testing.T) {
	var s outputScanner
	if got := s.feed([]byte("A\x1b[3")); got != "A" {
		t.Fatalf("after first chunk = %q, want %q", got, "A")
	}
	got := s.feed([]byte("2mB")) // completes the CSI, then a plain 'B'
	if got != "AB" {
		t.Fatalf("after second chunk = %q, want %q", got, "AB")
	}
}

// Bare carriage returns are dropped (overwrite semantics aren't emulated) while
// newlines are kept, so a substring still matches across a progress-style redraw.
func TestOutputScannerDropsCarriageReturn(t *testing.T) {
	var s outputScanner
	got := s.feed([]byte("Progress: 50%\rProgress: 100%\n"))
	if strings.Contains(got, "\r") {
		t.Fatalf("carriage return should be stripped: %q", got)
	}
	if !strings.Contains(got, "100%") {
		t.Fatalf("newest content lost: %q", got)
	}
}

// Multi-byte UTF-8 passes through untouched (only ESC and C0 bytes are examined).
func TestOutputScannerPassesUTF8(t *testing.T) {
	var s outputScanner
	got := s.feed([]byte("café ✓ 日本語"))
	if got != "café ✓ 日本語" {
		t.Fatalf("utf-8 mangled: %q", got)
	}
}

// The rolling buffer is bounded to the tail, but a pattern that arrives after the
// cap is still present (matching runs against the returned tail).
func TestOutputScannerBoundsBufferKeepingTail(t *testing.T) {
	var s outputScanner
	s.feed([]byte(strings.Repeat("x", maxScanBytes*2))) // overflow with filler
	got := s.feed([]byte("TAILMARKER"))
	if len(got) > maxScanBytes {
		t.Fatalf("buffer not bounded: len=%d, cap=%d", len(got), maxScanBytes)
	}
	if !strings.HasSuffix(got, "TAILMARKER") {
		t.Fatalf("newest content trimmed away; tail=%q", got[max(0, len(got)-20):])
	}
}
