//go:build ghostty

package main

import (
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/rohanthewiz/rweb"

	"github.com/rohanthewiz/herdr-web/internal/app"
	"github.com/rohanthewiz/herdr-web/internal/browserproto"
	"github.com/rohanthewiz/herdr-web/internal/inputenc"
	"github.com/rohanthewiz/herdr-web/internal/layout"
	"github.com/rohanthewiz/herdr-web/internal/orchestration"
	"github.com/rohanthewiz/herdr-web/internal/terminal"
	"github.com/rohanthewiz/herdr-web/internal/workspace"
)

// chromeRows is reserved at the top of every pane rect for the HTML chrome
// strip (title/cwd/agent as data); the pane's grid fills the inner rect.
const chromeRows = 1

// defaultArea is the layout area assumed until the first browser reports its
// grid via init/resize.
var defaultArea = layout.Rect{Width: 120, Height: 32}

// paneRuntime is the orchestrator's per-pane runtime state — everything about a
// pane that is NOT the domain model (which lives in app.Session): the input
// encoder, cached chrome for late-joining browsers, the desired grid, and
// whether the daemon has spawned its PTY. Keyed by pane id in orch.panes.
type paneRuntime struct {
	id     uint32
	enc    *inputenc.Encoder
	modes  terminal.InputModes
	title  string
	cwd    string
	agent  *orchestration.PaneAgent
	cols   uint16
	rows   uint16
	exited *int
	// created reports whether the daemon holds this pane's PTY. reconcile
	// resets it from the daemon's surviving-pane set on every (re)connect.
	created bool
}

// orch is the WS2 orchestrator: a single event-loop actor (run) that owns all
// mutable state — the app.Session domain model, per-pane runtimes, connected
// browsers, the layout area, and the daemon link. Producers (per-connection
// readers, the daemon pump) never touch that state directly; they post closures
// onto mailbox, which run serially in the loop goroutine. So there is no lock:
// state is touched by exactly one goroutine.
type orch struct {
	session *app.Session
	panes   map[uint32]*paneRuntime
	conns   map[*client]struct{}
	daemon  *daemon
	area    layout.Rect
	cellW   uint32
	cellH   uint32
	cwd     string
	// visible is the current viewport's pane set (active workspace's active
	// tab) — the panes whose frames stream to browsers (§8). Recomputed by
	// refreshViewport whenever the viewport changes.
	visible map[uint32]bool
	// pendingReads holds in-flight read commands awaiting the daemon's
	// pane_selection reply, FIFO per pane (read is the only round-trip command).
	pendingReads map[uint32][]*pendingRead
	mailbox      chan func()
	// stop is the process-shutdown hook wired by main (server.stop). It flushes
	// pending browser writes, then exits — the persistent termhost daemon is a
	// separate process and survives. nil in tests, where stop is a no-op.
	stop func()
}

// pendingRead is one in-flight read command. read is unique among §7 commands in
// needing a daemon round-trip: handleCmd sends a RequestSelection and returns
// without replying; the pane_selection reply (resolveRead), a timeout, or a
// daemon disconnect (flushPendingReads) resolves it. The daemon emits one
// pane_selection per request over its single ordered connection, so per-pane
// FIFO correlation is exact (the reply carries no command id).
type pendingRead struct {
	c     *client
	id    string
	timer *time.Timer
}

// readTimeout bounds how long a read waits for the daemon's pane_selection
// reply, so a browser's cmd never hangs if the daemon errors or the reply is
// lost without the connection itself dropping.
const readTimeout = 5 * time.Second

// modelSpawner satisfies workspace.PaneSpawner without touching the daemon: the
// orchestrator syncs the daemon's PTYs to the model separately (syncDaemon), so
// the model must be buildable before any daemon connection exists.
type modelSpawner struct{}

func (modelSpawner) Spawn(spec workspace.SpawnSpec) (workspace.TerminalID, error) {
	return workspace.TerminalID(fmt.Sprintf("term_%d", spec.PaneID)), nil
}
func (modelSpawner) Despawn(workspace.TerminalID) {}

