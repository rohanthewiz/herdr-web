//go:build ghostty

package main

import (
	"errors"
	"fmt"
	"log"
	"sync/atomic"
	"time"

	"github.com/rohanthewiz/rweb"

	"github.com/rohanthewiz/herdr-web/internal/app"
	"github.com/rohanthewiz/herdr-web/internal/browserproto"
	"github.com/rohanthewiz/herdr-web/internal/config"
	"github.com/rohanthewiz/herdr-web/internal/inputenc"
	"github.com/rohanthewiz/herdr-web/internal/layout"
	"github.com/rohanthewiz/herdr-web/internal/orchestration"
	"github.com/rohanthewiz/herdr-web/internal/persist"
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
	// --- hook-report ingestion (hooks.go), all loop-goroutine only ---
	// agentAt stamps when the daemon's detection last reported (hook-vs-detection
	// recency in effectiveAgent). hook is the live hook authority; agentSession
	// the resumable session identity; hookSeqs/hookSuppressed the per-source
	// idempotency and release-suppression state. pubAgent/pubState are the last
	// *published* arbitrated pair — the transition baseline for notifications.
	agentAt        time.Time
	hook           *hookAuthority
	agentSession   *agentSessionRef
	hookSeqs       map[string]uint64
	hookSuppressed map[string]hookSuppression
	pubAgent       string
	pubState       string
	// created reports whether the daemon holds this pane's PTY. reconcile
	// resets it from the daemon's surviving-pane set on every (re)connect.
	created bool
	// histDirty marks output since the last history capture (WS3): the periodic
	// sweep only captures panes whose content actually changed. Set on every
	// pane_frame, cleared when a capture is issued.
	histDirty bool
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
	// pendingReqs holds in-flight daemon round-trip commands (read, capture)
	// awaiting their reply, FIFO per (pane, kind). Both replies carry no command
	// id, so the kind picks the queue and per-pane order does the correlation.
	pendingReqs map[reqKey][]*pending
	// waiters holds active pane.wait_for_output waiters per pane; each matches the
	// pane's live output stream (plus a one-shot seed of the current screen) and
	// resolves on a match, its own timeout, or the pane exiting. waiterCheck marks
	// the seed capture-check in flight. outAccum is the per-pane rolling cleaned-text
	// buffer fed by the β pane_output stream (enabled via set_output_stream while any
	// waiter is active); it exists only for panes with a live waiter.
	waiters     map[uint32][]*waiter
	waiterCheck map[uint32]bool
	outAccum    map[uint32]*outputScanner
	// subs holds control-API event subscribers (events.subscribe); emitEvent fans
	// a pane event out to the matching ones and drops any that can't keep up.
	subs map[*ctlSubscriber]struct{}
	// structPanes (pane id → public handle) and structFocus snapshot the model's
	// pane set and globally-focused pane at the last emit; emitStructuralEvents
	// diffs against them after each mutation to derive pane_added / pane_removed /
	// focus_changed. Seeded in newOrch so pre-existing panes never emit retroactively.
	structPanes map[uint32]string
	structFocus uint32
	// lastTitle is the app-level browser-tab title last broadcast (WS8):
	// broadcastTitle dedupes against it so focus/title churn doesn't spam every
	// connection with identical title messages.
	lastTitle string
	mailbox   chan func()
	// stop is the process-shutdown hook wired by main (server.stop). It flushes
	// pending browser writes, then exits — the persistent termhost daemon is a
	// separate process and survives. nil in tests, where stop is a no-op.
	stop func()
	// hookSocket is where the hook-report API listens (hooks.go); createPane
	// injects it into every pane's environment so installed agent hooks can
	// dial back. Wired by main before the loop starts; "" disables injection.
	hookSocket string
	// baseHTML is the un-injected served page; cfgPath is the config file to
	// re-read on server.reload_config; page holds the config-injected page the
	// HTTP handler serves. The handler (rweb goroutine) and ReloadConfig (loop
	// goroutine) both touch page, so it is an atomic pointer. All three are wired
	// by main after construction; nil/"" in tests.
	baseHTML []byte
	cfgPath  string
	page     atomic.Pointer[[]byte]
	// cfg is the loaded config-file state (defaults + file, not flag overrides —
	// config.set marshals it back to disk, so flag values must never leak in).
	// worktreeDir is the tilde-expanded worktrees root new checkouts land under.
	// Both wired by main; loop-goroutine only after the loop starts.
	cfg         config.Config
	worktreeDir string
	// --- session persistence (WS3), wired by main; zero values disable it ---
	// sessionPath/historyPath are the state files ("" ⇒ persistence off). seeds
	// and restoredCwds are loaded at startup and consumed by createPane for
	// panes the daemon no longer holds (cold start): the seed replays saved
	// scrollback via create_pane.initial_history, the cwd re-spawns the shell
	// where it was. capturedHist accumulates the latest ANSI capture per pane —
	// initialized from the loaded seeds so a partial capture sweep never wipes
	// another pane's seed from disk. saveArmed/histArmed debounce the writes.
	// histLines bounds each capture (0 = whole buffer). All loop-goroutine only.
	// restoredAgents and resumePlans carry the persisted agent-session refs
	// across a restart (resume.go): restoredAgents holds each pane's loaded
	// ref until the pane goes live (adopted by reconcile or re-spawned by
	// createPane) and is merged under live refs at save time; resumePlans is
	// the argv to exec instead of a shell for a cold-started pane, consumed
	// exactly once like seeds/restoredCwds.
	sessionPath    string
	historyPath    string
	seeds          map[uint32]string
	restoredCwds   map[uint32]string
	restoredAgents map[uint32]persist.AgentSession
	resumePlans    map[uint32][]string
	capturedHist   map[uint32]string
	histLines      uint32
	saveArmed      bool
	histArmed      bool
	// finalCap tracks the clean-shutdown capture sweep (nil when not shutting
	// down): Shutdown captures every live pane's scrollback, bounded by a short
	// deadline, before firing the stop hook.
	finalCap *finalCapture
}

