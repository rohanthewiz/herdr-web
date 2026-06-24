//go:build ghostty

package orchestration

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/rohanthewiz/herdr-web/internal/terminal"
)

// startTestHost spins up a Host serving one end of an in-memory pipe and returns
// the client end, with the Hello/Welcome handshake already done.
func startTestHost(t *testing.T) net.Conn {
	t.Helper()
	serverEnd, clientEnd := net.Pipe()

	h := NewHost()
	h.FlushInterval = 5 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	go h.Serve(ctx, serverEnd)

	t.Cleanup(func() {
		cancel()
		clientEnd.Close()
	})

	// Overall safety deadline so a stuck test fails instead of hanging.
	_ = clientEnd.SetDeadline(time.Now().Add(15 * time.Second))

	if err := WriteMessage(clientEnd, NewHello()); err != nil {
		t.Fatalf("send hello: %v", err)
	}
	typ, _ := readEvent(t, clientEnd)
	if typ != MsgWelcome {
		t.Fatalf("first event = %q, want welcome", typ)
	}
	return clientEnd
}

func readEvent(t *testing.T, c net.Conn) (MessageType, []byte) {
	t.Helper()
	typ, payload, err := ReadMessage(c)
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	return typ, payload
}

func frameText(f *Frame) string {
	var b strings.Builder
	for _, c := range f.Cells {
		b.WriteString(c.Symbol)
	}
	return b.String()
}

func TestHostRunsCommandAndReportsFrames(t *testing.T) {
	c := startTestHost(t)

	cp := NewCreatePane(1, 40, 5)
	cp.Command = "/bin/sh"
	cp.Args = []string{"-c", "printf HELLO"}
	if err := WriteMessage(c, cp); err != nil {
		t.Fatalf("create_pane: %v", err)
	}

	var transcript strings.Builder
	for {
		typ, payload := readEvent(t, c)
		switch typ {
		case MsgPaneFrame:
			var pf PaneFrame
			if err := json.Unmarshal(payload, &pf); err != nil {
				t.Fatalf("decode pane_frame: %v", err)
			}
			if pf.PaneID != 1 {
				t.Fatalf("frame for pane %d, want 1", pf.PaneID)
			}
			transcript.WriteString(frameText(pf.Frame))
		case MsgPaneExited:
			var pe PaneExited
			if err := json.Unmarshal(payload, &pe); err != nil {
				t.Fatalf("decode pane_exited: %v", err)
			}
			if pe.PaneID != 1 {
				t.Fatalf("exited for pane %d, want 1", pe.PaneID)
			}
			if pe.ExitCode != 0 {
				t.Errorf("exit code = %d, want 0", pe.ExitCode)
			}
			if !strings.Contains(transcript.String(), "HELLO") {
				t.Fatalf("never saw HELLO in frames; transcript=%q", transcript.String())
			}
			return
		case MsgError:
			t.Fatalf("unexpected error event: %s", string(payload))
		}
	}
}

