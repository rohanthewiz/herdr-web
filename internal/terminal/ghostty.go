//go:build ghostty

// This file implements Emulator on top of go-libghostty. It only builds with
// `-tags ghostty` and requires libghostty-vt on PKG_CONFIG_PATH.
//
// go-libghostty is pinned in go.mod (v0.0.0-20260528200934-790a3ff6e9f6,
// commit 790a3ff6e9f6) and makes no API-stability promise yet, so all of its
// surface is confined to this file behind the Emulator interface.
package terminal

import (
	"fmt"

	libghostty "go.mitchellh.com/libghostty"
)

// Default cell pixel size reported to libghostty on resize. The cell grid we
// read back is independent of these; they only matter for pixel-based reports
// (e.g. Kitty graphics, which Phase B doesn't render yet).
const (
	defaultCellWidthPx  = 8
	defaultCellHeightPx = 16

	// defaultMaxScrollback is the history depth (lines) kept per termhost pane.
	// libghostty defaults to 0 (no scrollback), so the Host must opt in for the
	// orchestrator's scrollback to work.
	defaultMaxScrollback = 10000
)

type ghosttyEmulator struct {
	term *libghostty.Terminal

	// scrollOffset is the viewport's distance (lines) above the live bottom; 0 =
	// pinned to the bottom. libghostty exposes no current-offset query, so we track
	// it ourselves (as herdr's Rust side does) and keep it in sync with Scroll and
	// with new output (which snaps the viewport back to the bottom).
	scrollOffset int

	// Reusable render-state scratch, to avoid per-snapshot allocation.
	rs *libghostty.RenderState
	ri *libghostty.RenderStateRowIterator
	rc *libghostty.RenderStateRowCells
}

// Option configures a new Emulator.
type Option func(*[]libghostty.TerminalOption)

// WithWritePTY registers a callback the terminal invokes when it needs to write
// bytes back to the PTY (e.g. responses to device-attribute / cursor-position
// queries). The Host wires this to the pane's PTY master so query responses are
// handled entirely within Go.
func WithWritePTY(fn func(data []byte)) Option {
	return func(opts *[]libghostty.TerminalOption) {
		*opts = append(*opts, libghostty.WithWritePty(func(_ *libghostty.Terminal, data []byte) {
			// Copy: the slice is only valid for the duration of the callback.
			fn(append([]byte(nil), data...))
		}))
	}
}

// New creates a go-libghostty-backed Emulator of the given cell dimensions.
func New(cols, rows uint16, opts ...Option) (Emulator, error) {
	topts := []libghostty.TerminalOption{
		libghostty.WithSize(cols, rows),
		libghostty.WithMaxScrollback(defaultMaxScrollback),
	}
	for _, o := range opts {
		o(&topts)
	}
	term, err := libghostty.NewTerminal(topts...)
	if err != nil {
		return nil, fmt.Errorf("terminal: new: %w", err)
	}

	rs, err := libghostty.NewRenderState()
	if err != nil {
		term.Close()
		return nil, fmt.Errorf("terminal: render state: %w", err)
	}
	ri, err := libghostty.NewRenderStateRowIterator()
	if err != nil {
		rs.Close()
		term.Close()
		return nil, fmt.Errorf("terminal: row iterator: %w", err)
	}
	rc, err := libghostty.NewRenderStateRowCells()
	if err != nil {
		ri.Close()
		rs.Close()
		term.Close()
		return nil, fmt.Errorf("terminal: row cells: %w", err)
	}

	return &ghosttyEmulator{term: term, rs: rs, ri: ri, rc: rc}, nil
}

// Write feeds raw VT bytes through the parser. It always consumes all bytes.
// New output snaps the viewport back to the live bottom (no scroll-lock yet —
// pinning the view during streaming output is a follow-up).
func (e *ghosttyEmulator) Write(p []byte) (int, error) {
	n, err := e.term.Write(p)
	if n > 0 && e.scrollOffset != 0 {
		e.term.ScrollViewportBottom()
		e.scrollOffset = 0
	}
	return n, err
}

// Scroll moves the viewport by delta lines (negative = up into history), clamped
// to the available scrollback, and tracks the resulting offset-from-bottom.
func (e *ghosttyEmulator) Scroll(delta int) error {
	max, err := e.term.ScrollbackRows()
	if err != nil {
		return fmt.Errorf("terminal: scrollback rows: %w", err)
	}
	target := e.scrollOffset - delta // delta<0 (up) raises the offset
	if target < 0 {
		target = 0
	}
	if target > int(max) {
		target = int(max)
	}
	// ScrollViewportDelta: up is negative. Moving offset cur→target needs a delta
	// of (cur-target): negative when target>cur (scrolling up), positive otherwise.
	if move := e.scrollOffset - target; move != 0 {
		e.term.ScrollViewportDelta(move)
	}
	e.scrollOffset = target
	return nil
}

// ScrollMetrics reports the current scrollback position.
func (e *ghosttyEmulator) ScrollMetrics() (ScrollMetrics, error) {
	max, err := e.term.ScrollbackRows()
	if err != nil {
		return ScrollMetrics{}, fmt.Errorf("terminal: scrollback rows: %w", err)
	}
	rows, err := e.term.Rows()
	if err != nil {
		return ScrollMetrics{}, fmt.Errorf("terminal: rows: %w", err)
	}
	off := e.scrollOffset
	if off > int(max) { // history was pruned below our tracked offset
		off = int(max)
	}
	return ScrollMetrics{
		OffsetFromBottom:    off,
		MaxOffsetFromBottom: int(max),
		ViewportRows:        int(rows),
	}, nil
}

