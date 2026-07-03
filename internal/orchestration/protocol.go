// Package orchestration defines the Phase B Go↔Rust seam: Rust orchestrates
// (workspace/pane tree, layout, detection, session, compositing) and Go is the
// terminal backend (PTY + VT emulation per pane). Rust sends commands
// (create/input/resize/close); Go sends events (pane frames, exit).
//
// This file is the wire contract and is pure Go (no CGO), so it compiles and is
// testable without the libghostty toolchain. The Host that actually runs panes
// lives in host.go behind the `ghostty` build tag.
//
// See ai_docs/phase-b-orchestration-seam.md for the full design.
package orchestration

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"

	"github.com/rohanthewiz/herdr-web/internal/terminal"
)

// ProtocolVersion is bumped on any breaking change to the message shapes.
const ProtocolVersion = 1

// MaxFrameSize caps a single length-prefixed frame. A host frame is one pane
// (smaller than a full composited UI); 8 MiB leaves headroom for large grids.
const MaxFrameSize = 8 * 1024 * 1024

// MessageType is the JSON "type" discriminator.
type MessageType string

const (
	// Rust → Go (commands).
	MsgHello            MessageType = "hello"
	MsgCreatePane       MessageType = "create_pane"
	MsgInput            MessageType = "input"
	MsgResize           MessageType = "resize"
	MsgClosePane        MessageType = "close_pane"
	MsgScrollViewport   MessageType = "scroll_viewport"
	MsgRequestSelection MessageType = "request_selection"
	MsgRequestText      MessageType = "request_text"
	MsgRequestResync    MessageType = "request_resync"
	MsgShutdown         MessageType = "shutdown"

	// Go → Rust (events).
	MsgWelcome       MessageType = "welcome"
	MsgPaneFrame     MessageType = "pane_frame"
	MsgPaneCwd       MessageType = "pane_cwd"
	MsgPaneAgent     MessageType = "pane_agent"
	MsgPaneClipboard MessageType = "pane_clipboard"
	MsgPaneTitle     MessageType = "pane_title"
	MsgPaneSelection MessageType = "pane_selection"
	MsgPaneText      MessageType = "pane_text"
	MsgPaneModes     MessageType = "pane_modes"
	MsgPaneExited    MessageType = "pane_exited"
	MsgError         MessageType = "error"
)

// --- Commands (Rust → Go) ---------------------------------------------------

type Hello struct {
	Type            MessageType `json:"type"`
	ProtocolVersion int         `json:"protocol_version"`
}

func NewHello() Hello { return Hello{Type: MsgHello, ProtocolVersion: ProtocolVersion} }

type CreatePane struct {
	Type         MessageType       `json:"type"`
	PaneID       uint32            `json:"pane_id"`
	Cols         uint16            `json:"cols"`
	Rows         uint16            `json:"rows"`
	CellWidthPx  uint32            `json:"cell_width_px"`
	CellHeightPx uint32            `json:"cell_height_px"`
	Cwd          string            `json:"cwd,omitempty"`
	Command      string            `json:"command,omitempty"` // empty ⇒ default shell
	Args         []string          `json:"args,omitempty"`
	Env          map[string]string `json:"env,omitempty"`
	// InitialHistory is VT-encoded scrollback to seed the emulator with before the
	// child's first output, so a restored session shows its prior history above the
	// freshly spawned shell (the analogue of herdr's seed_history_ansi).
	InitialHistory string `json:"initial_history,omitempty"`
}

func NewCreatePane(id uint32, cols, rows uint16) CreatePane {
	return CreatePane{Type: MsgCreatePane, PaneID: id, Cols: cols, Rows: rows}
}

// Input carries raw bytes to write to a pane's PTY. Data marshals as base64.
type Input struct {
	Type   MessageType `json:"type"`
	PaneID uint32      `json:"pane_id"`
	Data   []byte      `json:"data"`
}

func NewInput(id uint32, data []byte) Input {
	return Input{Type: MsgInput, PaneID: id, Data: data}
}

