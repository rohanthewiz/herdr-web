//go:build ghostty

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/rohanthewiz/herdr-web/internal/browserproto"
	"github.com/rohanthewiz/herdr-web/internal/orchestration"
	"github.com/rohanthewiz/herdr-web/internal/terminal"
)

// daemon manages the orchestrator's single connection to the termhost daemon:
// dial + hello/welcome, reconciling the daemon's surviving panes against the
// model, then pumping events until the connection drops — and redialing. All
// state that the pump touches beyond the raw socket lives in orch and is
// reached by posting closures onto the orchestrator loop (never a lock).
type daemon struct {
	o      *orch
	socket string

	mu   sync.Mutex // serializes writes; guards conn
	conn net.Conn
}

func (d *daemon) connected() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.conn != nil
}

// send writes one command to the daemon. Disconnected sends are dropped —
// reconcile replays the model when the connection comes back. Called from the
// orchestrator loop (which owns the decision to send).
func (d *daemon) send(m any) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.conn == nil {
		return
	}
	if err := orchestration.WriteMessage(d.conn, m); err != nil {
		log.Printf("gateway2: daemon write: %v", err)
		_ = d.conn.Close() // the pump's read fails and triggers redial
	}
}

func (d *daemon) setConn(c net.Conn) {
	d.mu.Lock()
	d.conn = c
	d.mu.Unlock()
}

// run dials the daemon forever, with backoff.
func (d *daemon) run() {
	backoff := time.Second
	for {
		conn, err := net.DialTimeout("unix", d.socket, 3*time.Second)
		if err != nil {
			log.Printf("gateway2: termhost dial: %v (retrying in %s)", err, backoff)
			time.Sleep(backoff)
			backoff = min(backoff*2, 5*time.Second)
			continue
		}
		backoff = time.Second
		if err := d.session(conn); err != nil {
			log.Printf("gateway2: termhost session: %v", err)
		}
		_ = conn.Close()
		d.setConn(nil)
		d.o.post(func() {
			d.o.flushPending("termhost connection lost")
			d.o.broadcast(browserproto.NewError(0, "termhost connection lost — reconnecting"))
		})
	}
}

// session runs one daemon connection: handshake, reconcile, event pump.
func (d *daemon) session(conn net.Conn) error {
	if err := orchestration.WriteMessage(conn, orchestration.NewHello()); err != nil {
		return err
	}
	mt, payload, err := orchestration.ReadMessage(conn)
	if err != nil {
		return err
	}
	if mt != orchestration.MsgWelcome {
		return fmt.Errorf("expected welcome, got %q", mt)
	}
	var w orchestration.Welcome
	if err := json.Unmarshal(payload, &w); err != nil {
		return err
	}
	if w.Error != "" {
		return errors.New("daemon rejected hello: " + w.Error)
	}
	if w.ProtocolVersion != orchestration.ProtocolVersion {
		return fmt.Errorf("daemon speaks protocol %d, want %d", w.ProtocolVersion, orchestration.ProtocolVersion)
	}

	d.setConn(conn)
	d.reconcile(w.Panes)

	for {
		mt, payload, err := orchestration.ReadMessage(conn)
		if err != nil {
			return err
		}
		d.dispatch(mt, payload)
	}
}

// reconcile syncs the daemon's surviving pane set to the model: mark survivors
// created, drop the created flag on the rest so syncDaemon respawns them, close
// daemon panes outside the model, then re-apply the model and resync the
// visible panes for any attached browser. Runs on the orchestrator loop.
func (d *daemon) reconcile(alivePanes []uint32) {
	d.o.post(func() {
		o := d.o
		alive := make(map[uint32]bool, len(alivePanes))
		for _, id := range alivePanes {
			alive[id] = true
		}
		model := make(map[uint32]bool)
		for _, id := range o.session.AllPaneIDs() {
			pid := uint32(id)
			model[pid] = true
			rt := o.panes[pid]
			if rt == nil {
				continue // syncDaemon (in applyModel) creates missing runtimes
			}
			rt.created = alive[pid]
		}
		for _, id := range alivePanes {
			if !model[id] {
				d.send(orchestration.NewClosePane(id))
			}
		}
		o.applyModel()
		for _, id := range o.session.VisiblePaneIDs() {
			o.resyncPane(uint32(id))
		}
	})
}