func (e *ghosttyEmulator) Resize(cols, rows uint16) error {
	if err := e.term.Resize(cols, rows, defaultCellWidthPx, defaultCellHeightPx); err != nil {
		return fmt.Errorf("terminal: resize: %w", err)
	}
	return nil
}

func (e *ghosttyEmulator) Title() (string, error) {
	return e.term.Title()
}

func (e *ghosttyEmulator) Snapshot() (*Snapshot, error) {
	if err := e.rs.Update(e.term); err != nil {
		return nil, fmt.Errorf("terminal: render update: %w", err)
	}

	cols, err := e.rs.Cols()
	if err != nil {
		return nil, fmt.Errorf("terminal: cols: %w", err)
	}
	rows, err := e.rs.Rows()
	if err != nil {
		return nil, fmt.Errorf("terminal: rows: %w", err)
	}

	colors, err := e.rs.Colors()
	if err != nil {
		return nil, fmt.Errorf("terminal: colors: %w", err)
	}

	cur, err := e.cursor()
	if err != nil {
		return nil, err
	}

	snap := &Snapshot{
		Cols:      cols,
		Rows:      rows,
		Cursor:    cur,
		DefaultFg: toColor(colors.Foreground),
		DefaultBg: toColor(colors.Background),
		Cells:     make([][]Cell, 0, rows),
	}
	if sm, err := e.ScrollMetrics(); err == nil {
		snap.Scroll = sm
	}

	if err := e.rs.RowIterator(e.ri); err != nil {
		return nil, fmt.Errorf("terminal: row iterator bind: %w", err)
	}

	var style libghostty.RenderCellStyle
	buf := make([]byte, 0, 8)
	var linkRows []uint32 // viewport rows the iterator flags as containing OSC 8 links
	for e.ri.Next() {
		if err := e.ri.Cells(e.rc); err != nil {
			return nil, fmt.Errorf("terminal: cells: %w", err)
		}
		row := make([]Cell, 0, cols)
		for e.rc.Next() {
			g, err := e.rc.AppendGraphemes(buf[:0])
			if err != nil {
				return nil, fmt.Errorf("terminal: graphemes: %w", err)
			}
			if err := e.rc.StyleInto(&style); err != nil {
				return nil, fmt.Errorf("terminal: style: %w", err)
			}
			row = append(row, toCell(string(g), &style))
		}
		// Cheap per-row gate: only rows flagged as having hyperlinks get the
		// (relatively expensive) per-cell URI lookup below. The flag may have
		// false positives, which the per-cell HyperlinkURI ("" = none) absorbs.
		if raw, err := e.ri.Raw(); err == nil {
			if hl, err := raw.Hyperlink(); err == nil && hl {
				linkRows = append(linkRows, uint32(len(snap.Cells)))
			}
		}
		snap.Cells = append(snap.Cells, row)
	}

	// Resolve OSC 8 URIs for flagged rows after the render iteration completes, so
	// GridRef (a borrowed view of terminal internals) never interleaves with the
	// render-state iterators. libghostty does not surface hyperlinks on the
	// render-cell path, only via GridRef.HyperlinkURI.
	for _, y := range linkRows {
		row := snap.Cells[y]
		for x := range row {
			ref, err := e.term.GridRef(libghostty.Point{
				Tag: libghostty.PointTagViewport,
				X:   uint16(x),
				Y:   y,
			})
			if err != nil {
				continue
			}
			uri, err := ref.HyperlinkURI()
			if err != nil || uri == "" {
				continue
			}
			row[x].Link = uri
			snap.HasHyperlinks = true
		}
	}

	return snap, nil
}

func (e *ghosttyEmulator) cursor() (Cursor, error) {
	visible, err := e.rs.CursorVisible()
	if err != nil {
		return Cursor{}, fmt.Errorf("terminal: cursor visible: %w", err)
	}
	// When the viewport is scrolled into history the cursor lies outside it, and
	// libghostty reports its viewport position as invalid; treat that as a hidden
	// cursor rather than failing the whole snapshot.
	x, errX := e.rs.CursorViewportX()
	y, errY := e.rs.CursorViewportY()
	if errX != nil || errY != nil {
		return Cursor{Visible: false}, nil
	}
	vs, err := e.rs.CursorVisualStyle()
	if err != nil {
		return Cursor{}, fmt.Errorf("terminal: cursor style: %w", err)
	}
	return Cursor{X: x, Y: y, Visible: visible, Style: toCursorStyle(vs)}, nil
}

func (e *ghosttyEmulator) Close() error {
	e.rc.Close()
	e.ri.Close()
	e.rs.Close()
	e.term.Close()
	return nil
}

func toColor(c libghostty.ColorRGB) Color {
	return Color{R: c.R, G: c.G, B: c.B}
}

func toCell(rune string, s *libghostty.RenderCellStyle) Cell {
	c := Cell{
		Rune:          rune,
		Bold:          s.Bold,
		Faint:         s.Faint,
		Italic:        s.Italic,
		Underline:     s.Underline,
		Strikethrough: s.Strikethrough,
		Inverse:       s.Inverse,
	}
	if s.HasForeground {
		fg := toColor(s.Foreground)
		c.Fg = &fg
	}
	if s.HasBackground {
		bg := toColor(s.Background)
		c.Bg = &bg
	}
	return c
}

func toCursorStyle(s libghostty.CursorVisualStyle) CursorStyle {
	switch s {
	case libghostty.CursorVisualStyleBar:
		return CursorBar
	case libghostty.CursorVisualStyleUnderline:
		return CursorUnderline
	case libghostty.CursorVisualStyleBlockHollow:
		return CursorBlockHollow
	default:
		return CursorBlock
	}
}