func TestHostReportsPaneCwd(t *testing.T) {
	c := startTestHost(t)

	cp := NewCreatePane(3, 40, 5)
	cp.Command = "/bin/sh"
	// Emit an OSC 7 working-directory report on stdout, then linger briefly so the
	// flusher observes the pwd change before the child exits.
	cp.Args = []string{"-c", `printf '\033]7;file://localhost/tmp\033\\'; sleep 0.3`}
	if err := WriteMessage(c, cp); err != nil {
		t.Fatalf("create_pane: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		typ, payload := readEvent(t, c)
		switch typ {
		case MsgPaneCwd:
			var pc PaneCwd
			if err := json.Unmarshal(payload, &pc); err != nil {
				t.Fatalf("decode pane_cwd: %v", err)
			}
			if pc.PaneID != 3 {
				t.Fatalf("pane_cwd for pane %d, want 3", pc.PaneID)
			}
			if pc.Cwd != "/tmp" {
				t.Fatalf("pane_cwd = %q, want /tmp", pc.Cwd)
			}
			return
		case MsgError:
			t.Fatalf("unexpected error event: %s", string(payload))
		}
	}
	t.Fatal("never received pane_cwd")
}

func TestHostReportsAgent(t *testing.T) {
	c := startTestHost(t)

	cp := NewCreatePane(4, 40, 5)
	cp.Command = "/bin/sh"
	// Advertise argv[0]="codex" over a real binary so process-based detection
	// identifies the agent without needing one installed.
	cp.Args = []string{"-c", "exec -a codex sleep 3"}
	if err := WriteMessage(c, cp); err != nil {
		t.Fatalf("create_pane: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		typ, payload := readEvent(t, c)
		switch typ {
		case MsgPaneAgent:
			var pa PaneAgent
			if err := json.Unmarshal(payload, &pa); err != nil {
				t.Fatalf("decode pane_agent: %v", err)
			}
			if pa.PaneID != 4 {
				t.Fatalf("pane_agent for pane %d, want 4", pa.PaneID)
			}
			if pa.Agent == "codex" {
				return // identity reported
			}
		case MsgError:
			t.Fatalf("unexpected error event: %s", string(payload))
		}
	}
	t.Fatal("never received pane_agent with agent=codex")
}

func TestHostReportsAgentWorkingState(t *testing.T) {
	c := startTestHost(t)

	cp := NewCreatePane(5, 40, 5)
	cp.Command = "/bin/sh"
	// A foreground process named "pi" (agent) that prints the pi manifest's
	// working marker — exercises identity + manifest state classification. It must
	// outlive the Stage C startup grace window (3s), during which the screen is not
	// yet scanned, so it sleeps comfortably past it.
	cp.Args = []string{"-c", `exec -a pi sh -c 'printf "Working..."; sleep 8'`}
	if err := WriteMessage(c, cp); err != nil {
		t.Fatalf("create_pane: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		typ, payload := readEvent(t, c)
		switch typ {
		case MsgPaneAgent:
			var pa PaneAgent
			if err := json.Unmarshal(payload, &pa); err != nil {
				t.Fatalf("decode pane_agent: %v", err)
			}
			if pa.Agent == "pi" && pa.State == "working" {
				if !pa.VisibleWorking {
					t.Errorf("expected visible_working for working state")
				}
				return
			}
		case MsgError:
			t.Fatalf("unexpected error event: %s", string(payload))
		}
	}
	t.Fatal("never received pane_agent with agent=pi state=working")
}

func TestHostReportsPaneClipboard(t *testing.T) {
	c := startTestHost(t)

	cp := NewCreatePane(6, 40, 5)
	cp.Command = "/bin/sh"
	// Emit an OSC 52 clipboard write ("hello" = aGVsbG8=) on stdout, then linger
	// briefly so the read pump forwards it before the child exits.
	cp.Args = []string{"-c", `printf '\033]52;c;aGVsbG8=\033\\'; sleep 0.3`}
	if err := WriteMessage(c, cp); err != nil {
		t.Fatalf("create_pane: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		typ, payload := readEvent(t, c)
		switch typ {
		case MsgPaneClipboard:
			var pc PaneClipboard
			if err := json.Unmarshal(payload, &pc); err != nil {
				t.Fatalf("decode pane_clipboard: %v", err)
			}
			if pc.PaneID != 6 {
				t.Fatalf("pane_clipboard for pane %d, want 6", pc.PaneID)
			}
			if string(pc.Data) != "hello" {
				t.Fatalf("pane_clipboard data = %q, want hello", pc.Data)
			}
			return
		case MsgError:
			t.Fatalf("unexpected error event: %s", string(payload))
		}
	}
	t.Fatal("never received pane_clipboard")
}

func TestHostReportsPaneTitle(t *testing.T) {
	c := startTestHost(t)

	cp := NewCreatePane(7, 40, 5)
	cp.Command = "/bin/sh"
	// Emit an OSC 2 window-title report on stdout, then linger briefly so the read
	// pump forwards it before the child exits.
	cp.Args = []string{"-c", `printf '\033]2;vim - main.go\033\\'; sleep 0.3`}
	if err := WriteMessage(c, cp); err != nil {
		t.Fatalf("create_pane: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		typ, payload := readEvent(t, c)
		switch typ {
		case MsgPaneTitle:
			var pt PaneTitle
			if err := json.Unmarshal(payload, &pt); err != nil {
				t.Fatalf("decode pane_title: %v", err)
			}
			if pt.PaneID != 7 {
				t.Fatalf("pane_title for pane %d, want 7", pt.PaneID)
			}
			if pt.Title != "vim - main.go" {
				t.Fatalf("pane_title = %q, want %q", pt.Title, "vim - main.go")
			}
			return
		case MsgError:
			t.Fatalf("unexpected error event: %s", string(payload))
		}
	}
	t.Fatal("never received pane_title")
}

func TestHostReportsHyperlinkFrame(t *testing.T) {
	c := startTestHost(t)

	cp := NewCreatePane(8, 40, 5)
	cp.Command = "/bin/sh"
	// Emit an OSC 8 hyperlink ("link" wrapped in a link to example.com), then
	// linger so the flusher emits a frame carrying it before the child exits.
	cp.Args = []string{"-c", `printf '\033]8;;https://example.com\033\\link\033]8;;\033\\'; sleep 0.5`}
	if err := WriteMessage(c, cp); err != nil {
		t.Fatalf("create_pane: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		typ, payload := readEvent(t, c)
		switch typ {
		case MsgPaneFrame:
			var pf PaneFrame
			if err := json.Unmarshal(payload, &pf); err != nil {
				t.Fatalf("decode pane_frame: %v", err)
			}
			if pf.PaneID != 8 || pf.Frame == nil || len(pf.Frame.Hyperlinks) == 0 {
				continue // wait for the frame that carries the link table
			}
			if pf.Frame.Hyperlinks[0] != "https://example.com" {
				t.Fatalf("hyperlink table = %v, want [https://example.com]", pf.Frame.Hyperlinks)
			}
			// At least one cell must index into the table.
			for _, cell := range pf.Frame.Cells {
				if cell.Hyperlink != nil && *cell.Hyperlink == 0 {
					return // link plumbed end-to-end
				}
			}
			t.Fatal("hyperlink table present but no cell indexes it")
		case MsgError:
			t.Fatalf("unexpected error event: %s", string(payload))
		}
	}
	t.Fatal("never received a pane_frame carrying a hyperlink")
}

func TestHostScrollbackReportsMetrics(t *testing.T) {
	c := startTestHost(t)

	cp := NewCreatePane(9, 20, 3) // 3 rows ⇒ output quickly fills scrollback
	cp.Command = "/bin/sh"
	// Print 30 numbered lines (most scroll into history), then linger.
	cp.Args = []string{"-c", `for i in $(seq 1 30); do printf "line%d\r\n" "$i"; done; sleep 1`}
	if err := WriteMessage(c, cp); err != nil {
		t.Fatalf("create_pane: %v", err)
	}

	// Wait for a frame reporting scrollback history, then scroll up into it.
	deadline := time.Now().Add(10 * time.Second)
	scrolled := false
	for time.Now().Before(deadline) {
		typ, payload := readEvent(t, c)
		switch typ {
		case MsgPaneFrame:
			var pf PaneFrame
			if err := json.Unmarshal(payload, &pf); err != nil {
				t.Fatalf("decode pane_frame: %v", err)
			}
			if pf.PaneID != 9 || pf.Frame == nil || pf.Frame.Scroll == nil {
				continue
			}
			s := pf.Frame.Scroll
			if s.ViewportRows != 3 {
				t.Fatalf("viewport_rows = %d, want 3", s.ViewportRows)
			}
			if s.MaxOffsetFromBottom == 0 {
				t.Fatalf("expected scrollback history, got max offset 0")
			}
			if !scrolled {
				// First metrics-bearing frame: we're at the bottom. Scroll up.
				if s.OffsetFromBottom != 0 {
					t.Fatalf("expected offset 0 at bottom, got %d", s.OffsetFromBottom)
				}
				scrolled = true
				if err := WriteMessage(c, NewScrollViewport(9, -5)); err != nil {
					t.Fatalf("scroll_viewport: %v", err)
				}
				continue
			}
			// After scrolling up, a frame should report a non-zero offset.
			if s.OffsetFromBottom > 0 {
				return
			}
		case MsgError:
			t.Fatalf("unexpected error event: %s", string(payload))
		}
	}
	t.Fatal("never observed a scrolled-up frame with non-zero offset")
}

func TestHostReportsPaneSelection(t *testing.T) {
	c := startTestHost(t)

	cp := NewCreatePane(11, 40, 5)
	cp.Command = "/bin/sh"
	// Print a known line on row 0, then linger so the pane is still open when we
	// request the selection. No scrollback happens, so screen row 0 == the line.
	cp.Args = []string{"-c", `printf 'HELLO WORLD'; sleep 1`}
	if err := WriteMessage(c, cp); err != nil {
		t.Fatalf("create_pane: %v", err)
	}

	// Wait for a frame showing the line, then request "HELLO" (cols 0..4 inclusive).
	requested := false
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		typ, payload := readEvent(t, c)
		switch typ {
		case MsgPaneFrame:
			if requested {
				continue
			}
			var pf PaneFrame
			if err := json.Unmarshal(payload, &pf); err != nil {
				t.Fatalf("decode pane_frame: %v", err)
			}
			if !strings.Contains(frameText(pf.Frame), "HELLO WORLD") {
				continue
			}
			requested = true
			req := NewRequestSelection(11,
				SelectionPoint{Row: 0, Col: 0}, SelectionPoint{Row: 0, Col: 4}, false)
			if err := WriteMessage(c, req); err != nil {
				t.Fatalf("request_selection: %v", err)
			}
		case MsgPaneSelection:
			var ps PaneSelection
			if err := json.Unmarshal(payload, &ps); err != nil {
				t.Fatalf("decode pane_selection: %v", err)
			}
			if ps.PaneID != 11 {
				t.Fatalf("pane_selection for pane %d, want 11", ps.PaneID)
			}
			if ps.Text != "HELLO" {
				t.Fatalf("pane_selection text = %q, want %q", ps.Text, "HELLO")
			}
			return
		case MsgError:
			t.Fatalf("unexpected error event: %s", string(payload))
		}
	}
	t.Fatal("never received pane_selection")
}

func TestHostReportsPaneModes(t *testing.T) {
	c := startTestHost(t)

	cp := NewCreatePane(12, 40, 5)
	cp.Command = "/bin/sh"
	// Enable bracketed paste (DEC ?2004) and SGR mouse any-motion tracking (?1003,
	// ?1006), then linger so the flusher observes the mode change.
	cp.Args = []string{"-c", `printf '\033[?2004h\033[?1003h\033[?1006h'; sleep 0.5`}
	if err := WriteMessage(c, cp); err != nil {
		t.Fatalf("create_pane: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		typ, payload := readEvent(t, c)
		switch typ {
		case MsgPaneModes:
			var pm PaneModes
			if err := json.Unmarshal(payload, &pm); err != nil {
				t.Fatalf("decode pane_modes: %v", err)
			}
			if pm.PaneID != 12 {
				t.Fatalf("pane_modes for pane %d, want 12", pm.PaneID)
			}
			// Wait for the frame that carries our enabled modes (an initial all-off
			// report may arrive first).
			if !pm.BracketedPaste {
				continue
			}
			if pm.MouseMode != uint8(terminal.MouseAnyMotion) {
				t.Fatalf("mouse_mode = %d, want %d (any-motion)", pm.MouseMode, terminal.MouseAnyMotion)
			}
			if pm.MouseEncoding != uint8(terminal.MouseEncodingSGR) {
				t.Fatalf("mouse_encoding = %d, want %d (sgr)", pm.MouseEncoding, terminal.MouseEncodingSGR)
			}
			return
		case MsgError:
			t.Fatalf("unexpected error event: %s", string(payload))
		}
	}
	t.Fatal("never received pane_modes with bracketed paste enabled")
}

func TestHostReportsPaneText(t *testing.T) {
	c := startTestHost(t)

	cp := NewCreatePane(13, 20, 3) // 3 rows ⇒ output fills scrollback
	cp.Command = "/bin/sh"
	// Print 12 numbered lines (most scroll into history), then linger.
	cp.Args = []string{"-c", `for i in $(seq 1 12); do printf "row%d\r\n" "$i"; done; sleep 1`}
	if err := WriteMessage(c, cp); err != nil {
		t.Fatalf("create_pane: %v", err)
	}

	// Wait for a frame showing the latest line, then request recent scrollback text.
	requested := false
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		typ, payload := readEvent(t, c)
		switch typ {
		case MsgPaneFrame:
			if requested {
				continue
			}
			var pf PaneFrame
			if err := json.Unmarshal(payload, &pf); err != nil {
				t.Fatalf("decode pane_frame: %v", err)
			}
			if !strings.Contains(frameText(pf.Frame), "row12") {
				continue
			}
			requested = true
			// Whole buffer (lines=0), plain, unwrapped — the snapshot_history case;
			// reaches into scrollback history.
			req := NewRequestText(13, uint8(terminal.TextRecent), 0, false, true)
			if err := WriteMessage(c, req); err != nil {
				t.Fatalf("request_text: %v", err)
			}
		case MsgPaneText:
			var pt PaneText
			if err := json.Unmarshal(payload, &pt); err != nil {
				t.Fatalf("decode pane_text: %v", err)
			}
			if pt.PaneID != 13 {
				t.Fatalf("pane_text for pane %d, want 13", pt.PaneID)
			}
			// row1 is only in scrollback (only 3 rows visible), so its presence
			// proves the Go side read history, not just the viewport.
			if !strings.Contains(pt.Text, "row1\n") || !strings.Contains(pt.Text, "row12") {
				t.Fatalf("pane_text missing scrollback rows; got %q", pt.Text)
			}
			return
		case MsgError:
			t.Fatalf("unexpected error event: %s", string(payload))
		}
	}
	t.Fatal("never received pane_text")
}

func TestHostSeedsInitialHistory(t *testing.T) {
	c := startTestHost(t)

	cp := NewCreatePane(14, 40, 5)
	cp.Command = "/bin/sh"
	cp.Args = []string{"-c", "sleep 1"} // quiet shell; the history is what we assert
	cp.InitialHistory = "restored-line-A\r\nrestored-line-B\r\n"
	if err := WriteMessage(c, cp); err != nil {
		t.Fatalf("create_pane: %v", err)
	}

	// The seeded history must show in a frame (it was written before the child ran).
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		typ, payload := readEvent(t, c)
		switch typ {
		case MsgPaneFrame:
			var pf PaneFrame
			if err := json.Unmarshal(payload, &pf); err != nil {
				t.Fatalf("decode pane_frame: %v", err)
			}
			if strings.Contains(frameText(pf.Frame), "restored-line-A") &&
				strings.Contains(frameText(pf.Frame), "restored-line-B") {
				return
			}
		case MsgError:
			t.Fatalf("unexpected error event: %s", string(payload))
		}
	}
	t.Fatal("seeded history never rendered")
}

func TestHostInputEchoAndClose(t *testing.T) {
	c := startTestHost(t)

	cp := NewCreatePane(2, 40, 5)
	cp.Command = "/bin/cat" // PTY line discipline echoes input back
	if err := WriteMessage(c, cp); err != nil {
		t.Fatalf("create_pane: %v", err)
	}

	if err := WriteMessage(c, NewInput(2, []byte("ping\r"))); err != nil {
		t.Fatalf("input: %v", err)
	}

	// Read frames until the echoed input shows up.
	sawEcho := false
	deadline := time.Now().Add(10 * time.Second)
	for !sawEcho && time.Now().Before(deadline) {
		typ, payload := readEvent(t, c)
		switch typ {
		case MsgPaneFrame:
			var pf PaneFrame
			if err := json.Unmarshal(payload, &pf); err != nil {
				t.Fatalf("decode pane_frame: %v", err)
			}
			if strings.Contains(frameText(pf.Frame), "ping") {
				sawEcho = true
			}
		case MsgError:
			t.Fatalf("unexpected error event: %s", string(payload))
		}
	}
	if !sawEcho {
		t.Fatal("never saw echoed input 'ping'")
	}

	// Close the pane; expect a pane_exited for it.
	if err := WriteMessage(c, NewClosePane(2)); err != nil {
		t.Fatalf("close_pane: %v", err)
	}
	for {
		typ, payload := readEvent(t, c)
		if typ == MsgPaneExited {
			var pe PaneExited
			if err := json.Unmarshal(payload, &pe); err != nil {
				t.Fatalf("decode pane_exited: %v", err)
			}
			if pe.PaneID != 2 {
				t.Fatalf("exited for pane %d, want 2", pe.PaneID)
			}
			return
		}
	}
}

// --- Persistence 3b: reconnect / resync ------------------------------------

// newPersistentHost starts a persistent Host (panes outlive a connection) with
// the flusher running for the test's lifetime. Idle timeout is disabled so the
// daemon doesn't exit between connections.
func newPersistentHost(t *testing.T) (*Host, context.Context) {
	t.Helper()
	h := NewHost()
	h.FlushInterval = 5 * time.Millisecond
	h.Persistent = true
	h.IdleTimeout = 0
	ctx, cancel := context.WithCancel(context.Background())
	h.Start(ctx)
	t.Cleanup(func() {
		cancel()
		h.Stop()
	})
	return h, ctx
}

// attachClient binds a fresh in-memory connection to the Host as the active
// client and returns the client end plus a detach func (closes the client, which
// ends the attachment but leaves panes running). The Hello/Welcome handshake is
// left to the caller so tests can inspect the welcome's pane list.
func attachClient(t *testing.T, h *Host, ctx context.Context) (client net.Conn, detach func()) {
	t.Helper()
	serverEnd, clientEnd := net.Pipe()
	done := make(chan struct{})
	go func() {
		_ = h.Attach(ctx, serverEnd)
		close(done)
	}()
	_ = clientEnd.SetDeadline(time.Now().Add(15 * time.Second))
	t.Cleanup(func() { clientEnd.Close() })
	return clientEnd, func() {
		clientEnd.Close()
		<-done
	}
}

// waitForText reads events until a pane's accumulated frame text contains substr.
func waitForText(t *testing.T, c net.Conn, paneID uint32, substr string) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	var transcript strings.Builder
	for time.Now().Before(deadline) {
		typ, payload := readEvent(t, c)
		switch typ {
		case MsgPaneFrame:
			var pf PaneFrame
			if err := json.Unmarshal(payload, &pf); err != nil {
				t.Fatalf("decode pane_frame: %v", err)
			}
			if pf.PaneID == paneID {
				transcript.WriteString(frameText(pf.Frame))
				if strings.Contains(transcript.String(), substr) {
					return
				}
			}
		case MsgPaneExited:
			t.Fatalf("pane %d exited before %q appeared; transcript=%q", paneID, substr, transcript.String())
		case MsgError:
			t.Fatalf("unexpected error event: %s", string(payload))
		}
	}
	t.Fatalf("never saw %q in pane %d; transcript=%q", substr, paneID, transcript.String())
}

func containsU32(s []uint32, v uint32) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// TestHostPersistsPanesAcrossReconnect is the core 3b proof: a shell created on
// one connection survives the client disconnecting, and a second connection
// reconnects, is told the pane still exists (welcome.panes), is resynced its
// prior screen, and can drive the *same* live shell.
func TestHostPersistsPanesAcrossReconnect(t *testing.T) {
	h, ctx := newPersistentHost(t)

	// Connection 1: create a long-lived interactive shell and run a command.
	c1, detach1 := attachClient(t, h, ctx)
	if err := WriteMessage(c1, NewHello()); err != nil {
		t.Fatalf("hello: %v", err)
	}
	typ, _ := readEvent(t, c1)
	if typ != MsgWelcome {
		t.Fatalf("first event = %q, want welcome", typ)
	}

	cp := NewCreatePane(7, 40, 10)
	cp.Command = "/bin/sh" // interactive (reads the PTY), stays alive
	if err := WriteMessage(c1, cp); err != nil {
		t.Fatalf("create_pane: %v", err)
	}
	if err := WriteMessage(c1, NewInput(7, []byte("printf MARK1\n"))); err != nil {
		t.Fatalf("input: %v", err)
	}
	waitForText(t, c1, 7, "MARK1")
	detach1() // client goes away; the shell must keep running

	// Connection 2: reconnect. Welcome lists the surviving pane and a resync frame
	// carries its prior screen.
	c2, detach2 := attachClient(t, h, ctx)
	defer detach2()
	if err := WriteMessage(c2, NewHello()); err != nil {
		t.Fatalf("hello 2: %v", err)
	}
	typ, payload := readEvent(t, c2)
	if typ != MsgWelcome {
		t.Fatalf("reconnect first event = %q, want welcome", typ)
	}
	var w Welcome
	if err := json.Unmarshal(payload, &w); err != nil {
		t.Fatalf("decode welcome: %v", err)
	}
	if !containsU32(w.Panes, 7) {
		t.Fatalf("welcome.panes = %v, want it to list surviving pane 7", w.Panes)
	}
	waitForText(t, c2, 7, "MARK1") // resync replayed the prior screen

	// The shell is the same live process: a new command runs in it.
	if err := WriteMessage(c2, NewInput(7, []byte("printf MARK2\n"))); err != nil {
		t.Fatalf("input 2: %v", err)
	}
	waitForText(t, c2, 7, "MARK2")
}

// TestHostShutdownCommand verifies a clean-quit shutdown triggers daemon exit.
func TestHostShutdownCommand(t *testing.T) {
	h, ctx := newPersistentHost(t)
	c, _ := attachClient(t, h, ctx)
	if err := WriteMessage(c, NewHello()); err != nil {
		t.Fatalf("hello: %v", err)
	}
	if typ, _ := readEvent(t, c); typ != MsgWelcome {
		t.Fatalf("first event = %q, want welcome", typ)
	}
	if err := WriteMessage(c, NewShutdown()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	select {
	case <-h.Exit():
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown command did not trigger daemon Exit")
	}
}

// TestHostIdleTimeoutExits verifies a persistent daemon with no client attached
// exits after the idle timeout (a crashed herdr that never reconnects).
func TestHostIdleTimeoutExits(t *testing.T) {
	h := NewHost()
	h.FlushInterval = 5 * time.Millisecond
	h.Persistent = true
	h.IdleTimeout = 80 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		h.Stop()
	})
	h.Start(ctx) // arms the idle timer; no client ever attaches
	select {
	case <-h.Exit():
	case <-time.After(2 * time.Second):
		t.Fatal("idle timeout did not fire")
	}
}