// reqKind distinguishes the two §7 commands that need a daemon round-trip: read
// (RequestSelection → pane_selection) and capture (RequestText → pane_text). A
// pane may have both in flight at once, and the daemon's replies carry no command
// id, so the reply's message type — not just per-pane order — picks the queue.
type reqKind uint8

const (
	reqSelection reqKind = iota // read → pane_selection
	reqText                     // capture → pane_text
)

// label names the command for user-facing errors ("<label> timed out").
func (k reqKind) label() string {
	if k == reqText {
		return "capture"
	}
	return "read"
}

// reqKey identifies a per-pane FIFO queue of in-flight requests of one kind.
type reqKey struct {
	pane uint32
	kind reqKind
}

// pending is one in-flight daemon round-trip (read or capture). The dispatch
// sends the β request and returns without replying; the matching daemon reply
// (resolvePending), a timeout, or a daemon disconnect (flushPending) resolves the
// caller's Responder. The daemon emits one reply per request over its single
// ordered connection, so per-(pane, kind) FIFO correlation is exact (the reply
// carries no command id).
type pending struct {
	resp  app.Responder
	timer *time.Timer
}

// reqTimeout bounds how long a read/capture waits for the daemon's reply, so a
// browser's cmd never hangs if the daemon errors or the reply is lost without the
// connection itself dropping.
const reqTimeout = 5 * time.Second

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
	sess, err := app.NewSession(modelSpawner{}, cwd)
	if err != nil {
		return nil, err
	}
	return newOrchWith(socket, cwd, sess), nil
}

