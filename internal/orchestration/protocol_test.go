package orchestration

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/rohanthewiz/herdr-web/internal/terminal"
)

func TestColorPacking(t *testing.T) {
	got := packRGB(terminal.Color{R: 0x12, G: 0x34, B: 0x56})
	want := uint32(0x02123456)
	if got != want {
		t.Fatalf("packRGB = %#08x, want %#08x", got, want)
	}
}

func TestModifierBits(t *testing.T) {
	c := terminal.Cell{Bold: true, Italic: true, Inverse: true}
	got := modifierBits(c)
	want := modBold | modItalic | modReversed
	if got != want {
		t.Fatalf("modifierBits = %#b, want %#b", got, want)
	}
	if modifierBits(terminal.Cell{}) != 0 {
		t.Fatal("plain cell should have no modifier bits")
	}
}

func mkSnap(cols, rows uint16, rows2d [][]terminal.Cell, cur terminal.Cursor) *terminal.Snapshot {
	return &terminal.Snapshot{
		Cols:      cols,
		Rows:      rows,
		Cells:     rows2d,
		Cursor:    cur,
		DefaultFg: terminal.Color{R: 200, G: 200, B: 200},
		DefaultBg: terminal.Color{R: 0, G: 0, B: 0},
	}
}

func TestFrameFromSnapshotFull(t *testing.T) {
	red := terminal.Color{R: 0xcc, G: 0x66, B: 0x66}
	cells := [][]terminal.Cell{{
		{Rune: "h", Fg: &red, Bold: true},
		{Rune: ""}, // blank ⇒ should become " "
	}}
	cur := terminal.Cursor{X: 1, Y: 0, Visible: true, Style: terminal.CursorBar}
	f := FrameFromSnapshot(mkSnap(2, 1, cells, cur), nil)

	if !f.Full {
		t.Fatal("frame from nil prev should be full")
	}
	if f.Cols != 2 || f.Rows != 1 || len(f.Cells) != 2 {
		t.Fatalf("dims/cells wrong: %dx%d, %d cells", f.Cols, f.Rows, len(f.Cells))
	}
	h := f.Cells[0]
	if h.Symbol != "h" || h.Fg != packRGB(red) || h.Modifier != modBold || h.Skip {
		t.Errorf("cell h = %+v", h)
	}
	blank := f.Cells[1]
	if blank.Symbol != " " || blank.Fg != packRGB(terminal.Color{R: 200, G: 200, B: 200}) {
		t.Errorf("blank cell should be space with default fg, got %+v", blank)
	}
	if f.Cursor == nil || f.Cursor.X != 1 || !f.Cursor.Visible || f.Cursor.Shape != 6 {
		t.Errorf("cursor = %+v, want x=1 visible shape=6(bar)", f.Cursor)
	}
}

func TestFrameFromSnapshotDiff(t *testing.T) {
	prevCells := [][]terminal.Cell{{{Rune: "a"}, {Rune: "b"}}}
	curCells := [][]terminal.Cell{{{Rune: "a"}, {Rune: "X"}}}
	cur := terminal.Cursor{Visible: true}
	prev := mkSnap(2, 1, prevCells, cur)
	f := FrameFromSnapshot(mkSnap(2, 1, curCells, cur), prev)

	if f.Full {
		t.Fatal("diff frame should not be full")
	}
	if !f.Cells[0].Skip {
		t.Error("unchanged cell 0 should be skipped")
	}
	if f.Cells[1].Skip || f.Cells[1].Symbol != "X" {
		t.Errorf("changed cell 1 should not be skipped: %+v", f.Cells[1])
	}
}