// newOrch builds the orchestrator with a fresh session (one workspace, one tab,
// one pane). Splits, tabs, and workspaces are created at runtime via commands.
func newOrch(socket, cwd string) (*orch, error) {
	o := &orch{
		panes:        make(map[uint32]*paneRuntime),
		conns:        make(map[*client]struct{}),
		area:         defaultArea,
		cellW:        8,
		cellH:        16,
		cwd:          cwd,
		visible:      make(map[uint32]bool),
		pendingReads: make(map[uint32][]*pendingRead),
		mailbox:      make(chan func(), 256),
	}
	o.daemon = &daemon{o: o, socket: socket}

	sess, err := app.NewSession(modelSpawner{}, cwd)
	if err != nil {
		return nil, err
	}
	o.session = sess
	o.syncDaemon()      // desired sizes; no daemon/conns yet, sends are dropped
	o.refreshViewport() // seed the visible set
	return o, nil
}

// run is the event loop: the sole owner of orch state. Every state mutation
// happens inside a closure delivered here.
func (o *orch) run() {
	for fn := range o.mailbox {
		fn()
	}
}

// post enqueues work onto the loop. It blocks if the mailbox is momentarily
// full (backpressure); the loop is always draining, so it never deadlocks.
func (o *orch) post(fn func()) { o.mailbox <- fn }

// --- Layout / daemon reconciliation ------------------------------------------

// viewportLayout builds the browser layout message for the current viewport
// (active workspace's active tab), reserving the chrome strip in each pane's
// inner rect.
func (o *orch) viewportLayout() browserproto.Layout {
	msg := browserproto.BuildLayout(o.session.Workspaces(), o.session.ActiveIndex(), o.area)
	for i := range msg.Panes {
		cols, rows := innerGrid(msg.Panes[i].Rect)
		msg.Panes[i].Inner = browserproto.Rect{msg.Panes[i].Rect[0], msg.Panes[i].Rect[1] + chromeRows, cols, rows}
	}
	return msg
}

// innerGrid is a pane rect's terminal grid after reserving the chrome row.
func innerGrid(r browserproto.Rect) (cols, rows uint16) {
	cols, rows = r[2], r[3]
	if rows > chromeRows {
		rows -= chromeRows
	}
	return cols, rows
}

// desiredGrids computes the target grid for every pane in every tab/workspace —
// all are live PTYs on the daemon (§8), sized from their own tab's layout over
// the shared window area.
func (o *orch) desiredGrids() map[uint32][2]uint16 {
	grids := make(map[uint32][2]uint16)
	gridRows := func(h uint16) uint16 {
		if h > chromeRows {
			return h - chromeRows
		}
		return h
	}
	for _, ws := range o.session.Workspaces() {
		for _, tab := range ws.Tabs {
			for _, info := range tab.Layout.Panes(o.area) {
				grids[uint32(info.ID)] = [2]uint16{info.Rect.Width, gridRows(info.Rect.Height)}
			}
			// A zoomed tab renders its focused pane at the full area (§8, the
			// browser only sees that one), so it must be sized to fill it. The
			// hidden siblings keep their split-rect sizes above — they stay live
			// PTYs so syncDaemon won't close them, and don't stream while hidden.
			if tab.Zoomed {
				grids[uint32(tab.Layout.Focused())] = [2]uint16{o.area.Width, gridRows(o.area.Height)}
			}
		}
	}
	return grids
}