// newOrchWith builds the orchestrator around an existing session — fresh
// (newOrch) or restored from a snapshot (WS3; main falls back to fresh when
// there is nothing to restore).
func newOrchWith(socket, cwd string, sess *app.Session) *orch {
	o := &orch{
		session:        sess,
		panes:          make(map[uint32]*paneRuntime),
		conns:          make(map[*client]struct{}),
		area:           defaultArea,
		cellW:          8,
		cellH:          16,
		cwd:            cwd,
		visible:        make(map[uint32]bool),
		pendingReqs:    make(map[reqKey][]*pending),
		waiters:        make(map[uint32][]*waiter),
		waiterCheck:    make(map[uint32]bool),
		outAccum:       make(map[uint32]*outputScanner),
		subs:           make(map[*ctlSubscriber]struct{}),
		seeds:          make(map[uint32]string),
		restoredCwds:   make(map[uint32]string),
		restoredAgents: make(map[uint32]persist.AgentSession),
		resumePlans:    make(map[uint32][]string),
		capturedHist:   make(map[uint32]string),
		mailbox:        make(chan func(), 256),
	}
	o.daemon = &daemon{o: o, socket: socket}
	o.syncDaemon()      // desired sizes; no daemon/conns yet, sends are dropped
	o.refreshViewport() // seed the visible set
	o.seedStructure()   // snapshot the initial pane set/focus (no retroactive events)
	return o
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
				log.Printf("gateway: encoder: %v", err)
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

// createPane spawns a pane's PTY at its desired grid and marks it created. A
// restored pane the daemon no longer holds (cold start) is re-spawned in its
// saved cwd with its saved scrollback replayed via initial_history (WS3) — or,
// when a resume plan exists (resume.go), with the agent's native resume argv
// as the pane command instead of a shell. Everything restored is consumed only
// on a connected send — a pre-connection create is dropped by daemon.send and
// retried by reconcile, which must still find it.
func (o *orch) createPane(rt *paneRuntime) {
	cp := orchestration.NewCreatePane(rt.id, rt.cols, rt.rows)
	cp.Cwd = o.paneCwd(rt.id)
	cp.CellWidthPx, cp.CellHeightPx = o.cellW, o.cellH
	// Arm the integration hooks: every pane learns the hook-report socket and
	// its own public handle (WS7's herdrctl installers plant hooks that read
	// exactly these variables).
	pub, _ := o.session.PublicPaneID(layout.PaneID(rt.id))
	cp.Env = paneEnvMap(o.hookSocket, rt.id, pub)
	if o.daemon.connected() {
		if cwd, ok := o.restoredCwds[rt.id]; ok {
			cp.Cwd = cwd
			delete(o.restoredCwds, rt.id)
		}
		if argv, ok := o.resumePlans[rt.id]; ok {
			cp.Command, cp.Args = argv[0], argv[1:]
			delete(o.resumePlans, rt.id)
			// The resuming pane keeps its ref live so the next snapshot still
			// carries it (herdr's set_persisted_agent_session on restore).
			if s, ok := o.restoredAgents[rt.id]; ok {
				rt.agentSession = &agentSessionRef{source: s.Source, agent: s.Agent, kind: s.Kind, value: s.Value}
				delete(o.restoredAgents, rt.id)
			}
		}
		if h, ok := o.seeds[rt.id]; ok {
			cp.InitialHistory = h
			delete(o.seeds, rt.id)
		}
	}
	o.daemon.send(cp)
	rt.created = true
}

// paneCwd is the directory a pane's PTY spawns in: its owning workspace's
// identity cwd (set for worktree-checkout workspaces) when present, else the
// process cwd. A restored pane's saved cwd still overrides this in createPane.
func (o *orch) paneCwd(pid uint32) string {
	if ws := o.session.PaneWorkspace(layout.PaneID(pid)); ws != nil && ws.IdentityCwd != "" {
		return ws.IdentityCwd
	}
	return o.cwd
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
// daemon, recompute the viewport, broadcast the new layout + agents, refresh any
// newly-visible panes (chrome + full frame), emit any structural events, and
// arm the debounced session save.
func (o *orch) applyModel() {
	o.syncDaemon()
	added := o.refreshViewport()
	o.broadcast(o.viewportLayout())
	o.broadcast(o.agentsMsg())
	for _, pid := range added {
		o.broadcastPaneChrome(pid)
		o.resyncPane(pid)
	}
	o.emitStructuralEvents()
	o.broadcastTitle()
	o.saveSoon()
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
				if rt == nil {
					continue
				}
				agent, state := rt.effectiveAgent()
				if agent == "" {
					continue
				}
				pub, _ := o.session.PublicPaneID(id)
				items = append(items, browserproto.AgentItem{
					Pane: rt.id, Pub: pub, Workspace: ws.ID,
					Agent: agent, State: state, Seen: true,
				})
			}
		}
	}
	return browserproto.NewAgents(items)
}

