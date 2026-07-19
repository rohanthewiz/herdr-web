package main

// outputScanner turns a pane's raw PTY byte stream (as delivered by the β
// pane_output event) into a bounded rolling buffer of plain text for
// pane.wait_for_output matching. It strips VT escape sequences (CSI / OSC / DCS
// and friends) and control bytes so a text pattern matches the visible content
// rather than the raw wire — a colour-wrapped "READY" reads as READY, and the
// per-line context the matcher reports (lineAround) lands on a clean line.
//
// It is deliberately a stripper, not an emulator: it does not honour cursor
// movement, so a bare carriage-return overwrite ("50%\r100%") appends rather than
// replacing. That is fine for substring/regex matching — the newest bytes are
// always present — and far cheaper than running a second emulator gateway-side
// (the daemon already owns the real one). The state machine carries across feed()
// calls so an escape split over two chunks is still consumed whole.
//
// This file is untagged (like page.go) so its logic is testable without the
// libghostty toolchain; the ghostty-tagged orchestrator wires it into the waiter
// path in gateway.go.

// maxScanBytes caps the rolling cleaned-text buffer. Matching runs on every fed
// chunk before the buffer is trimmed to this tail, so any pattern that completes
// within a single chunk (≤ the daemon's 32 KiB read) is always seen intact; the
// cap only discards older content that already failed to match. 64 KiB comfortably
// exceeds any screenful while bounding memory for a long-running wait.
const maxScanBytes = 64 * 1024

// scanState is the escape-stripping state machine's current position.
type scanState uint8

const (
	scGround    scanState = iota // normal text
	scEsc                        // saw ESC (0x1b); next byte selects the sequence kind
	scCSI                        // ESC [ … — control sequence, ends at a final byte 0x40–0x7e
	scOSC                        // ESC ] … — OSC string, ends at BEL or ST (ESC \)
	scOSCEsc                     // ESC seen inside an OSC (maybe the ST terminator)
	scString                     // ESC P/X/^/_ … — DCS/SOS/PM/APC string, ends at ST
	scStringEsc                  // ESC seen inside a DCS/etc string (maybe ST)
	scEscInt                     // ESC + intermediate (e.g. charset ESC ( B): consume to a final byte
)

// outputScanner holds the cross-chunk strip state and the rolling cleaned buffer.
// The zero value is a ready scanner in the ground state.
type outputScanner struct {
	state scanState
	buf   []byte // cleaned text, trimmed to the last maxScanBytes
}

// feed strips escape/control bytes from raw, appends the surviving text to the
// rolling buffer, trims to the byte cap, and returns the buffer as a string for
// the caller to match against. Called once per pane_output chunk.
func (s *outputScanner) feed(raw []byte) string {
	for _, b := range raw {
		s.step(b)
	}
	if len(s.buf) > maxScanBytes {
		// Keep the tail (the newest content); copy down so the backing array can be
		// reclaimed and doesn't grow without bound.
		tail := s.buf[len(s.buf)-maxScanBytes:]
		s.buf = append(s.buf[:0:0], tail...)
	}
	return string(s.buf)
}

// step advances the state machine by one byte, appending to buf only the printable
// text that survives stripping.
func (s *outputScanner) step(b byte) {
	switch s.state {
	case scGround:
		switch {
		case b == 0x1b: // ESC
			s.state = scEsc
		case b == '\n' || b == '\t':
			s.buf = append(s.buf, b) // keep line/column structure for line-context
		case b < 0x20 || b == 0x7f:
			// Drop other C0 controls (incl. bare \r) and DEL — they carry no matchable
			// text. UTF-8 continuation/lead bytes are ≥0x80, so they pass through below.
		default:
			s.buf = append(s.buf, b)
		}
	case scEsc:
		switch {
		case b == '[':
			s.state = scCSI
		case b == ']':
			s.state = scOSC
		case b == 'P' || b == 'X' || b == '^' || b == '_':
			s.state = scString // DCS / SOS / PM / APC — ST-terminated
		case b >= 0x20 && b <= 0x2f:
			s.state = scEscInt // intermediate byte(s) then a final (e.g. charset ESC ( B)
		case b == 0x1b:
			// Another ESC: restart the sequence rather than consuming it as a payload.
		default:
			s.state = scGround // two-byte escape (ESC c, ESC M, ESC 7, …): byte consumed
		}
	case scCSI:
		switch {
		case b == 0x1b:
			s.state = scEsc // malformed; treat as a fresh sequence
		case b >= 0x40 && b <= 0x7e:
			s.state = scGround // final byte ends the CSI
		default:
			// parameter/intermediate byte (0x20–0x3f) or embedded control: stay in CSI
		}
	case scOSC:
		switch b {
		case 0x07: // BEL terminates an OSC
			s.state = scGround
		case 0x1b:
			s.state = scOSCEsc // maybe ST (ESC \)
		default:
			// OSC payload: dropped
		}
	case scOSCEsc:
		if b == '\\' {
			s.state = scGround // ST completes the OSC
		} else {
			s.state = scOSC // not ST; keep consuming the OSC payload
		}
	case scString:
		if b == 0x1b {
			s.state = scStringEsc
		}
		// else: string payload, dropped
	case scStringEsc:
		if b == '\\' {
			s.state = scGround // ST ends the string
		} else {
			s.state = scString
		}
	case scEscInt:
		if b >= 0x30 && b <= 0x7e {
			s.state = scGround // final byte ends the escape
		}
		// else: another intermediate (0x20–0x2f), stay
	}
}