type Resize struct {
	Type         MessageType `json:"type"`
	PaneID       uint32      `json:"pane_id"`
	Cols         uint16      `json:"cols"`
	Rows         uint16      `json:"rows"`
	CellWidthPx  uint32      `json:"cell_width_px"`
	CellHeightPx uint32      `json:"cell_height_px"`
}

func NewResize(id uint32, cols, rows uint16) Resize {
	return Resize{Type: MsgResize, PaneID: id, Cols: cols, Rows: rows}
}

type ClosePane struct {
	Type   MessageType `json:"type"`
	PaneID uint32      `json:"pane_id"`
}

func NewClosePane(id uint32) ClosePane { return ClosePane{Type: MsgClosePane, PaneID: id} }

// ScrollViewport scrolls a pane's viewport by Delta lines: negative scrolls up
// into scrollback history, positive scrolls back down toward the live bottom. The
// Go side clamps to the available history, so a large positive Delta is a reliable
// "scroll to bottom". The resulting position is reported back via Frame.Scroll.
type ScrollViewport struct {
	Type   MessageType `json:"type"`
	PaneID uint32      `json:"pane_id"`
	Delta  int32       `json:"delta"`
}

func NewScrollViewport(id uint32, delta int32) ScrollViewport {
	return ScrollViewport{Type: MsgScrollViewport, PaneID: id, Delta: delta}
}

// SelectionPoint is one endpoint of a selection in screen-buffer (absolute)
// coordinates: Row counts from the top of the scrollback buffer (so it is stable
// while the pane scrolls), Col is the 0-based column. This mirrors herdr's
// Selection endpoints (row, col), which it tracks in screen-buffer space.
type SelectionPoint struct {
	Row uint32 `json:"row"`
	Col uint16 `json:"col"`
}

// RequestSelection asks the Go side to extract the text of the selection bounded
// by Anchor and Cursor (in screen-buffer coordinates). The orchestrator holds
// selection state and key/mouse handling; the Go daemon owns the emulator that
// can resolve those coordinates to text, so this is a request/response: the Host
// replies with a pane_selection event carrying the formatted text. The two
// endpoints may be in any order (the Host orders them top-left → bottom-right);
// Rectangle selects a block region rather than a linear (reading-order) range.
type RequestSelection struct {
	Type      MessageType    `json:"type"`
	PaneID    uint32         `json:"pane_id"`
	Anchor    SelectionPoint `json:"anchor"`
	Cursor    SelectionPoint `json:"cursor"`
	Rectangle bool           `json:"rectangle,omitempty"`
}

func NewRequestSelection(id uint32, anchor, cursor SelectionPoint, rectangle bool) RequestSelection {
	return RequestSelection{Type: MsgRequestSelection, PaneID: id, Anchor: anchor, Cursor: cursor, Rectangle: rectangle}
}

// RequestText asks the Go side to extract buffer text from a pane (the orchestrator
// holds an unfed local emulator for termhost panes, so it can't read text itself).
// The Host replies with a pane_text event. Scope is terminal.TextScope (0 visible,
// 1 recent); Lines bounds the recent scope (0 = whole buffer); Ansi selects VT vs
// plain; Unwrap rejoins soft-wrapped lines.
type RequestText struct {
	Type   MessageType `json:"type"`
	PaneID uint32      `json:"pane_id"`
	Scope  uint8       `json:"scope"`
	Lines  uint32      `json:"lines,omitempty"`
	Ansi   bool        `json:"ansi,omitempty"`
	Unwrap bool        `json:"unwrap,omitempty"`
}

func NewRequestText(id uint32, scope uint8, lines uint32, ansi, unwrap bool) RequestText {
	return RequestText{Type: MsgRequestText, PaneID: id, Scope: scope, Lines: lines, Ansi: ansi, Unwrap: unwrap}
}

// RequestResync asks the daemon to replay a single pane's current state (full
// frame + modes + cwd + title + agent). A reconnecting client sends this after
// adopting a surviving pane reported in welcome.panes, so the pane repaints
// deterministically regardless of when the client registered it (it doesn't have
// to race the daemon's post-hello replay). Unknown pane IDs are ignored.
type RequestResync struct {
	Type   MessageType `json:"type"`
	PaneID uint32      `json:"pane_id"`
}