// --- daemon round-trips: read + capture (loop goroutine only) ----------------
//
// read and capture are the only §7 commands that need a daemon round-trip: the
// dispatch sends a β request and returns without replying, then the daemon's
// reply (or a timeout / disconnect) resolves the browser's cmd_result later.
// registerPending / resolvePending / timeoutPending / flushPending are shared;
// only the request shape (startRead vs startCapture) and the reply data type
// differ per command.

// StartRead registers an in-flight read (app.Backend) and asks the daemon to
// extract the selection. The pane_selection reply completes r in resolvePending.
func (o *orch) StartRead(r app.Responder, p app.ReadParams) {
	o.registerPending(r, reqKey{p.Pane, reqSelection})
	o.daemon.send(orchestration.NewRequestSelection(p.Pane,
		orchestration.SelectionPoint{Row: p.Anchor[0], Col: uint16(p.Anchor[1])},
		orchestration.SelectionPoint{Row: p.Cursor[0], Col: uint16(p.Cursor[1])},
		p.Rect))
}

// StartCapture registers an in-flight capture (app.Backend) and asks the daemon
// to extract the pane's buffer text. The pane_text reply completes r.
func (o *orch) StartCapture(r app.Responder, p app.CaptureParams) {
	o.registerPending(r, reqKey{p.Pane, reqText})
	o.daemon.send(orchestration.NewRequestText(p.Pane, p.Scope, p.Lines, p.Ansi, p.Unwrap))
}

// registerPending enqueues an in-flight request under key and arms its timeout.
// The caller sends the β request separately (the request shape is per-command).
func (o *orch) registerPending(resp app.Responder, key reqKey) {
	pr := &pending{resp: resp}
	o.pendingReqs[key] = append(o.pendingReqs[key], pr)
	pr.timer = time.AfterFunc(reqTimeout, func() {
		o.post(func() { o.timeoutPending(key, pr) })
	})
}

// resolvePending completes the oldest in-flight request for key with the daemon's
// reply data. Per-(pane, kind) FIFO: the daemon replies to requests in order over
// its single connection, and the reply carries no command id.
func (o *orch) resolvePending(key reqKey, data any) {
	q := o.pendingReqs[key]
	if len(q) == 0 {
		return
	}
	pr := q[0]
	o.dropPending(key, 0)
	if pr.timer != nil {
		pr.timer.Stop()
	}
	o.replyPending(pr, data, "")
}

// timeoutPending fails a still-pending request after reqTimeout. It removes the
// request by identity, not position, since a late reply may have shifted the queue.
func (o *orch) timeoutPending(key reqKey, pr *pending) {
	for i, e := range o.pendingReqs[key] {
		if e == pr {
			o.dropPending(key, i)
			o.replyPending(pr, nil, key.kind.label()+" timed out")
			return
		}
	}
}

// flushPending fails every in-flight request (the daemon connection dropped, so
// no reply will arrive).
func (o *orch) flushPending(errMsg string) {
	for key, q := range o.pendingReqs {
		for _, pr := range q {
			if pr.timer != nil {
				pr.timer.Stop()
			}
			o.replyPending(pr, nil, errMsg)
		}
		delete(o.pendingReqs, key)
	}
}

// dropPending removes the request at index i from a (pane, kind) FIFO queue.
func (o *orch) dropPending(key reqKey, i int) {
	q := append(o.pendingReqs[key][:i], o.pendingReqs[key][i+1:]...)
	if len(q) == 0 {
		delete(o.pendingReqs, key)
	} else {
		o.pendingReqs[key] = q
	}
}

// replyPending completes a pending request through its Responder — the reply
// data on success, or an error. The Responder skips a caller with no reply
// channel (e.g. a browser cmd with no id).
func (o *orch) replyPending(pr *pending, data any, errMsg string) {
	if errMsg != "" {
		pr.resp.Fail(errMsg)
		return
	}
	pr.resp.OK(data)
}