func TestFrameFromSnapshotHyperlinks(t *testing.T) {
	const url = "https://example.com/a"
	cells := [][]terminal.Cell{{
		{Rune: "l", Link: url},
		{Rune: "k", Link: url}, // same URI ⇒ deduped to one table entry
		{Rune: "x"},            // no link
	}}
	snap := mkSnap(3, 1, cells, terminal.Cursor{Visible: true})
	snap.HasHyperlinks = true
	// A prev exists with identical dims, but a link-bearing frame must still be full.
	prev := mkSnap(3, 1, [][]terminal.Cell{{{Rune: "l"}, {Rune: "k"}, {Rune: "x"}}}, terminal.Cursor{Visible: true})

	f := FrameFromSnapshot(snap, prev)

	if !f.Full {
		t.Fatal("a frame carrying hyperlinks must be sent full")
	}
	if len(f.Hyperlinks) != 1 || f.Hyperlinks[0] != url {
		t.Fatalf("hyperlinks table = %v, want one entry %q", f.Hyperlinks, url)
	}
	if f.Cells[0].Hyperlink == nil || *f.Cells[0].Hyperlink != 0 {
		t.Errorf("cell 0 should index hyperlink 0, got %v", f.Cells[0].Hyperlink)
	}
	if f.Cells[1].Hyperlink == nil || *f.Cells[1].Hyperlink != 0 {
		t.Errorf("cell 1 should share hyperlink index 0, got %v", f.Cells[1].Hyperlink)
	}
	if f.Cells[2].Hyperlink != nil {
		t.Errorf("cell 2 has no link, want nil index, got %v", f.Cells[2].Hyperlink)
	}
}

func TestFrameFromSnapshotNoHyperlinksOmitsTable(t *testing.T) {
	f := FrameFromSnapshot(mkSnap(1, 1, [][]terminal.Cell{{{Rune: "a"}}}, terminal.Cursor{}), nil)
	if f.Hyperlinks != nil {
		t.Errorf("no-link frame should have nil hyperlinks table, got %v", f.Hyperlinks)
	}
	if f.Cells[0].Hyperlink != nil {
		t.Errorf("no-link cell should have nil hyperlink index")
	}
}

func TestFrameDiffForcesFullOnResize(t *testing.T) {
	prev := mkSnap(2, 1, [][]terminal.Cell{{{Rune: "a"}, {Rune: "b"}}}, terminal.Cursor{})
	cur := mkSnap(3, 1, [][]terminal.Cell{{{Rune: "a"}, {Rune: "b"}, {Rune: "c"}}}, terminal.Cursor{})
	f := FrameFromSnapshot(cur, prev)
	if !f.Full {
		t.Fatal("dimension change should force a full frame")
	}
	for i, c := range f.Cells {
		if c.Skip {
			t.Fatalf("cell %d skipped in a full frame", i)
		}
	}
}