func NewRequestResync(id uint32) RequestResync {
	return RequestResync{Type: MsgRequestResync, PaneID: id}
}

// Shutdown asks a persistent daemon to exit and tear down all panes. The
// orchestrator sends this on a *clean* quit so the daemon doesn't linger; a
// crash or binary handoff instead just drops the connection (the daemon keeps
// its panes alive for the next herdr to reconnect and resync).
type Shutdown struct {
	Type MessageType `json:"type"`
}

func NewShutdown() Shutdown { return Shutdown{Type: MsgShutdown} }

// --- Events (Go → Rust) -----------------------------------------------------

type Welcome struct {
	Type            MessageType `json:"type"`
	ProtocolVersion int         `json:"protocol_version"`
	Error           string      `json:"error,omitempty"`
	// Panes lists the pane IDs the daemon already has live when a client connects.
	// Empty on a fresh daemon; populated when a restarted/handed-off herdr reconnects
	// to a persistent daemon, so it can reconcile its restored session against the
	// surviving panes (adopt the matches, expect a resync for each) instead of
	// re-creating them. The daemon replays each pane's current state (full frame +
	// modes + cwd + title + agent) right after this welcome.
	Panes []uint32 `json:"panes,omitempty"`
}

func NewWelcome(errMsg string, panes []uint32) Welcome {
	return Welcome{Type: MsgWelcome, ProtocolVersion: ProtocolVersion, Error: errMsg, Panes: panes}
}

type PaneFrame struct {
	Type   MessageType `json:"type"`
	PaneID uint32      `json:"pane_id"`
	Frame  *Frame      `json:"frame"`
}

func NewPaneFrame(id uint32, f *Frame) PaneFrame {
	return PaneFrame{Type: MsgPaneFrame, PaneID: id, Frame: f}
}

// PaneCwd reports a pane's working directory (OSC 7) when it changes, so the
// orchestrator can track per-pane cwd (new-pane inheritance, worktree).
type PaneCwd struct {
	Type   MessageType `json:"type"`
	PaneID uint32      `json:"pane_id"`
	Cwd    string      `json:"cwd"`
}

func NewPaneCwd(id uint32, cwd string) PaneCwd {
	return PaneCwd{Type: MsgPaneCwd, PaneID: id, Cwd: cwd}
}

// PaneAgent reports the detected agent identity and state for a pane. The Go
// daemon owns the PTY child, so it runs detection and reports results; the
// orchestrator maps this onto its screen-detection path. Agent is "" for a plain
// shell; State is one of idle|working|blocked|unknown.
type PaneAgent struct {
	Type           MessageType `json:"type"`
	PaneID         uint32      `json:"pane_id"`
	Agent          string      `json:"agent"`
	State          string      `json:"state"`
	VisibleBlocker bool        `json:"visible_blocker"`
	VisibleWorking bool        `json:"visible_working"`
}

func NewPaneAgent(id uint32, agent, state string, visibleBlocker, visibleWorking bool) PaneAgent {
	return PaneAgent{
		Type:           MsgPaneAgent,
		PaneID:         id,
		Agent:          agent,
		State:          state,
		VisibleBlocker: visibleBlocker,
		VisibleWorking: visibleWorking,
	}
}

// PaneClipboard forwards an OSC 52 clipboard-write the pane's child emitted.
// libghostty-vt drops OSC 52, so the Host reconstructs it from the raw PTY byte
// stream (as it does OSC 7 cwd) and the orchestrator re-emits it through its own
// clipboard writer. Data is the decoded clipboard bytes (base64 on the wire); an
// empty Data is a clipboard-clear. Only the "c"/default selection is forwarded;
// queries have no reply path and are dropped.
type PaneClipboard struct {
	Type   MessageType `json:"type"`
	PaneID uint32      `json:"pane_id"`
	Data   []byte      `json:"data"`
}

func NewPaneClipboard(id uint32, data []byte) PaneClipboard {
	return PaneClipboard{Type: MsgPaneClipboard, PaneID: id, Data: data}
}

