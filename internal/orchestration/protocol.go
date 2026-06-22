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
	MsgHello      MessageType = "hello"
	MsgCreatePane MessageType = "create_pane"
	MsgInput      MessageType = "input"
	MsgResize     MessageType = "resize"
	MsgClosePane  MessageType = "close_pane"

	// Go → Rust (events).
	MsgWelcome    MessageType = "welcome"
	MsgPaneFrame  MessageType = "pane_frame"
	MsgPaneCwd    MessageType = "pane_cwd"
	MsgPaneAgent  MessageType = "pane_agent"
	MsgPaneExited MessageType = "pane_exited"
	MsgError      MessageType = "error"
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

// --- Events (Go → Rust) -----------------------------------------------------

type Welcome struct {
	Type            MessageType `json:"type"`
	ProtocolVersion int         `json:"protocol_version"`
	Error           string      `json:"error,omitempty"`
}

func NewWelcome(errMsg string) Welcome {
	return Welcome{Type: MsgWelcome, ProtocolVersion: ProtocolVersion, Error: errMsg}
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
	full := prev == nil || prev.Cols != cur.Cols || prev.Rows != cur.Rows

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

	for y := uint16(0); y < cur.Rows; y++ {
		for x := uint16(0); x < cur.Cols; x++ {
			cell := resolveCell(cur, cur.At(x, y))
			if !full {
				if prevCell := resolveCell(prev, prev.At(x, y)); prevCell == cell {
					cell.Skip = true
				}
			}
			f.Cells = append(f.Cells, cell)
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
