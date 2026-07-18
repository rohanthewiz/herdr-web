//go:build ghostty

package main

import (
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/rohanthewiz/herdr-web/internal/app"
	"github.com/rohanthewiz/herdr-web/internal/browserproto"
	"github.com/rohanthewiz/herdr-web/internal/layout"
	"github.com/rohanthewiz/herdr-web/internal/orchestration"
	"github.com/rohanthewiz/herdr-web/internal/persist"
)

// pipeDaemon wires a net.Pipe into the orch's daemon and pumps every β message
// the orch sends into a channel (net.Pipe writes block until read, so the pump
// must run concurrently).
type pipeDaemon struct {
	msgs chan pipeMsg
	conn net.Conn
}

type pipeMsg struct {
	mt      orchestration.MessageType
	payload []byte
}

func newPipeDaemon(t *testing.T, o *orch) *pipeDaemon {
	t.Helper()
	client, server := net.Pipe()
	o.daemon.setConn(client)
	pd := &pipeDaemon{msgs: make(chan pipeMsg, 64), conn: server}
	go func() {
		for {
			mt, payload, err := orchestration.ReadMessage(server)
			if err != nil {
				close(pd.msgs)
				return
			}
			pd.msgs <- pipeMsg{mt, payload}
		}
	}()
	t.Cleanup(func() { _ = server.Close(); _ = client.Close() })
	return pd
}