// --- pane.wait_for_output waiters (loop goroutine only) ----------------------
//
// wait_for_output rides the unary envelope but resolves only when the pane's
// output matches. Registering the first waiter for a pane turns on the daemon's
// raw-output stream (set_output_stream); each β pane_output chunk is stripped to
// plain text into a per-pane rolling buffer (outAccum) and matched against the
// pane's waiters (onPaneOutput). Because it is the *byte* stream, it catches
// fast-scrolling transient output the diffed frames coalesce away and the child's
// final pre-exit output. A one-shot capture-check at registration seeds the match
// with output already on screen (onWaiterText). A match resolves the caller
// Matched:true; the wait's own timer or the pane exiting resolves Matched:false; a
// daemon drop fails it. Removing the last waiter turns the stream back off.

// waiter is one in-flight pane.wait_for_output. match runs over the pane's cleaned
// output text, returning the matched line (for the result's context) and whether
// the pattern is present. done guards a single resolution.
type waiter struct {
	resp  app.Responder
	match func(text string) (line string, ok bool)
	lines uint32
	timer *time.Timer
	done  bool
}

// StartWaitForOutput registers a waiter (app.Backend). Registering the first
// waiter for a pane turns on the daemon's raw-output stream and starts a fresh
// accumulator, so all subsequent output is matched byte-for-byte; a one-shot
// capture-check then seeds the match with output already on screen. The dispatcher
// has validated the pattern and gated pane/daemon, so Matcher can't fail here
// (re-derived defensively).
func (o *orch) StartWaitForOutput(r app.Responder, p app.WaitForOutputParams) {
	match, err := p.Matcher()
	if err != nil {
		r.Fail(err.Error())
		return
	}
	first := len(o.waiters[p.Pane]) == 0
	w := &waiter{resp: r, match: match, lines: p.Lines}
	o.waiters[p.Pane] = append(o.waiters[p.Pane], w)
	w.timer = time.AfterFunc(app.WaitTimeout(p.TimeoutMs), func() {
		o.post(func() { o.finishWaiter(p.Pane, w, false, "") })
	})
	if first {
		o.outAccum[p.Pane] = &outputScanner{}
		o.sendStreamSub(p.Pane, true) // enable the raw stream *before* the seed
	}
	o.triggerWaiterCheck(p.Pane) // seed with output already on screen
}

// sendStreamSub toggles the daemon's raw pane_output stream for a pane. A nil or
// disconnected daemon drops the send (there is no stream to toggle); an enable is
// only issued while connected — the dispatcher gates on DaemonConnected before a
// waiter registers — and a disable on the last waiter is a best-effort cleanup.
func (o *orch) sendStreamSub(pane uint32, enabled bool) {
	if o.daemon == nil {
		return
	}
	o.daemon.send(orchestration.NewSetOutputStream(pane, enabled))
}

// triggerWaiterCheck issues one capture-check for a pane's active waiters unless
// one is already in flight. The pane_text reply lands on waiterResponder, which
// matches it against each waiter; the next frame re-triggers if any remain.
func (o *orch) triggerWaiterCheck(pane uint32) {
	if len(o.waiters[pane]) == 0 || o.waiterCheck[pane] {
		return
	}
	if o.daemon == nil || !o.daemon.connected() {
		return // nothing to capture from; a reconnect's frames re-trigger
	}
	o.waiterCheck[pane] = true
	o.registerPending(waiterResponder{o: o, pane: pane}, reqKey{pane, reqText})
	o.daemon.send(orchestration.NewRequestText(pane, uint8(terminal.TextRecent), o.waiterScanLines(pane), false, false))
}

// waiterScanLines is how many recent rows a capture-check reads: 0 (the whole
// buffer) if any waiter wants it, else the largest requested bound.
func (o *orch) waiterScanLines(pane uint32) uint32 {
	var max uint32
	for _, w := range o.waiters[pane] {
		if w.lines == 0 {
			return 0
		}
		if w.lines > max {
			max = w.lines
		}
	}
	return max
}

// waiterResponder is the app.Responder for a waiter capture-check: the pane_text
// reply (resolvePending) lands on OK and is matched against the pane's waiters; a
// failed capture (timeout / no such pane) just clears the in-flight flag so the
// next frame retries. It delivers no result to a client itself.
type waiterResponder struct {
	o    *orch
	pane uint32
}

func (waiterResponder) WantsReply() bool { return true }
func (r waiterResponder) OK(data any)    { r.o.onWaiterText(r.pane, data) }
func (r waiterResponder) Fail(string)    { r.o.waiterCheck[r.pane] = false }

