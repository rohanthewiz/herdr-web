// Package terminal is the Phase B Go-owned terminal runtime: a VT emulator that
// Go drives end to end (PTY output in, cell-grid Snapshot out), replacing the
// Rust server's src/pty + src/ghostty + src/terminal for a pane.
//
// The Emulator interface and the Snapshot/Cell value types in this file are
// pure Go and always compile, so the rest of herdr-web (the Phase A gateway)
// builds without the CGO terminal backend. The concrete implementation lives in
// ghostty.go behind the `ghostty` build tag and is selected with:
//
//	go build -tags ghostty   (with libghostty-vt on PKG_CONFIG_PATH)
//
// See scripts/build-libghostty-vt.sh.
package terminal

import "io"

// Color is an 8-bit-per-channel RGB color.
type Color struct {
	R, G, B uint8
}

// CursorStyle is the visual style of the cursor (DECSCUSR).
type CursorStyle uint8

const (
	CursorBlock CursorStyle = iota
	CursorBar
	CursorUnderline
	CursorBlockHollow
)

// Cursor describes the viewport cursor at snapshot time.
type Cursor struct {
	X, Y    uint16 // viewport column/row, 0-based
	Visible bool
	Style   CursorStyle
}

// Cell is one terminal cell: its grapheme cluster plus resolved styling.
// Fg/Bg are nil when the cell uses the terminal's default fore/background,
// letting the renderer apply Snapshot.DefaultFg/DefaultBg.
type Cell struct {
	Rune string // grapheme cluster; "" means blank
	Fg   *Color
	Bg   *Color

	Bold          bool
	Faint         bool
	Italic        bool
	Underline     bool
	Strikethrough bool
	Inverse       bool

	// Link is the OSC 8 hyperlink URI for this cell, or "" if none.
	Link string
}

// Snapshot is an immutable, copied view of the terminal grid. Every field is a
// Go value (no references into the C terminal), so it is safe to hand to the
// renderer and to retain across further writes.
type Snapshot struct {
	Cols, Rows uint16
	Cells      [][]Cell // [row][col], len == Rows, each row len == Cols
	Cursor     Cursor
	DefaultFg  Color
	DefaultBg  Color

	// HasHyperlinks is true when at least one cell carries an OSC 8 Link, letting
	// the frame builder skip the per-cell link scan when there are none.
	HasHyperlinks bool

	// Scroll is the scrollback position at snapshot time.
	Scroll ScrollMetrics
}

// ScrollMetrics describes a pane's scrollback position, mirroring herdr's
// ScrollMetrics so the orchestrator can drive its scrollbar/indicator.
type ScrollMetrics struct {
	// OffsetFromBottom is how many lines the viewport is scrolled up from the
	// live bottom (0 = pinned to the bottom / active area).
	OffsetFromBottom int
	// MaxOffsetFromBottom is the number of scrollback (history) lines available.
	MaxOffsetFromBottom int
	// ViewportRows is the visible grid height in cells.
	ViewportRows int
}

// SelectionEndpoint is one end of a text selection in screen-buffer (absolute)
// coordinates: Row counts from the top of the scrollback buffer (stable while the
// pane scrolls), Col is the 0-based column.
type SelectionEndpoint struct {
	Row uint32
	Col uint16
}

// TextScope selects which part of the buffer ExtractText reads.
type TextScope uint8

const (
	// TextVisible is the current viewport (what is on screen now).
	TextVisible TextScope = iota
	// TextRecent is the last N lines of the screen buffer (scrollback + active),
	// where N is the request's Lines (0 = the whole buffer).
	TextRecent
)

// MouseMode is the active mouse-tracking mode (DEC private modes 9/1000/1002/1003).
type MouseMode uint8

const (
	MouseNone         MouseMode = iota // tracking off
	MouseX10                           // press only (mode 9)
	MousePressRelease                  // press + release (mode 1000)
	MouseButtonMotion                  // + motion while a button is held (mode 1002)
	MouseAnyMotion                     // + all motion (mode 1003)
)

// MouseEncoding is the wire encoding for mouse reports (SGR / UTF-8 / legacy).
type MouseEncoding uint8

const (
	MouseEncodingDefault MouseEncoding = iota
	MouseEncodingUTF8                  // mode 1005
	MouseEncodingSGR                   // mode 1006
)

// InputModes is the terminal's current input-affecting DEC mode state. The Go
// daemon owns the emulator, so these reflect what the running program actually
// requested; the orchestrator needs them to encode keys/mouse and to decide
// whether a mouse/paste/focus event is for the program or for its own UI.
type InputModes struct {
	AlternateScreen      bool
	ApplicationCursor    bool // DECCKM
	BracketedPaste       bool
	FocusReporting       bool
	MouseMode            MouseMode
	MouseEncoding        MouseEncoding
	MouseAlternateScroll bool
	SynchronizedOutput   bool
	KittyKeyboardFlags   uint16 // 0 = legacy keyboard
	// ModifyOtherKeys is xterm's XTMODKEYS state (CSI >4;Nm). libghostty-vt
	// does not surface it, so the emulator leaves it false; the orchestration
	// host injects the value from its raw-stream scanner before reporting.
	ModifyOtherKeys bool
}

// At returns the cell at (col,row), or the zero Cell if out of range.
func (s *Snapshot) At(col, row uint16) Cell {
	if int(row) >= len(s.Cells) || int(col) >= len(s.Cells[row]) {
		return Cell{}
	}
	return s.Cells[row][col]
}

// Emulator is a VT terminal owned entirely by Go. Phase B feeds PTY/agent
// output in via Write and renders the resulting Snapshot in the browser.
//
// Implementations are not safe for concurrent use; serialize calls (a pane owns
// one Emulator on one goroutine).
type Emulator interface {
	io.Writer // feed raw VT-encoded bytes (PTY output)

	// Resize changes the grid dimensions in cells.
	Resize(cols, rows uint16) error

	// Snapshot returns a copied view of the current grid + cursor.
	Snapshot() (*Snapshot, error)

	// Title returns the window/icon title set via OSC 0/2, or "" if none.
	Title() (string, error)

	// Scroll moves the viewport by delta lines: negative scrolls up into history,
	// positive scrolls back down toward the live bottom. The viewport is clamped
	// to the available scrollback.
	Scroll(delta int) error

	// ScrollMetrics reports the current scrollback position.
	ScrollMetrics() (ScrollMetrics, error)

	// FormatSelection returns the text of the selection bounded by the anchor and
	// cursor endpoints (screen-buffer coordinates, in any order — the emulator
	// orders them top-left → bottom-right). The result is plain text with
	// soft-wrapped lines unwrapped and trailing whitespace trimmed. When rectangle
	// is true the range is a block region rather than a linear reading-order range.
	// Returns "" when the range has no selectable content.
	FormatSelection(anchor, cursor SelectionEndpoint, rectangle bool) (string, error)

	// InputModes reports the terminal's current input-affecting DEC mode state.
	InputModes() (InputModes, error)

	// ExtractText returns buffer text for the given scope. ansi selects VT (styled
	// escape sequences) vs plain text; unwrap rejoins soft-wrapped lines (only
	// meaningful for TextRecent). Trailing whitespace is trimmed. lines bounds
	// TextRecent (0 = the whole buffer) and is ignored for TextVisible.
	ExtractText(scope TextScope, lines int, ansi, unwrap bool) (string, error)

	// Close releases the underlying terminal resources.
	Close() error
}
