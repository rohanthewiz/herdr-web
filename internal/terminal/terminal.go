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

	// Close releases the underlying terminal resources.
	Close() error
}