// onWaiterText matches the one-shot seed capture-check (output already on screen)
// against the pane's waiters and clears the in-flight flag.
func (o *orch) onWaiterText(pane uint32, data any) {
	o.waiterCheck[pane] = false
	if cr, ok := data.(browserproto.CaptureResult); ok {
		o.matchWaiters(pane, cr.Text)
	}
}

// onPaneOutput feeds a raw β pane_output chunk into the pane's accumulator and
// matches the resulting cleaned text against its waiters. A chunk that arrives
// after the last waiter resolved (outAccum already dropped) is ignored — the
// daemon's set_output_stream(false) races the tail of the stream.
func (o *orch) onPaneOutput(pane uint32, data []byte) {
	sc := o.outAccum[pane]
	if sc == nil {
		return
	}
	o.matchWaiters(pane, sc.feed(data))
}

// matchWaiters resolves every still-pending waiter for a pane whose pattern now
// appears in text (Matched:true, with the matched line). Iterates a copy because
// finishWaiter mutates the pane's waiter slice.
func (o *orch) matchWaiters(pane uint32, text string) {
	for _, w := range append([]*waiter(nil), o.waiters[pane]...) {
		if w.done {
			continue
		}
		if line, ok := w.match(text); ok {
			o.finishWaiter(pane, w, true, line)
		}
	}
}

// finishWaiter resolves a waiter once — match (Matched:true), or timeout / pane
// exit (Matched:false) — and removes it from the pane's list. Idempotent via
// w.done, so a match racing the timeout resolves exactly once.
func (o *orch) finishWaiter(pane uint32, w *waiter, matched bool, line string) {
	if w.done {
		return
	}
	w.done = true
	if w.timer != nil {
		w.timer.Stop()
	}
	o.removeWaiter(pane, w)
	w.resp.OK(app.WaitForOutputResult{Matched: matched, Text: line})
}

// removeWaiter drops w from the pane's waiter list. When the last one goes it
// tears down the pane's waiter state and turns the raw-output stream back off, so
// a pane with no waiter stops paying the stream cost.
func (o *orch) removeWaiter(pane uint32, w *waiter) {
	q := o.waiters[pane]
	for i, e := range q {
		if e == w {
			q = append(q[:i], q[i+1:]...)
			break
		}
	}
	if len(q) == 0 {
		delete(o.waiters, pane)
		delete(o.waiterCheck, pane)
		delete(o.outAccum, pane)
		o.sendStreamSub(pane, false)
	} else {
		o.waiters[pane] = q
	}
}

// resolveWaitersOnExit fails a pane's remaining waiters when it exits: no more
// output will come, so an unmatched pattern won't appear. Output that arrived only
// in the final frame (which the post-exit capture can't reach) is the accepted edge.
func (o *orch) resolveWaitersOnExit(pane uint32) {
	for _, w := range append([]*waiter(nil), o.waiters[pane]...) {
		o.finishWaiter(pane, w, false, "")
	}
}

// flushWaiters fails every active waiter when the daemon connection drops — no
// capture can resolve, so a wait can't complete. Mirrors flushPending.
func (o *orch) flushWaiters(errMsg string) {
	for pane, q := range o.waiters {
		for _, w := range q {
			if w.done {
				continue
			}
			w.done = true
			if w.timer != nil {
				w.timer.Stop()
			}
			w.resp.Fail(errMsg)
		}
		delete(o.waiters, pane)
		delete(o.waiterCheck, pane)
		delete(o.outAccum, pane)
	}
}

// --- control-API event subscribers (loop goroutine only) ---------------------

// emitEvent fans a pane event out to every control-API subscriber whose filter
// accepts it, dropping any sink that can't keep up (a slow/dead reader).
func (o *orch) emitEvent(name string, pane uint32, data any) {
	for s := range o.subs {
		if !s.filter.Match(name, pane) {
			continue
		}
		if !s.sub.Send(name, data) {
			delete(o.subs, s)
		}
	}
}