// PaneTitle reports a pane's window title (OSC 0/2) when it changes. libghostty
// surfaces the title to the emulator, but the seam otherwise carries none, so the
// orchestrator can show the running program's title on a termhost pane's border the
// way it does for in-process panes. An empty Title is a title-clear.
type PaneTitle struct {
	Type   MessageType `json:"type"`
	PaneID uint32      `json:"pane_id"`
	Title  string      `json:"title"`
}

func NewPaneTitle(id uint32, title string) PaneTitle {
	return PaneTitle{Type: MsgPaneTitle, PaneID: id, Title: title}
}

// PaneSelection is the reply to a RequestSelection: the plain text of the
// requested range, with soft-wrapped lines unwrapped and trailing whitespace
// trimmed (matching herdr's own selection extraction). Text is "" when the range
// has no selectable content. The orchestrator hands this to its clipboard writer
// (AppEvent::ClipboardWrite). One pane_selection is emitted per request.
type PaneSelection struct {
	Type   MessageType `json:"type"`
	PaneID uint32      `json:"pane_id"`
	Text   string      `json:"text"`
}

func NewPaneSelection(id uint32, text string) PaneSelection {
	return PaneSelection{Type: MsgPaneSelection, PaneID: id, Text: text}
}

// PaneText is the reply to a RequestText: the extracted buffer text (empty when the
// range has no content). One pane_text is emitted per request.
type PaneText struct {
	Type   MessageType `json:"type"`
	PaneID uint32      `json:"pane_id"`
	Text   string      `json:"text"`
}

func NewPaneText(id uint32, text string) PaneText {
	return PaneText{Type: MsgPaneText, PaneID: id, Text: text}
}

// PaneModes reports a pane's input-affecting DEC mode state (mouse tracking,
// bracketed paste, focus reporting, application cursor, alt-scroll, sync output,
// kitty keyboard) when it changes. The Go emulator owns these; the orchestrator
// mirrors them so its key/mouse encoders and its "is this event for the program or
// for my UI" decisions match what the running program actually requested. Without
// this the orchestrator would read its own unfed emulator and mis-encode input.
type PaneModes struct {
	Type                 MessageType `json:"type"`
	PaneID               uint32      `json:"pane_id"`
	AlternateScreen      bool        `json:"alternate_screen"`
	ApplicationCursor    bool        `json:"application_cursor"`
	BracketedPaste       bool        `json:"bracketed_paste"`
	FocusReporting       bool        `json:"focus_reporting"`
	MouseMode            uint8       `json:"mouse_mode"`     // terminal.MouseMode
	MouseEncoding        uint8       `json:"mouse_encoding"` // terminal.MouseEncoding
	MouseAlternateScroll bool        `json:"mouse_alternate_scroll"`
	SynchronizedOutput   bool        `json:"synchronized_output"`
	KittyKeyboardFlags   uint16      `json:"kitty_keyboard_flags"`
	ModifyOtherKeys      bool        `json:"modify_other_keys,omitempty"`
}

func NewPaneModes(id uint32, m terminal.InputModes) PaneModes {
	return PaneModes{
		Type:                 MsgPaneModes,
		PaneID:               id,
		AlternateScreen:      m.AlternateScreen,
		ApplicationCursor:    m.ApplicationCursor,
		BracketedPaste:       m.BracketedPaste,
		FocusReporting:       m.FocusReporting,
		MouseMode:            uint8(m.MouseMode),
		MouseEncoding:        uint8(m.MouseEncoding),
		MouseAlternateScroll: m.MouseAlternateScroll,
		SynchronizedOutput:   m.SynchronizedOutput,
		KittyKeyboardFlags:   m.KittyKeyboardFlags,
		ModifyOtherKeys:      m.ModifyOtherKeys,
	}
}

type PaneExited struct {
	Type     MessageType `json:"type"`
	PaneID   uint32      `json:"pane_id"`
	ExitCode int         `json:"exit_code"`
}

func NewPaneExited(id uint32, code int) PaneExited {
	return PaneExited{Type: MsgPaneExited, PaneID: id, ExitCode: code}
}