func TestCodecRoundTrip(t *testing.T) {
	hl := uint32(7)
	msgs := []any{
		NewHello(),
		NewCreatePane(42, 80, 24),
		NewInput(42, []byte{0x1b, 'h', 'i', 0x00}),
		NewResize(42, 100, 30),
		NewClosePane(42),
		NewRequestSelection(42, SelectionPoint{Row: 3, Col: 0}, SelectionPoint{Row: 1, Col: 7}, true),
		NewRequestText(42, uint8(terminal.TextRecent), 100, true, true),
		NewShutdown(),
		NewWelcome("", []uint32{1, 2, 3}),
		NewPaneExited(42, 0),
		NewError(42, "boom"),
		NewPaneSelection(42, "hello world"),
		NewPaneText(42, "scrollback"),
		NewPaneModes(42, terminal.InputModes{
			BracketedPaste: true, MouseMode: terminal.MouseAnyMotion,
			MouseEncoding: terminal.MouseEncodingSGR, KittyKeyboardFlags: 5,
		}),
		NewPaneFrame(42, &Frame{
			Cols: 1, Rows: 1, Full: true,
			Cursor: &Cursor{X: 0, Y: 0, Visible: true, Shape: 2},
			Cells:  []Cell{{Symbol: "h", Fg: 0x02112233, Bg: 0x02000000, Modifier: modBold, Hyperlink: &hl}},
		}),
	}

	var buf bytes.Buffer
	for _, m := range msgs {
		if err := WriteMessage(&buf, m); err != nil {
			t.Fatalf("WriteMessage(%T): %v", m, err)
		}
	}

	wantTypes := []MessageType{
		MsgHello, MsgCreatePane, MsgInput, MsgResize, MsgClosePane, MsgRequestSelection, MsgRequestText,
		MsgShutdown, MsgWelcome, MsgPaneExited, MsgError, MsgPaneSelection, MsgPaneText, MsgPaneModes, MsgPaneFrame,
	}
	for i, want := range wantTypes {
		typ, payload, err := ReadMessage(&buf)
		if err != nil {
			t.Fatalf("ReadMessage[%d]: %v", i, err)
		}
		if typ != want {
			t.Fatalf("message[%d] type = %q, want %q", i, typ, want)
		}
		// spot-check a couple of payloads decode into their structs
		switch typ {
		case MsgWelcome:
			var w Welcome
			if err := json.Unmarshal(payload, &w); err != nil {
				t.Fatalf("decode welcome: %v", err)
			}
			if w.ProtocolVersion != ProtocolVersion || len(w.Panes) != 3 ||
				w.Panes[0] != 1 || w.Panes[2] != 3 {
				t.Errorf("welcome round-trip wrong: %+v", w)
			}
		case MsgRequestSelection:
			var rs RequestSelection
			if err := json.Unmarshal(payload, &rs); err != nil {
				t.Fatalf("decode request_selection: %v", err)
			}
			if rs.PaneID != 42 || rs.Anchor != (SelectionPoint{Row: 3, Col: 0}) ||
				rs.Cursor != (SelectionPoint{Row: 1, Col: 7}) || !rs.Rectangle {
				t.Errorf("request_selection round-trip wrong: %+v", rs)
			}
		case MsgPaneSelection:
			var ps PaneSelection
			if err := json.Unmarshal(payload, &ps); err != nil {
				t.Fatalf("decode pane_selection: %v", err)
			}
			if ps.PaneID != 42 || ps.Text != "hello world" {
				t.Errorf("pane_selection round-trip wrong: %+v", ps)
			}
		case MsgRequestText:
			var rt RequestText
			if err := json.Unmarshal(payload, &rt); err != nil {
				t.Fatalf("decode request_text: %v", err)
			}
			if rt.PaneID != 42 || rt.Scope != uint8(terminal.TextRecent) ||
				rt.Lines != 100 || !rt.Ansi || !rt.Unwrap {
				t.Errorf("request_text round-trip wrong: %+v", rt)
			}
		case MsgPaneText:
			var pt PaneText
			if err := json.Unmarshal(payload, &pt); err != nil {
				t.Fatalf("decode pane_text: %v", err)
			}
			if pt.PaneID != 42 || pt.Text != "scrollback" {
				t.Errorf("pane_text round-trip wrong: %+v", pt)
			}
		case MsgPaneModes:
			var pm PaneModes
			if err := json.Unmarshal(payload, &pm); err != nil {
				t.Fatalf("decode pane_modes: %v", err)
			}
			if pm.PaneID != 42 || !pm.BracketedPaste ||
				pm.MouseMode != uint8(terminal.MouseAnyMotion) ||
				pm.MouseEncoding != uint8(terminal.MouseEncodingSGR) ||
				pm.KittyKeyboardFlags != 5 {
				t.Errorf("pane_modes round-trip wrong: %+v", pm)
			}
		case MsgInput:
			var in Input
			if err := json.Unmarshal(payload, &in); err != nil {
				t.Fatalf("decode input: %v", err)
			}
			if in.PaneID != 42 || !bytes.Equal(in.Data, []byte{0x1b, 'h', 'i', 0x00}) {
				t.Errorf("input round-trip wrong: %+v", in)
			}
		case MsgPaneFrame:
			var pf PaneFrame
			if err := json.Unmarshal(payload, &pf); err != nil {
				t.Fatalf("decode pane_frame: %v", err)
			}
			if pf.Frame == nil || len(pf.Frame.Cells) != 1 || pf.Frame.Cells[0].Symbol != "h" {
				t.Errorf("pane_frame round-trip wrong: %+v", pf.Frame)
			}
			if pf.Frame.Cells[0].Hyperlink == nil || *pf.Frame.Cells[0].Hyperlink != 7 {
				t.Errorf("hyperlink round-trip wrong: %+v", pf.Frame.Cells[0].Hyperlink)
			}
		}
	}

	if buf.Len() != 0 {
		t.Errorf("%d trailing bytes after reading all messages", buf.Len())
	}
}