// syncDaemon reconciles the daemon's PTY set with the session: spawn panes the
// daemon lacks, resize panes whose grid changed, close panes dropped from the
// model, and drop their runtimes.
func (o *orch) syncDaemon() {
	grids := o.desiredGrids()

	for pid := range grids {
		if o.panes[pid] == nil {
			enc, err := inputenc.New()
			if err != nil {
				log.Printf("gateway2: encoder: %v", err)
				continue
			}
			o.panes[pid] = &paneRuntime{id: pid, enc: enc}
		}
	}
	for pid, rt := range o.panes {
		if _, ok := grids[pid]; !ok {
			if rt.created {
				o.daemon.send(orchestration.NewClosePane(pid))
			}
			delete(o.panes, pid)
		}
	}
	for pid, g := range grids {
		rt := o.panes[pid]
		if rt == nil {
			continue
		}
		cols, rows := g[0], g[1]
		if cols == 0 || rows == 0 {
			continue
		}
		changed := cols != rt.cols || rows != rt.rows
		if changed {
			rt.cols, rt.rows = cols, rows
			rt.enc.SetGrid(cols, rows)
		}
		switch {
		case !rt.created:
			o.createPane(rt)
		case changed:
			r := orchestration.NewResize(pid, cols, rows)
			r.CellWidthPx, r.CellHeightPx = o.cellW, o.cellH
			o.daemon.send(r)
		}
	}
}

// createPane spawns a pane's PTY at its desired grid and marks it created.
func (o *orch) createPane(rt *paneRuntime) {
	cp := orchestration.NewCreatePane(rt.id, rt.cols, rt.rows)
	cp.Cwd = o.cwd
	cp.CellWidthPx, cp.CellHeightPx = o.cellW, o.cellH
	o.daemon.send(cp)
	rt.created = true
}

// refreshViewport recomputes the visible-pane set and returns the panes that
// just became visible (a viewport change), so the loop can resend their chrome
// and full frames.
func (o *orch) refreshViewport() (added []uint32) {
	next := make(map[uint32]bool)
	for _, id := range o.session.VisiblePaneIDs() {
		pid := uint32(id)
		next[pid] = true
		if !o.visible[pid] {
			added = append(added, pid)
		}
	}
	o.visible = next
	return added
}

// applyModel is the standard follow-up after a model-mutating command: sync the
// daemon, recompute the viewport, broadcast the new layout + agents, and refresh
// any newly-visible panes (chrome + full frame).
func (o *orch) applyModel() {
	o.syncDaemon()
	added := o.refreshViewport()
	o.broadcast(o.viewportLayout())
	o.broadcast(o.agentsMsg())
	for _, pid := range added {
		o.broadcastPaneChrome(pid)
		o.resyncPane(pid)
	}
}

// resyncPane forces every connection's translator for the pane to emit a full
// frame and asks the daemon to replay one.
func (o *orch) resyncPane(pid uint32) {
	for c := range o.conns {
		if t := c.trans[pid]; t != nil {
			t.Reset()
		}
	}
	o.daemon.send(orchestration.NewRequestResync(pid))
}

// agentsMsg builds the global sidebar rollup from every pane's cached agent
// state (agent chrome is not viewport-filtered, §8).
func (o *orch) agentsMsg() browserproto.Agents {
	items := []browserproto.AgentItem{}
	for _, ws := range o.session.Workspaces() {
		for _, tab := range ws.Tabs {
			for _, id := range tab.Layout.PaneIDs() {
				rt := o.panes[uint32(id)]
				if rt == nil || rt.agent == nil || rt.agent.Agent == "" {
					continue
				}
				pub, _ := o.session.PublicPaneID(id)
				items = append(items, browserproto.AgentItem{
					Pane: rt.id, Pub: pub, Workspace: ws.ID,
					Agent: rt.agent.Agent, State: rt.agent.State, Seen: true,
				})
			}
		}
	}
	return browserproto.NewAgents(items)
}

// --- read round-trip (loop goroutine only) -----------------------------------

// startRead registers an in-flight read and asks the daemon to extract the
// selection. The reply arrives asynchronously and completes it in resolveRead;
// a timer fails it after readTimeout if no reply comes.
func (o *orch) startRead(c *client, id string, p browserproto.ReadParams) {
	pr := &pendingRead{c: c, id: id}
	pane := p.Pane
	o.pendingReads[pane] = append(o.pendingReads[pane], pr)
	pr.timer = time.AfterFunc(readTimeout, func() {
		o.post(func() { o.timeoutRead(pane, pr) })
	})
	o.daemon.send(orchestration.NewRequestSelection(pane,
		orchestration.SelectionPoint{Row: p.Anchor[0], Col: uint16(p.Anchor[1])},
		orchestration.SelectionPoint{Row: p.Cursor[0], Col: uint16(p.Cursor[1])},
		p.Rect))
}

