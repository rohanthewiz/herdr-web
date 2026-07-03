package orchestration

// xtmodkeysScanner tracks xterm's XTMODKEYS modifyOtherKeys state (CSI > 4 ; N m
// written by the child into its output stream), tolerating sequences split
// across reads. libghostty-vt applies the mode to its key encoder but does not
// surface the parsed state to Go, so the Host scans the raw PTY bytes itself —
// the same pattern as the OSC passthrough scanners. Not safe for concurrent
// use: a pane drives one scanner from its readPump goroutine.
//
// Recognized forms: CSI >4;0m / >4;1m / >4;2m (set level) and CSI >4m (reset).
// Any other CSI > ... m (e.g. >0m, >4;2;...m junk) leaves the state unchanged
// unless it addresses resource 4.
type xtmodkeysScanner struct {
	state   xtmodState
	params  []byte
	enabled bool
}

type xtmodState uint8

const (
	xtmodNormal  xtmodState = iota // scanning for ESC
	xtmodSawEsc                    // last byte was ESC (0x1b)
	xtmodSawCSI                    // saw ESC [
	xtmodCollect                   // saw ESC [ > — collecting params until final byte
)

// xtmodMaxParams caps the buffered parameter bytes so malformed input cannot
// grow the scanner without bound.
const xtmodMaxParams = 32

// scan consumes a chunk of terminal output. It returns the current
// modifyOtherKeys enablement and whether this chunk changed it.
func (s *xtmodkeysScanner) scan(b []byte) (enabled, changed bool) {
	for _, c := range b {
		switch s.state {
		case xtmodNormal:
			if c == 0x1b {
				s.state = xtmodSawEsc
			}
		case xtmodSawEsc:
			switch c {
			case '[':
				s.state = xtmodSawCSI
			case 0x1b: // ESC ESC — still a potential introducer
			default:
				s.state = xtmodNormal
			}
		case xtmodSawCSI:
			switch c {
			case '>':
				s.state = xtmodCollect
				s.params = s.params[:0]
			case 0x1b:
				s.state = xtmodSawEsc
			default:
				// Some other CSI; skip until its final byte without buffering.
				if c >= 0x40 && c <= 0x7e {
					s.state = xtmodNormal
				}
			}
		case xtmodCollect:
			switch {
			case (c >= '0' && c <= '9') || c == ';':
				if len(s.params) < xtmodMaxParams {
					s.params = append(s.params, c)
				}
			case c == 'm':
				if v, ok := parseXtmodkeys(s.params); ok && v != s.enabled {
					s.enabled = v
					changed = true
				}
				s.state = xtmodNormal
			case c == 0x1b:
				s.state = xtmodSawEsc
			default:
				// Any other final/intermediate byte: not XTMODKEYS.
				if c >= 0x40 && c <= 0x7e {
					s.state = xtmodNormal
				}
			}
		}
	}
	return s.enabled, changed
}

// parseXtmodkeys interprets the parameter bytes of a CSI > ... m sequence.
// Returns (enabled, ok) where ok reports whether the sequence addressed the
// modifyOtherKeys resource (4).
func parseXtmodkeys(params []byte) (bool, bool) {
	fields := splitParams(params)
	if len(fields) == 0 || fields[0] != 4 {
		return false, false
	}
	if len(fields) == 1 {
		return false, true // CSI >4m resets the resource
	}
	return fields[1] > 0, true
}

func splitParams(params []byte) []int {
	var out []int
	cur, has := 0, false
	for _, c := range params {
		switch {
		case c >= '0' && c <= '9':
			cur = cur*10 + int(c-'0')
			has = true
		case c == ';':
			out = append(out, cur)
			cur, has = 0, true // empty params count as 0
		}
	}
	if has {
		out = append(out, cur)
	}
	return out
}