// expect pops messages until one of type want arrives (skipping others).
func (pd *pipeDaemon) expect(t *testing.T, want orchestration.MessageType) []byte {
	t.Helper()
	for {
		select {
		case m, ok := <-pd.msgs:
			if !ok {
				t.Fatalf("pipe closed while waiting for %q", want)
			}
			if m.mt == want {
				return m.payload
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for %q", want)
		}
	}
}

// A restored snapshot drives the orchestrator exactly like a live-built model:
// runtimes exist for every pane, and the viewport layout matches the original.
func TestNewOrchWithRestoredSession(t *testing.T) {
	o, err := newOrch(filepath.Join(t.TempDir(), "s.sock"), t.TempDir())
	if err != nil {
		t.Fatalf("newOrch: %v", err)
	}
	if _, err := o.session.SplitPane(nil, layout.Horizontal); err != nil {
		t.Fatalf("split: %v", err)
	}
	if _, err := o.session.SplitPane(nil, layout.Vertical); err != nil {
		t.Fatalf("split2: %v", err)
	}
	o.applyModel()

	snap := o.session.Snapshot()
	sess, err := app.RestoreSession(modelSpawner{}, snap)
	if err != nil {
		t.Fatalf("RestoreSession: %v", err)
	}
	r := newOrchWith(filepath.Join(t.TempDir(), "s2.sock"), t.TempDir(), sess)

	for _, id := range sess.AllPaneIDs() {
		if r.panes[uint32(id)] == nil {
			t.Fatalf("no runtime for restored pane %d", id)
		}
	}
	orig, rest := o.viewportLayout(), r.viewportLayout()
	if len(orig.Panes) != len(rest.Panes) {
		t.Fatalf("viewport panes: got %d want %d", len(rest.Panes), len(orig.Panes))
	}
	for i := range orig.Panes {
		if orig.Panes[i].Pane != rest.Panes[i].Pane || orig.Panes[i].Rect != rest.Panes[i].Rect ||
			orig.Panes[i].Pub != rest.Panes[i].Pub || orig.Panes[i].Focused != rest.Panes[i].Focused {
			t.Fatalf("pane %d: got %+v want %+v", i, rest.Panes[i], orig.Panes[i])
		}
	}
}

// createPane must re-spawn a cold-start pane in its saved cwd with its saved
// scrollback as initial_history — and consume both exactly once.
func TestCreatePaneConsumesSeedAndCwd(t *testing.T) {
	o, err := newOrch(filepath.Join(t.TempDir(), "s.sock"), t.TempDir())
	if err != nil {
		t.Fatalf("newOrch: %v", err)
	}
	pd := newPipeDaemon(t, o)

	pid := uint32(o.session.AllPaneIDs()[0])
	o.seeds[pid] = "old \x1b[32mscrollback\x1b[0m\r\n"
	o.restoredCwds[pid] = "/tmp/restored-here"
	rt := o.panes[pid]
	rt.created = false

	synced := make(chan struct{})
	go func() { o.syncDaemon(); close(synced) }() // pipe writes block until the pump reads

	var cp orchestration.CreatePane
	if err := json.Unmarshal(pd.expect(t, orchestration.MsgCreatePane), &cp); err != nil {
		t.Fatalf("unmarshal create_pane: %v", err)
	}
	<-synced
	if cp.PaneID != pid || cp.InitialHistory != "old \x1b[32mscrollback\x1b[0m\r\n" || cp.Cwd != "/tmp/restored-here" {
		t.Fatalf("create_pane: %+v", cp)
	}
	if _, ok := o.seeds[pid]; ok {
		t.Fatal("seed not consumed")
	}
	if _, ok := o.restoredCwds[pid]; ok {
		t.Fatal("restored cwd not consumed")
	}
}

// With no daemon connection, a create keeps the seed for the retry that
// reconcile triggers once the daemon comes back.
func TestCreatePaneKeepsSeedWhenDisconnected(t *testing.T) {
	o, err := newOrch(filepath.Join(t.TempDir(), "s.sock"), t.TempDir())
	if err != nil {
		t.Fatalf("newOrch: %v", err)
	}
	pid := uint32(o.session.AllPaneIDs()[0])
	o.seeds[pid] = "keep me"
	rt := o.panes[pid]
	rt.created = false

	o.syncDaemon() // disconnected: send dropped

	if o.seeds[pid] != "keep me" {
		t.Fatal("seed must survive a dropped (disconnected) create")
	}
}

// A model mutation arms the debounced save, which lands a loadable, restorable
// session file.
func TestSaveSoonWritesSessionFile(t *testing.T) {
	o, err := newOrch(filepath.Join(t.TempDir(), "s.sock"), t.TempDir())
	if err != nil {
		t.Fatalf("newOrch: %v", err)
	}
	o.sessionPath = persist.SessionPath(t.TempDir())
	go o.run()

	done := make(chan struct{})
	o.post(func() {
		if _, err := o.session.SplitPane(nil, layout.Horizontal); err != nil {
			t.Errorf("split: %v", err)
		}
		o.applyModel()
		close(done)
	})
	<-done

	deadline := time.Now().Add(3 * time.Second)
	for {
		snap, _, err := persist.LoadSession(o.sessionPath)
		if err == nil {
			if _, err := app.RestoreSession(modelSpawner{}, snap); err != nil {
				t.Fatalf("saved snapshot does not restore: %v", err)
			}
			if n := len(snap.Workspaces[0].Tabs[0].Panes); n != 2 {
				t.Fatalf("saved panes: got %d want 2", n)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("session file never appeared: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The shutdown sweep captures every live pane (ANSI), writes history.json, and
// only then fires the continuation.
func TestFinalCaptureSweep(t *testing.T) {
	o, err := newOrch(filepath.Join(t.TempDir(), "s.sock"), t.TempDir())
	if err != nil {
		t.Fatalf("newOrch: %v", err)
	}
	o.historyPath = persist.HistoryPath(t.TempDir())
	o.histLines = 1234
	pd := newPipeDaemon(t, o)
	pid := uint32(o.session.AllPaneIDs()[0])
	o.panes[pid].created = true
	go o.run() // orch state is loop-goroutine-only; drive it like production does

	stopped := make(chan struct{})
	o.post(func() { o.beginFinalCapture(func() { close(stopped) }) })

	var req orchestration.RequestText
	if err := json.Unmarshal(pd.expect(t, orchestration.MsgRequestText), &req); err != nil {
		t.Fatalf("unmarshal request_text: %v", err)
	}
	if req.PaneID != pid || !req.Ansi || req.Lines != 1234 {
		t.Fatalf("request_text: %+v", req)
	}
	select {
	case <-stopped:
		t.Fatal("done fired before the capture resolved")
	default:
	}

	// Deliver the reply the way the daemon dispatch would.
	o.post(func() {
		o.resolvePending(reqKey{pid, reqText}, browserproto.CaptureResult{Text: "FINAL\r\n"})
	})

	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("done did not fire after the last capture")
	}
	seeds, err := persist.LoadHistory(o.historyPath)
	if err != nil {
		t.Fatalf("LoadHistory: %v", err)
	}
	if seeds[pid] != "FINAL\r\n" {
		t.Fatalf("history: %+v", seeds)
	}
}

// With persistence off (no history path), shutdown completes synchronously.
func TestFinalCaptureDisabled(t *testing.T) {
	o, err := newOrch(filepath.Join(t.TempDir(), "s.sock"), t.TempDir())
	if err != nil {
		t.Fatalf("newOrch: %v", err)
	}
	stopped := false
	o.beginFinalCapture(func() { stopped = true })
	if !stopped {
		t.Fatal("done must fire synchronously with persistence off")
	}
}

// The periodic sweep only captures panes with output since the last one.
func TestPeriodicCaptureIsActivityGated(t *testing.T) {
	o, err := newOrch(filepath.Join(t.TempDir(), "s.sock"), t.TempDir())
	if err != nil {
		t.Fatalf("newOrch: %v", err)
	}
	o.historyPath = persist.HistoryPath(t.TempDir())
	newPipeDaemon(t, o)
	pid := uint32(o.session.AllPaneIDs()[0])
	o.panes[pid].created = true

	if n := o.captureHistory(false); n != 0 {
		t.Fatalf("clean pane captured: %d", n)
	}
	o.panes[pid].histDirty = true
	done := make(chan int)
	go func() { done <- o.captureHistory(false) }() // the pump drains the pipe write
	select {
	case n := <-done:
		if n != 1 {
			t.Fatalf("dirty pane not captured: %d", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("captureHistory stuck")
	}
	if o.panes[pid].histDirty {
		t.Fatal("histDirty not cleared after capture")
	}
}