// seedStructure records the current pane set + focused pane without emitting, so
// the first emitStructuralEvents diff reports only real changes — a subscriber
// that connects later never gets a retroactive pane_added for a pane that already
// existed. Called once at construction.
func (o *orch) seedStructure() {
	o.structPanes = make(map[uint32]string)
	for _, id := range o.session.AllPaneIDs() {
		h, _ := o.session.PublicPaneID(id)
		o.structPanes[uint32(id)] = h
	}
	o.structFocus = o.focusedPaneID()
}

// focusedPaneID is the globally-focused pane's internal id (0 if none).
func (o *orch) focusedPaneID() uint32 {
	if id, ok := o.session.FocusedPane(); ok {
		return uint32(id)
	}
	return 0
}

// emitStructuralEvents diffs the model's pane set + focused pane against the last
// snapshot and emits pane_added / pane_removed / focus_changed for any change,
// then updates the snapshot. Called at the end of every model mutation (applyModel,
// BroadcastLayout); a no-op when nothing structural changed (a browser resize, a
// rename, a re-focus of the same pane). Loop-goroutine only. The snapshot is kept
// current even with no subscribers, so a later subscriber diffs from a live base.
func (o *orch) emitStructuralEvents() {
	cur := make(map[uint32]string, len(o.structPanes))
	for _, id := range o.session.AllPaneIDs() {
		pid := uint32(id)
		h, _ := o.session.PublicPaneID(id)
		cur[pid] = h
		if _, existed := o.structPanes[pid]; !existed {
			o.emitEvent(app.EventPaneAdded, pid, app.PaneRefEvent{Pane: pid, Handle: h})
		}
	}
	for pid, h := range o.structPanes {
		if _, still := cur[pid]; !still {
			o.emitEvent(app.EventPaneRemoved, pid, app.PaneRefEvent{Pane: pid, Handle: h})
		}
	}
	o.structPanes = cur

	if focus := o.focusedPaneID(); focus != o.structFocus {
		o.structFocus = focus
		h, _ := o.session.PublicPaneID(layout.PaneID(focus))
		o.emitEvent(app.EventFocusChanged, focus, app.PaneRefEvent{Pane: focus, Handle: h})
	}
}

// --- app.Backend adapters (the runtime-effect seam) --------------------------
//
// orch implements app.Backend so the protocol-neutral app.Dispatcher can drive
// it. Most are one-liners over existing orch/daemon methods; the async round-trip
// pair (StartRead/StartCapture) is above with the pending machinery. All run on
// the loop goroutine.

// Area is the current viewport grid.
func (o *orch) Area() layout.Rect { return o.area }

// ApplyModel reconciles the daemon and rebroadcasts the viewport after a
// model-mutating command.
func (o *orch) ApplyModel() { o.applyModel() }

// BroadcastLayout rebroadcasts just the viewport layout (focus/rename moved) and
// emits any structural event — a focus_changed for the focus commands that route
// here without touching the pane set (rename is a no-op diff). Focus and names
// are durable state, so the debounced save is armed too.
func (o *orch) BroadcastLayout() {
	o.broadcast(o.viewportLayout())
	o.emitStructuralEvents()
	o.broadcastTitle()
	o.saveSoon()
}

// BroadcastPaneTitle pushes a pane's effective title if it is on screen; else it
// rides the chrome resend when the pane next becomes visible. The custom name it
// reflects (pane.rename) is durable, so the save is armed.
func (o *orch) BroadcastPaneTitle(pane uint32) {
	if o.visible[pane] {
		o.broadcast(browserproto.NewPaneTitle(pane, o.effectiveTitle(pane)))
	}
	o.broadcastTitle()
	o.saveSoon()
}

// ScrollPane passes a scrollback delta to the pane's PTY.
func (o *orch) ScrollPane(pane uint32, delta int) error {
	if o.panes[pane] == nil {
		return fmt.Errorf("unknown pane %d", pane)
	}
	o.daemon.send(orchestration.NewScrollViewport(pane, int32(delta)))
	return nil
}

// PaneExists / DaemonConnected gate the async round-trip commands.
func (o *orch) PaneExists(pane uint32) bool { return o.panes[pane] != nil }
func (o *orch) DaemonConnected() bool       { return o.daemon.connected() }