type Error struct {
	Type    MessageType `json:"type"`
	PaneID  uint32      `json:"pane_id,omitempty"`
	Message string      `json:"message"`
}

func NewError(paneID uint32, msg string) Error {
	return Error{Type: MsgError, PaneID: paneID, Message: msg}
}

// --- Frame / cell wire types ------------------------------------------------
//
// Shaped to drop straight into Rust wire::FrameData / CellData compositing.

// Cell mirrors Rust wire::CellData.
type Cell struct {
	Symbol    string  `json:"symbol"`
	Fg        uint32  `json:"fg"`        // packed: 0x02_RR_GG_BB
	Bg        uint32  `json:"bg"`        // packed: 0x02_RR_GG_BB
	Modifier  uint16  `json:"modifier"`  // ratatui Modifier bitmask
	Skip      bool    `json:"skip"`      // true ⇒ unchanged since last frame (diff)
	Hyperlink *uint32 `json:"hyperlink"` // OSC 8 index (reserved; not yet populated)
}

// Cursor mirrors Rust wire::CursorState.
type Cursor struct {
	X       uint16 `json:"x"`
	Y       uint16 `json:"y"`
	Visible bool   `json:"visible"`
	Shape   uint8  `json:"shape"` // DECSCUSR param
}

// Frame is one pane's grid, full or diffed.
type Frame struct {
	Cols   uint16  `json:"cols"`
	Rows   uint16  `json:"rows"`
	Full   bool    `json:"full"`
	Cursor *Cursor `json:"cursor"`
	Cells  []Cell  `json:"cells"` // row-major, len == cols*rows
	// Hyperlinks is the frame's OSC 8 URI table; a cell's Hyperlink indexes into it.
	// Only populated on frames that carry links (which are always sent full).
	Hyperlinks []string `json:"hyperlinks,omitempty"`
	// Scroll is the pane's scrollback position, present only when the pane has
	// scrollback history (so non-scrollback panes' frames are unchanged).
	Scroll *ScrollInfo `json:"scroll,omitempty"`
}

// ScrollInfo mirrors terminal.ScrollMetrics on the wire (and herdr's ScrollMetrics).
type ScrollInfo struct {
	OffsetFromBottom    int `json:"offset_from_bottom"`
	MaxOffsetFromBottom int `json:"max_offset_from_bottom"`
	ViewportRows        int `json:"viewport_rows"`
}

// ratatui Modifier bits (subset we map).
const (
	modBold       uint16 = 0b0000_0000_0001
	modDim        uint16 = 0b0000_0000_0010
	modItalic     uint16 = 0b0000_0000_0100
	modUnderlined uint16 = 0b0000_0000_1000
	modReversed   uint16 = 0b0000_0100_0000
	modCrossedOut uint16 = 0b0001_0000_0000
)

// packRGB encodes an RGB color like Rust wire::color_to_u32 (RGB variant).
func packRGB(c terminal.Color) uint32 {
	return 0x02000000 | uint32(c.R)<<16 | uint32(c.G)<<8 | uint32(c.B)
}

func modifierBits(c terminal.Cell) uint16 {
	var m uint16
	if c.Bold {
		m |= modBold
	}
	if c.Faint {
		m |= modDim
	}
	if c.Italic {
		m |= modItalic
	}
	if c.Underline {
		m |= modUnderlined
	}
	if c.Inverse {
		m |= modReversed
	}
	if c.Strikethrough {
		m |= modCrossedOut
	}
	return m
}

func cursorShape(s terminal.CursorStyle) uint8 {
	switch s {
	case terminal.CursorBar:
		return 6
	case terminal.CursorUnderline:
		return 4
	default: // block / block-hollow
		return 2
	}
}

// resolveCell turns a Snapshot cell into a wire Cell (without the skip flag),
// resolving nil fg/bg to the snapshot defaults so Rust receives concrete colors.
func resolveCell(snap *terminal.Snapshot, c terminal.Cell) Cell {
	fg := snap.DefaultFg
	if c.Fg != nil {
		fg = *c.Fg
	}
	bg := snap.DefaultBg
	if c.Bg != nil {
		bg = *c.Bg
	}
	sym := c.Rune
	if sym == "" {
		sym = " "
	}
	return Cell{
		Symbol:   sym,
		Fg:       packRGB(fg),
		Bg:       packRGB(bg),
		Modifier: modifierBits(c),
	}
}