// resolveRead completes the oldest in-flight read for a pane with the daemon's
// extracted text. Per-pane FIFO: the daemon replies to requests in order over
// its single connection, and pane_selection carries no command id.
func (o *orch) resolveRead(pane uint32, text string) {
	q := o.pendingReads[pane]
	if len(q) == 0 {
		return
	}
	pr := q[0]
	o.dropPending(pane, 0)
	if pr.timer != nil {
		pr.timer.Stop()
	}
	o.replyRead(pr, text, "")
}

// timeoutRead fails a still-pending read after readTimeout. It removes the read
// by identity, not position, since a late reply may have shifted the queue.
func (o *orch) timeoutRead(pane uint32, pr *pendingRead) {
	for i, e := range o.pendingReads[pane] {
		if e == pr {
			o.dropPending(pane, i)
			o.replyRead(pr, "", "read timed out")
			return
		}
	}
}

// flushPendingReads fails every in-flight read (the daemon connection dropped,
// so no pane_selection will arrive).
func (o *orch) flushPendingReads(errMsg string) {
	for pane, q := range o.pendingReads {
		for _, pr := range q {
			if pr.timer != nil {
				pr.timer.Stop()
			}
			o.replyRead(pr, "", errMsg)
		}
		delete(o.pendingReads, pane)
	}
}

// dropPending removes the read at index i from a pane's FIFO queue.
func (o *orch) dropPending(pane uint32, i int) {
	q := append(o.pendingReads[pane][:i], o.pendingReads[pane][i+1:]...)
	if len(q) == 0 {
		delete(o.pendingReads, pane)
	} else {
		o.pendingReads[pane] = q
	}
}

// replyRead sends a read's cmd_result — the extracted text on success, or an
// error. A read with no id has no result channel and is skipped.
func (o *orch) replyRead(pr *pendingRead, text, errMsg string) {
	if pr.id == "" {
		return
	}
	var data any
	if errMsg == "" {
		data = browserproto.ReadResult{Text: text}
	}
	if r, err := browserproto.NewCmdResult(pr.id, errMsg == "", errMsg, data); err == nil {
		o.send(pr.c, r)
	}
}

// --- Broadcasting ------------------------------------------------------------

func (o *orch) broadcast(m any) {
	b, err := browserproto.Marshal(m)
	if err != nil {
		log.Printf("gateway2: marshal broadcast: %v", err)
		return
	}
	for c := range o.conns {
		o.enqueue(c, b)
	}
}

func (o *orch) send(c *client, m any) {
	b, err := browserproto.Marshal(m)
	if err != nil {
		log.Printf("gateway2: marshal: %v", err)
		return
	}
	o.enqueue(c, b)
}

// effectiveTitle is what the browser should show for a pane: the user's custom
// name (pane.rename) when set, otherwise the terminal-reported title cached on
// the runtime.
func (o *orch) effectiveTitle(pid uint32) string {
	if name, ok := o.session.PaneCustomName(layout.PaneID(pid)); ok && name != "" {
		return name
	}
	if rt := o.panes[pid]; rt != nil {
		return rt.title
	}
	return ""
}