// dispatch translates one daemon β event into browser messages and model
// updates, posted onto the orchestrator loop. Chrome is cached on the pane
// runtime regardless of visibility (§8), but only forwarded to browsers when
// the pane is in the current viewport; the agents rollup is always global.
func (d *daemon) dispatch(mt orchestration.MessageType, payload []byte) {
	o := d.o
	switch mt {
	case orchestration.MsgPaneFrame:
		var ev orchestration.PaneFrame
		if err := json.Unmarshal(payload, &ev); err != nil || ev.Frame == nil {
			return
		}
		o.post(func() {
			if o.panes[ev.PaneID] == nil || !o.visible[ev.PaneID] {
				return
			}
			for c := range o.conns {
				msg := c.translator(ev.PaneID).Translate(ev.Frame)
				if b, err := browserproto.Marshal(msg); err == nil {
					o.enqueue(c, b)
				}
			}
		})

	case orchestration.MsgPaneModes:
		var ev orchestration.PaneModes
		if err := json.Unmarshal(payload, &ev); err != nil {
			return
		}
		o.post(func() {
			rt := o.panes[ev.PaneID]
			if rt == nil {
				return
			}
			rt.modes = inputModesFrom(ev)
			rt.enc.SetModes(rt.modes)
			if o.visible[ev.PaneID] {
				o.broadcast(browserproto.ModesFrom(ev))
			}
		})

	case orchestration.MsgPaneTitle:
		var ev orchestration.PaneTitle
		if err := json.Unmarshal(payload, &ev); err != nil {
			return
		}
		o.post(func() {
			rt := o.panes[ev.PaneID]
			if rt == nil {
				return
			}
			rt.title = ev.Title
			if o.visible[ev.PaneID] {
				o.broadcast(browserproto.NewPaneTitle(ev.PaneID, o.effectiveTitle(ev.PaneID)))
			}
		})

	case orchestration.MsgPaneCwd:
		var ev orchestration.PaneCwd
		if err := json.Unmarshal(payload, &ev); err != nil {
			return
		}
		o.post(func() {
			rt := o.panes[ev.PaneID]
			if rt == nil {
				return
			}
			rt.cwd = ev.Cwd
			if o.visible[ev.PaneID] {
				o.broadcast(browserproto.NewPaneCwd(ev.PaneID, ev.Cwd))
			}
		})

	case orchestration.MsgPaneAgent:
		var ev orchestration.PaneAgent
		if err := json.Unmarshal(payload, &ev); err != nil {
			return
		}
		o.post(func() {
			rt := o.panes[ev.PaneID]
			if rt == nil {
				return
			}
			rt.agent = &ev
			if o.visible[ev.PaneID] {
				o.broadcast(browserproto.NewPaneAgent(ev.PaneID, ev.Agent, ev.State, true))
			}
			o.broadcast(o.agentsMsg())
		})

	case orchestration.MsgPaneClipboard:
		var ev orchestration.PaneClipboard
		if err := json.Unmarshal(payload, &ev); err != nil {
			return
		}
		o.post(func() { o.broadcast(browserproto.NewClipboard(ev.Data)) })

	case orchestration.MsgPaneExited:
		var ev orchestration.PaneExited
		if err := json.Unmarshal(payload, &ev); err != nil {
			return
		}
		o.post(func() {
			rt := o.panes[ev.PaneID]
			if rt == nil {
				return
			}
			code := ev.ExitCode
			rt.exited = &code
			if o.visible[ev.PaneID] {
				o.broadcast(browserproto.NewPaneExited(ev.PaneID, ev.ExitCode))
			}
		})

	case orchestration.MsgPaneSelection:
		var ev orchestration.PaneSelection
		if err := json.Unmarshal(payload, &ev); err != nil {
			return
		}
		o.post(func() {
			o.resolvePending(reqKey{ev.PaneID, reqSelection}, browserproto.ReadResult{Text: ev.Text})
		})

	case orchestration.MsgPaneText:
		var ev orchestration.PaneText
		if err := json.Unmarshal(payload, &ev); err != nil {
			return
		}
		o.post(func() {
			o.resolvePending(reqKey{ev.PaneID, reqText}, browserproto.CaptureResult{Text: ev.Text})
		})

	case orchestration.MsgError:
		var ev orchestration.Error
		if err := json.Unmarshal(payload, &ev); err != nil {
			return
		}
		log.Printf("gateway2: daemon error (pane %d): %s", ev.PaneID, ev.Message)
		o.post(func() { o.broadcast(browserproto.NewError(ev.PaneID, ev.Message)) })
	}
}

// inputModesFrom rehydrates the β pane_modes mirror into the emulator-side
// struct the input encoder consumes.
func inputModesFrom(m orchestration.PaneModes) terminal.InputModes {
	return terminal.InputModes{
		AlternateScreen:      m.AlternateScreen,
		ApplicationCursor:    m.ApplicationCursor,
		BracketedPaste:       m.BracketedPaste,
		FocusReporting:       m.FocusReporting,
		MouseMode:            terminal.MouseMode(m.MouseMode),
		MouseEncoding:        terminal.MouseEncoding(m.MouseEncoding),
		MouseAlternateScroll: m.MouseAlternateScroll,
		SynchronizedOutput:   m.SynchronizedOutput,
		KittyKeyboardFlags:   m.KittyKeyboardFlags,
		ModifyOtherKeys:      m.ModifyOtherKeys,
	}
}