// FrameFromSnapshot builds a Frame for cur. If prev is nil or its dimensions
// differ, the frame is full (all cells sent, skip=false). Otherwise it is a
// diff: cells unchanged from prev are marked skip=true.
func FrameFromSnapshot(cur, prev *terminal.Snapshot) *Frame {
	// A frame carrying OSC 8 links is always sent full: the per-cell hyperlink
	// index points into this frame's Hyperlinks table, and a skipped (diff) cell
	// would keep a stale index from the prior frame's table. Links are uncommon
	// and transient, so the lost diff savings while a link is on screen is fine.
	full := prev == nil || prev.Cols != cur.Cols || prev.Rows != cur.Rows || cur.HasHyperlinks

	f := &Frame{
		Cols:  cur.Cols,
		Rows:  cur.Rows,
		Full:  full,
		Cells: make([]Cell, 0, int(cur.Cols)*int(cur.Rows)),
		Cursor: &Cursor{
			X:       cur.Cursor.X,
			Y:       cur.Cursor.Y,
			Visible: cur.Cursor.Visible,
			Shape:   cursorShape(cur.Cursor.Style),
		},
	}

	var hlIndex map[string]uint32 // URI → table index, built only when links present
	if cur.HasHyperlinks {
		hlIndex = make(map[string]uint32)
	}

	for y := uint16(0); y < cur.Rows; y++ {
		for x := uint16(0); x < cur.Cols; x++ {
			src := cur.At(x, y)
			cell := resolveCell(cur, src)
			if !full {
				if prevCell := resolveCell(prev, prev.At(x, y)); prevCell == cell {
					cell.Skip = true
				}
			}
			if hlIndex != nil && src.Link != "" {
				idx, ok := hlIndex[src.Link]
				if !ok {
					idx = uint32(len(f.Hyperlinks))
					hlIndex[src.Link] = idx
					f.Hyperlinks = append(f.Hyperlinks, src.Link)
				}
				i := idx // stable address; &idx would alias the loop's reused var
				cell.Hyperlink = &i
			}
			f.Cells = append(f.Cells, cell)
		}
	}
	// Carry scrollback position only when the pane has history (or is scrolled),
	// leaving non-scrollback panes' frames byte-for-byte as before.
	if cur.Scroll.MaxOffsetFromBottom > 0 || cur.Scroll.OffsetFromBottom > 0 {
		f.Scroll = &ScrollInfo{
			OffsetFromBottom:    cur.Scroll.OffsetFromBottom,
			MaxOffsetFromBottom: cur.Scroll.MaxOffsetFromBottom,
			ViewportRows:        cur.Scroll.ViewportRows,
		}
	}
	return f
}

// --- Framing codec: [u32-LE length][JSON payload] ---------------------------

// WriteMessage marshals m to JSON and writes it as a length-prefixed frame.
func WriteMessage(w io.Writer, m any) error {
	payload, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("orchestration: marshal: %w", err)
	}
	if len(payload) > MaxFrameSize {
		return fmt.Errorf("orchestration: message %d exceeds max %d", len(payload), MaxFrameSize)
	}
	var hdr [4]byte
	binary.LittleEndian.PutUint32(hdr[:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err = w.Write(payload)
	return err
}

// ReadMessage reads one frame and returns its type plus the raw JSON payload.
// Callers unmarshal the payload into the concrete message struct for that type.
func ReadMessage(r io.Reader) (MessageType, []byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return "", nil, err
	}
	n := binary.LittleEndian.Uint32(hdr[:])
	if n > MaxFrameSize {
		return "", nil, fmt.Errorf("orchestration: frame length %d exceeds max %d", n, MaxFrameSize)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return "", nil, err
	}
	var env struct {
		Type MessageType `json:"type"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		return "", nil, fmt.Errorf("orchestration: decode type: %w", err)
	}
	return env.Type, payload, nil
}