// broadcastPaneChrome resends a pane's cached chrome to all connections (used
// when a pane becomes visible after a viewport switch).
func (o *orch) broadcastPaneChrome(pid uint32) {
	rt := o.panes[pid]
	if rt == nil {
		return
	}
	o.broadcast(browserproto.PaneModes{T: browserproto.MsgPaneModes, Pane: pid,
		Mouse: rt.modes.MouseMode != terminal.MouseNone, AltScreen: rt.modes.AlternateScreen})
	if t := o.effectiveTitle(pid); t != "" {
		o.broadcast(browserproto.NewPaneTitle(pid, t))
	}
	if rt.cwd != "" {
		o.broadcast(browserproto.NewPaneCwd(pid, rt.cwd))
	}
	if rt.agent != nil {
		o.broadcast(browserproto.NewPaneAgent(pid, rt.agent.Agent, rt.agent.State, true))
	}
	if rt.exited != nil {
		o.broadcast(browserproto.NewPaneExited(pid, *rt.exited))
	}
}

// enqueue delivers bytes to a connection's writer, dropping the connection if
// it can't keep up. Loop-goroutine only.
func (o *orch) enqueue(c *client, b []byte) {
	if _, ok := o.conns[c]; !ok {
		return
	}
	select {
	case c.out <- b:
	default:
		log.Printf("gateway2: dropping slow browser connection")
		o.dropConn(c)
	}
}

// dropConn removes a connection and closes its writer queue. Idempotent and
// loop-goroutine only, so the queue is closed exactly once.
func (o *orch) dropConn(c *client) {
	if _, ok := o.conns[c]; !ok {
		return
	}
	delete(o.conns, c)
	close(c.out)
}

// --- Browser connections -----------------------------------------------------

// client is one connected browser. The writer goroutine is the only WSConn
// writer; trans (per-pane frame translators) is touched only in the loop.
type client struct {
	o     *orch
	ws    *rweb.WSConn
	out   chan []byte
	trans map[uint32]*browserproto.FrameTranslator
}

func (c *client) translator(pid uint32) *browserproto.FrameTranslator {
	t := c.trans[pid]
	if t == nil {
		t = browserproto.NewFrameTranslator(pid)
		c.trans[pid] = t
	}
	return t
}

func (c *client) writer() {
	for b := range c.out {
		if err := c.ws.WriteMessage(rweb.TextMessage, b); err != nil {
			c.o.post(func() { c.o.dropConn(c) })
			return
		}
	}
	_ = c.ws.Close(1000, "bye")
}

// serve runs one browser session: the init handshake (synchronous), then the
// up-message read loop that posts each message onto the orchestrator loop.
func (o *orch) serve(ws *rweb.WSConn) error {
	defer ws.Close(1000, "bye")

	first, err := ws.ReadMessage()
	if err != nil {
		return nil
	}
	up, err := browserproto.DecodeUp(first.Data)
	init, ok := up.(*browserproto.Init)
	if err != nil || !ok {
		b, _ := browserproto.Marshal(browserproto.NewWelcome("first message must be init"))
		_ = ws.WriteMessage(rweb.TextMessage, b)
		return nil
	}
	if init.V != browserproto.ProtocolVersion {
		b, _ := browserproto.Marshal(browserproto.NewWelcome(
			fmt.Sprintf("protocol version %d unsupported (server speaks %d)", init.V, browserproto.ProtocolVersion)))
		_ = ws.WriteMessage(rweb.TextMessage, b)
		return nil
	}

	c := &client{o: o, ws: ws, out: make(chan []byte, 512),
		trans: make(map[uint32]*browserproto.FrameTranslator)}
	go c.writer()
	o.post(func() { o.registerConn(c, init) })

	for {
		m, err := ws.ReadMessage()
		if err != nil {
			break
		}
		if m.Type != rweb.TextMessage {
			continue
		}
		up, err := browserproto.DecodeUp(m.Data)
		if err != nil {
			if !errors.Is(err, browserproto.ErrUnknownType) {
				log.Printf("gateway2: bad up message: %v", err)
			}
			continue // spec §1: unknown types are dropped
		}
		o.post(func() { o.handleUp(c, up) })
	}
	o.post(func() { o.dropConn(c) })
	return nil
}