// ReloadConfig re-reads the config file and re-renders the served page so its
// theme and copy-mode keybindings take effect on the next page load / browser
// connection. Server settings (addr, sockets, auth, tls) are fixed for the
// process's lifetime — they need a restart — so this deliberately re-applies
// only the front-end half. A missing config path or a parse/validate error
// leaves the current page in place and reports the failure to the caller. Runs
// on the loop goroutine; the HTTP handler reads o.page atomically.
func (o *orch) ReloadConfig() error {
	if o.cfgPath == "" || o.baseHTML == nil {
		log.Printf("gateway: server.reload_config — no config file in use; nothing to reload")
		return nil
	}
	cfg, path, err := config.Load(o.cfgPath)
	if err != nil {
		log.Printf("gateway: server.reload_config failed: %v", err)
		return err
	}
	o.cfg = cfg // keep config.get / config.set working from the reloaded state
	page := renderPage(o.baseHTML, cfg)
	o.page.Store(&page)
	log.Printf("gateway: reloaded config from %s — theme + keybindings apply to new page loads; server settings need a restart", path)
	return nil
}

// Shutdown tells every browser we are going away, saves the session state, and
// — after a short, bounded final scrollback capture (WS3) — fires the
// process-exit hook (set by main). The persistent termhost daemon is a separate
// process and deliberately survives.
func (o *orch) Shutdown() {
	o.broadcast(browserproto.NewShutdown())
	o.saveNow()
	o.beginFinalCapture(func() {
		if o.stop != nil {
			o.stop()
		}
	})
}

// --- Broadcasting ------------------------------------------------------------

func (o *orch) broadcast(m any) {
	b, err := browserproto.Marshal(m)
	if err != nil {
		log.Printf("gateway: marshal broadcast: %v", err)
		return
	}
	for c := range o.conns {
		o.enqueue(c, b)
	}
}

func (o *orch) send(c *client, m any) {
	b, err := browserproto.Marshal(m)
	if err != nil {
		log.Printf("gateway: marshal: %v", err)
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

// appTitle is the app-level browser-tab title (WS8): the focused pane's
// effective title when it has one, otherwise the active workspace's name.
func (o *orch) appTitle() string {
	if id, ok := o.session.FocusedPane(); ok {
		if t := o.effectiveTitle(uint32(id)); t != "" {
			return t
		}
	}
	wss := o.session.Workspaces()
	if i := o.session.ActiveIndex(); i >= 0 && i < len(wss) {
		return wss[i].DisplayName()
	}
	return ""
}

// broadcastTitle pushes the app title to every browser when it changed —
// called after anything that can move focus or retitle the focused pane.
func (o *orch) broadcastTitle() {
	if t := o.appTitle(); t != o.lastTitle {
		o.lastTitle = t
		o.broadcast(browserproto.NewTitle(t))
	}
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
	if agent, state := rt.effectiveAgent(); agent != "" {
		o.broadcast(browserproto.NewPaneAgent(pid, agent, state, true))
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
		log.Printf("gateway: dropping slow browser connection")
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
				log.Printf("gateway: bad up message: %v", err)
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
		// Effective title, not rt.title: a pane.rename custom name must survive
		// a page reload just like it survives a viewport switch.
		if t := o.effectiveTitle(pid); t != "" {
			o.send(c, browserproto.NewPaneTitle(pid, t))
		}
		if rt.cwd != "" {
			o.send(c, browserproto.NewPaneCwd(pid, rt.cwd))
		}
		if agent, state := rt.effectiveAgent(); agent != "" {
			o.send(c, browserproto.NewPaneAgent(pid, agent, state, true))
		}
		if rt.exited != nil {
			o.send(c, browserproto.NewPaneExited(pid, *rt.exited))
		}
		c.translator(pid).Reset()
		o.daemon.send(orchestration.NewRequestResync(pid))
	}
	o.send(c, o.agentsMsg())
	o.send(c, browserproto.NewTitle(o.appTitle()))
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
			log.Printf("gateway: key encode: %v", err)
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
			log.Printf("gateway: mouse encode: %v", err)
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
			log.Printf("gateway: paste encode: %v", err)
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
		o.send(c, browserproto.NewError(0, "image paste is not supported by the gateway spike"))

	case *browserproto.Cmd:
		o.handleCmd(c, m)
	}
}