// registerConn adds a connection, applies its reported grid, and pushes the
// initial viewport state (welcome, layout, cached chrome, agents) plus a full
// frame per visible pane. Loop-goroutine only.
func (o *orch) registerConn(c *client, init *browserproto.Init) {
	o.conns[c] = struct{}{}
	if init.Cols > 0 && init.Rows > 0 {
		o.area = layout.Rect{Width: init.Cols, Height: init.Rows}
	}
	if init.CellWPx > 0 && init.CellHPx > 0 {
		o.cellW, o.cellH = init.CellWPx, init.CellHPx
	}
	o.syncDaemon() // the new grid may resize panes
	o.refreshViewport()

	o.send(c, browserproto.NewWelcome(""))
	o.send(c, o.viewportLayout())
	for _, id := range o.session.VisiblePaneIDs() {
		pid := uint32(id)
		rt := o.panes[pid]
		if rt == nil {
			continue
		}
		o.send(c, browserproto.PaneModes{T: browserproto.MsgPaneModes, Pane: pid,
			Mouse: rt.modes.MouseMode != terminal.MouseNone, AltScreen: rt.modes.AlternateScreen})
		if rt.title != "" {
			o.send(c, browserproto.NewPaneTitle(pid, rt.title))
		}
		if rt.cwd != "" {
			o.send(c, browserproto.NewPaneCwd(pid, rt.cwd))
		}
		if rt.agent != nil {
			o.send(c, browserproto.NewPaneAgent(pid, rt.agent.Agent, rt.agent.State, true))
		}
		if rt.exited != nil {
			o.send(c, browserproto.NewPaneExited(pid, *rt.exited))
		}
		c.translator(pid).Reset()
		o.daemon.send(orchestration.NewRequestResync(pid))
	}
	o.send(c, o.agentsMsg())
	if !o.daemon.connected() {
		o.send(c, browserproto.NewError(0, "termhost daemon not connected — retrying"))
	}
}

// --- Up-message handling (loop goroutine) ------------------------------------

func (o *orch) handleUp(c *client, up any) {
	switch m := up.(type) {
	case *browserproto.Key:
		id, ok := o.session.FocusedPane()
		rt := o.panes[uint32(id)]
		if !ok || rt == nil || rt.exited != nil {
			return
		}
		if b, err := rt.enc.Key(*m); err != nil {
			log.Printf("gateway2: key encode: %v", err)
		} else if len(b) > 0 {
			o.daemon.send(orchestration.NewInput(rt.id, b))
		}

	case *browserproto.Mouse:
		rt := o.panes[m.Pane]
		if rt == nil || rt.exited != nil || !o.visible[m.Pane] {
			return
		}
		b, err := rt.enc.Mouse(*m)
		if err != nil {
			log.Printf("gateway2: mouse encode: %v", err)
			return
		}
		switch {
		case len(b) > 0:
			o.daemon.send(orchestration.NewInput(rt.id, b))
		case m.Kind == browserproto.MouseWheel && m.DY != 0:
			o.daemon.send(orchestration.NewScrollViewport(rt.id, int32(m.DY)))
		}

	case *browserproto.Paste:
		id, ok := o.session.FocusedPane()
		rt := o.panes[uint32(id)]
		if !ok || rt == nil || rt.exited != nil {
			return
		}
		if b, err := rt.enc.Paste(m.Data); err != nil {
			log.Printf("gateway2: paste encode: %v", err)
		} else if len(b) > 0 {
			o.daemon.send(orchestration.NewInput(rt.id, b))
		}

	case *browserproto.Raw:
		id, ok := o.session.FocusedPane()
		if ok && len(m.Data) > 0 {
			o.daemon.send(orchestration.NewInput(uint32(id), m.Data))
		}

	case *browserproto.Resize:
		if m.Cols == 0 || m.Rows == 0 {
			return
		}
		o.area = layout.Rect{Width: m.Cols, Height: m.Rows}
		o.applyModel()

	case *browserproto.Image:
		o.send(c, browserproto.NewError(0, "image paste is not supported by the gateway2 spike"))

	case *browserproto.Cmd:
		o.handleCmd(c, m)
	}
}
