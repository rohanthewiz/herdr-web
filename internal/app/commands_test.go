package app

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/rohanthewiz/herdr-web/internal/layout"
)

// errScroll stands in for a backend ScrollPane failure (e.g. unknown pane).
var errScroll = errors.New("unknown pane 7")

// These tests drive the protocol-neutral dispatcher directly against a real
// Session and fakes for the runtime seam — no libghostty, no daemon, no browser.
// A shared event log records the order of backend effects and responder replies
// so command flows can be asserted precisely (e.g. server.stop replies before it
// shuts down). This coverage did not exist below the ghostty-tagged, daemon-backed
// integration tests.

// fakeBackend records the runtime effects the dispatcher drives and returns
// canned answers for the gating queries.
type fakeBackend struct {
	log          *[]string
	area         layout.Rect
	paneExists   bool
	daemonUp     bool
	scrollErr    error
	reloadErr    error
	lastRead     Responder
	lastCapture  Responder
	lastWait     Responder
	lastWaitP    WaitForOutputParams
	lastScroll   [2]int
	lastTitle    uint32
	lastWtList   Responder
	lastWtCreate Responder
	lastWtCreP   WorktreeCreateParams
	lastWtOpen   Responder
	lastWtOpenP  WorktreeOpenParams
	lastWtRemove Responder
	lastWtRemP   WorktreeRemoveParams
	lastCfgSetP  ConfigSetParams
}

func (b *fakeBackend) rec(s string)                { *b.log = append(*b.log, s) }
func (b *fakeBackend) Area() layout.Rect           { return b.area }
func (b *fakeBackend) ApplyModel()                 { b.rec("applyModel") }
func (b *fakeBackend) BroadcastLayout()            { b.rec("broadcastLayout") }
func (b *fakeBackend) BroadcastPaneTitle(p uint32) { b.rec("title"); b.lastTitle = p }
func (b *fakeBackend) PaneExists(uint32) bool      { return b.paneExists }
func (b *fakeBackend) DaemonConnected() bool       { return b.daemonUp }
func (b *fakeBackend) ReloadConfig() error         { b.rec("reload"); return b.reloadErr }
func (b *fakeBackend) Shutdown()                   { b.rec("shutdown") }

func (b *fakeBackend) ScrollPane(pane uint32, delta int) error {
	b.rec("scroll")
	b.lastScroll = [2]int{int(pane), delta}
	return b.scrollErr
}
func (b *fakeBackend) StartRead(r Responder, _ ReadParams) { b.rec("startRead"); b.lastRead = r }
func (b *fakeBackend) StartCapture(r Responder, _ CaptureParams) {
	b.rec("startCapture")
	b.lastCapture = r
}
func (b *fakeBackend) StartWaitForOutput(r Responder, p WaitForOutputParams) {
	b.rec("startWait")
	b.lastWait = r
	b.lastWaitP = p
}
func (b *fakeBackend) StartWorktreeList(r Responder, _ WorktreeListParams) {
	b.rec("wtList")
	b.lastWtList = r
}
func (b *fakeBackend) StartWorktreeCreate(r Responder, p WorktreeCreateParams) {
	b.rec("wtCreate")
	b.lastWtCreate = r
	b.lastWtCreP = p
}
func (b *fakeBackend) StartWorktreeOpen(r Responder, p WorktreeOpenParams) {
	b.rec("wtOpen")
	b.lastWtOpen = r
	b.lastWtOpenP = p
}
func (b *fakeBackend) StartWorktreeRemove(r Responder, p WorktreeRemoveParams) {
	b.rec("wtRemove")
	b.lastWtRemove = r
	b.lastWtRemP = p
}
func (b *fakeBackend) ConfigGet(r Responder) { b.rec("cfgGet"); r.OK(ConfigGetResult{Path: "/cfg"}) }
func (b *fakeBackend) ConfigSet(r Responder, p ConfigSetParams) {
	b.rec("cfgSet")
	b.lastCfgSetP = p
	r.OK(nil)
}

// fakeResponder records the terminal reply (and its data), writing "ok"/"fail" to
// the shared log so ordering against backend effects can be asserted.
type fakeResponder struct {
	log      *[]string
	wants    bool
	data     any
	errMsg   string
	okCall   bool
	failCall bool
}

func (r *fakeResponder) WantsReply() bool { return r.wants }
func (r *fakeResponder) OK(data any)      { *r.log = append(*r.log, "ok"); r.okCall = true; r.data = data }
func (r *fakeResponder) Fail(msg string) {
	*r.log = append(*r.log, "fail")
	r.failCall = true
	r.errMsg = msg
}

// jsonDec mirrors gateway's browser param decoder: empty ⇒ ErrNoParams.
type jsonDec struct{ raw []byte }

func (d jsonDec) Decode(v any) error {
	if len(d.raw) == 0 {
		return ErrNoParams
	}
	return json.Unmarshal(d.raw, v)
}

func params(t *testing.T, v any) jsonDec {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	return jsonDec{b}
}
func noParams() jsonDec { return jsonDec{} }
func badJSON() jsonDec  { return jsonDec{[]byte("{")} } // non-ErrNoParams decode error

// cmdHarness wires a real Session, a fakeBackend, and a shared log. daemonUp and
// paneExists default true (the common case); tests flip them.
type cmdHarness struct {
	d   *Dispatcher
	b   *fakeBackend
	s   *Session
	log *[]string
}

func newCmdHarness(t *testing.T) cmdHarness {
	t.Helper()
	log := &[]string{}
	s := newTestSession(t)
	b := &fakeBackend{log: log, area: layout.Rect{Width: 120, Height: 32}, paneExists: true, daemonUp: true}
	return cmdHarness{d: NewDispatcher(s, b), b: b, s: s, log: log}
}

func (h cmdHarness) resp() *fakeResponder { return &fakeResponder{log: h.log, wants: true} }

// A pure focus command rebroadcasts the layout and acks, without reconciling the
// daemon or mutating the pane set.
func TestDispatchFocus(t *testing.T) {
	h := newCmdHarness(t)
	focused, _ := h.s.FocusedPane()
	r := h.resp()

	h.d.Dispatch(CmdPaneFocus, params(t, PaneParams{Pane: uint32(focused)}), r)

	if !r.okCall || r.failCall {
		t.Fatalf("focus should ack ok: ok=%v fail=%v (%q)", r.okCall, r.failCall, r.errMsg)
	}
	if got := *h.log; len(got) != 2 || got[0] != "broadcastLayout" || got[1] != "ok" {
		t.Fatalf("focus effects = %v, want [broadcastLayout ok]", got)
	}
	if len(h.s.VisiblePaneIDs()) != 1 {
		t.Fatalf("focus must not change the pane set")
	}
}

// A required-params command with no params fails in the historical wording.
func TestDispatchMissingParams(t *testing.T) {
	h := newCmdHarness(t)
	r := h.resp()

	h.d.Dispatch(CmdPaneFocus, noParams(), r)

	if !r.failCall || r.errMsg != "bad params: missing params" {
		t.Fatalf("missing params: fail=%v msg=%q, want bad params: missing params", r.failCall, r.errMsg)
	}
	if len(*h.log) != 1 || (*h.log)[0] != "fail" {
		t.Fatalf("a params failure must not run effects, log=%v", *h.log)
	}
}

// A bad split direction fails without mutating the session or reconciling.
func TestDispatchSplitBadDirection(t *testing.T) {
	h := newCmdHarness(t)
	r := h.resp()

	h.d.Dispatch(CmdPaneSplit, params(t, SplitParams{Direction: "diagonal"}), r)

	if !r.failCall {
		t.Fatalf("bad direction should fail")
	}
	if len(h.s.VisiblePaneIDs()) != 1 {
		t.Fatalf("failed split must not mutate the session, panes=%d", len(h.s.VisiblePaneIDs()))
	}
	for _, e := range *h.log {
		if e == "applyModel" {
			t.Fatalf("failed split must not reconcile the daemon, log=%v", *h.log)
		}
	}
}

// A valid split mutates the session and reconciles exactly once, then acks.
func TestDispatchSplitOK(t *testing.T) {
	h := newCmdHarness(t)
	r := h.resp()

	h.d.Dispatch(CmdPaneSplit, params(t, SplitParams{Direction: SplitH}), r)

	if !r.okCall || r.failCall {
		t.Fatalf("valid split should ack: ok=%v fail=%v (%q)", r.okCall, r.failCall, r.errMsg)
	}
	if len(h.s.VisiblePaneIDs()) != 2 {
		t.Fatalf("split should leave 2 panes, got %d", len(h.s.VisiblePaneIDs()))
	}
	if got := *h.log; len(got) != 2 || got[0] != "applyModel" || got[1] != "ok" {
		t.Fatalf("split effects = %v, want [applyModel ok]", got)
	}
}

// read with no reply channel (WantsReply false) does nothing — no orphan pending.
func TestDispatchReadNoReply(t *testing.T) {
	h := newCmdHarness(t)
	r := h.resp()
	r.wants = false

	h.d.Dispatch(CmdRead, params(t, ReadParams{Pane: 1}), r)

	if r.okCall || r.failCall || len(*h.log) != 0 {
		t.Fatalf("id-less read should do nothing, log=%v ok=%v fail=%v", *h.log, r.okCall, r.failCall)
	}
}

// read on an unknown pane fails before starting a round-trip.
func TestDispatchReadUnknownPane(t *testing.T) {
	h := newCmdHarness(t)
	h.b.paneExists = false
	r := h.resp()

	h.d.Dispatch(CmdRead, params(t, ReadParams{Pane: 9999}), r)

	if !r.failCall || r.errMsg != "unknown pane 9999" {
		t.Fatalf("unknown-pane read: fail=%v msg=%q", r.failCall, r.errMsg)
	}
	if h.b.lastRead != nil {
		t.Fatalf("no round-trip should start for an unknown pane")
	}
}

// read with the daemon down fails with the connection message.
func TestDispatchReadDaemonDown(t *testing.T) {
	h := newCmdHarness(t)
	h.b.daemonUp = false
	r := h.resp()

	h.d.Dispatch(CmdRead, params(t, ReadParams{Pane: 1}), r)

	if !r.failCall || r.errMsg != "termhost daemon not connected" {
		t.Fatalf("daemon-down read: fail=%v msg=%q", r.failCall, r.errMsg)
	}
}

// A valid read starts the async round-trip carrying the caller's responder, and
// does not reply yet (the daemon reply will).
func TestDispatchReadStarts(t *testing.T) {
	h := newCmdHarness(t)
	r := h.resp()

	h.d.Dispatch(CmdRead, params(t, ReadParams{Pane: 1}), r)

	if r.okCall || r.failCall {
		t.Fatalf("read must not reply synchronously")
	}
	if got := *h.log; len(got) != 1 || got[0] != "startRead" {
		t.Fatalf("read effects = %v, want [startRead]", got)
	}
	if h.b.lastRead != Responder(r) {
		t.Fatalf("StartRead should receive the caller's responder")
	}
}

// capture on an unknown pane fails (same gate as read).
func TestDispatchCaptureUnknownPane(t *testing.T) {
	h := newCmdHarness(t)
	h.b.paneExists = false
	r := h.resp()

	h.d.Dispatch(CmdCapture, params(t, CaptureParams{Pane: 42}), r)

	if !r.failCall || r.errMsg != "unknown pane 42" || h.b.lastCapture != nil {
		t.Fatalf("unknown-pane capture: fail=%v msg=%q lastCapture=%v", r.failCall, r.errMsg, h.b.lastCapture)
	}
}

// scroll surfaces the backend's error (e.g. unknown pane) as a failure.
func TestDispatchScrollError(t *testing.T) {
	h := newCmdHarness(t)
	h.b.scrollErr = errScroll
	r := h.resp()

	h.d.Dispatch(CmdScroll, params(t, ScrollParams{Pane: 7, Delta: -3}), r)

	if !r.failCall || r.errMsg != errScroll.Error() {
		t.Fatalf("scroll error: fail=%v msg=%q", r.failCall, r.errMsg)
	}
	if h.b.lastScroll != [2]int{7, -3} {
		t.Fatalf("scroll should pass pane/delta through, got %v", h.b.lastScroll)
	}
}

// An all-optional command with no params decodes to the zero value (focused pane)
// rather than failing.
func TestDispatchZoomOptionalNoParams(t *testing.T) {
	h := newCmdHarness(t)
	r := h.resp()

	h.d.Dispatch(CmdPaneZoom, noParams(), r)

	if !r.okCall || r.failCall {
		t.Fatalf("zoom with no params should ack: ok=%v fail=%v (%q)", r.okCall, r.failCall, r.errMsg)
	}
	if got := *h.log; len(got) != 2 || got[0] != "applyModel" || got[1] != "ok" {
		t.Fatalf("zoom effects = %v, want [applyModel ok]", got)
	}
}

// workspace.close ignores ALL decode errors (not just ErrNoParams): malformed
// params still close the active workspace.
func TestDispatchWorkspaceCloseIgnoresBadParams(t *testing.T) {
	h := newCmdHarness(t)
	if _, err := h.s.CreateWorkspace(); err != nil { // need a 2nd so close is legal
		t.Fatalf("CreateWorkspace: %v", err)
	}
	r := h.resp()

	h.d.Dispatch(CmdWorkspaceClose, badJSON(), r)

	if !r.okCall || r.failCall {
		t.Fatalf("workspace.close should ignore a decode error and ack: ok=%v fail=%v (%q)", r.okCall, r.failCall, r.errMsg)
	}
	if len(h.s.Workspaces()) != 1 {
		t.Fatalf("workspace.close should have closed one workspace, have %d", len(h.s.Workspaces()))
	}
}

// server.stop replies BEFORE it shuts down, so the caller receives its result.
func TestDispatchServerStopOrder(t *testing.T) {
	h := newCmdHarness(t)
	r := h.resp()

	h.d.Dispatch(CmdServerStop, noParams(), r)

	if got := *h.log; len(got) != 2 || got[0] != "ok" || got[1] != "shutdown" {
		t.Fatalf("server.stop order = %v, want [ok shutdown]", got)
	}
}

// server.reload_config acks after the backend's (no-op) reload.
func TestDispatchReloadConfig(t *testing.T) {
	h := newCmdHarness(t)
	r := h.resp()

	h.d.Dispatch(CmdServerReloadConfig, noParams(), r)

	if got := *h.log; len(got) != 2 || got[0] != "reload" || got[1] != "ok" {
		t.Fatalf("reload_config effects = %v, want [reload ok]", got)
	}
}

// Every name CommandNames() advertises must actually be routed by Dispatch —
// none may fall through to the unknown-command default. This guards the
// enumeration (which CLI/control-API clients trust) against drifting from the
// switch. A command may still fail for a domain/params reason on empty input;
// we only reject the "not supported yet" fall-through.
func TestCommandNamesAllRouted(t *testing.T) {
	const unknown = "not supported yet"
	for _, name := range CommandNames() {
		h := newCmdHarness(t) // fresh session per command; order-independent
		r := h.resp()
		h.d.Dispatch(name, noParams(), r)
		if r.failCall && strings.Contains(r.errMsg, unknown) {
			t.Errorf("command %q is enumerated but not routed by Dispatch (%q)", name, r.errMsg)
		}
	}
}

// --- pane.wait_for_output ----------------------------------------------------

// With no reply channel a wait yields nothing to await, so it short-circuits
// before registering a waiter (no backend effect, no reply).
func TestDispatchWaitNoReply(t *testing.T) {
	h := newCmdHarness(t)
	r := &fakeResponder{log: h.log, wants: false}
	h.d.Dispatch(CmdWaitForOutput, params(t, WaitForOutputParams{Pane: 1, Pattern: "x"}), r)
	if r.okCall || r.failCall {
		t.Fatalf("no-reply wait should not resolve: ok=%v fail=%v", r.okCall, r.failCall)
	}
	if len(*h.log) != 0 {
		t.Fatalf("no-reply wait should not start a waiter: log=%v", *h.log)
	}
}

// An empty pattern / bad regex is rejected as bad params before a waiter starts.
func TestDispatchWaitBadPattern(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    WaitForOutputParams
		want string
	}{
		{"empty", WaitForOutputParams{Pane: 1}, "empty pattern"},
		{"badRegex", WaitForOutputParams{Pane: 1, Pattern: "(", Regex: true}, "bad regex"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newCmdHarness(t)
			r := h.resp()
			h.d.Dispatch(CmdWaitForOutput, params(t, tc.p), r)
			if !r.failCall || !strings.Contains(r.errMsg, tc.want) {
				t.Fatalf("fail=%v msg=%q, want %q", r.failCall, r.errMsg, tc.want)
			}
			if h.b.lastWait != nil {
				t.Fatalf("bad pattern should not start a waiter")
			}
		})
	}
}

// The daemon-round-trip gates (unknown pane, daemon down) fail before starting.
func TestDispatchWaitGated(t *testing.T) {
	unknown := newCmdHarness(t)
	unknown.b.paneExists = false
	r := unknown.resp()
	unknown.d.Dispatch(CmdWaitForOutput, params(t, WaitForOutputParams{Pane: 9, Pattern: "x"}), r)
	if !r.failCall || !strings.Contains(r.errMsg, "unknown pane") {
		t.Fatalf("unknown pane: fail=%v msg=%q", r.failCall, r.errMsg)
	}

	down := newCmdHarness(t)
	down.b.daemonUp = false
	r = down.resp()
	down.d.Dispatch(CmdWaitForOutput, params(t, WaitForOutputParams{Pane: 1, Pattern: "x"}), r)
	if !r.failCall || !strings.Contains(r.errMsg, "not connected") {
		t.Fatalf("daemon down: fail=%v msg=%q", r.failCall, r.errMsg)
	}
}

// A valid wait registers with the backend and does not resolve synchronously; the
// params are forwarded intact.
func TestDispatchWaitStarts(t *testing.T) {
	h := newCmdHarness(t)
	r := h.resp()
	p := WaitForOutputParams{Pane: 1, Pattern: "ready", TimeoutMs: 5000}
	h.d.Dispatch(CmdWaitForOutput, params(t, p), r)
	if r.okCall || r.failCall {
		t.Fatalf("wait should not resolve synchronously: ok=%v fail=%v", r.okCall, r.failCall)
	}
	if len(*h.log) != 1 || (*h.log)[0] != "startWait" {
		t.Fatalf("expected a single startWait, log=%v", *h.log)
	}
	if h.b.lastWait != r || h.b.lastWaitP != p {
		t.Fatalf("wait not forwarded: resp=%v params=%+v", h.b.lastWait == r, h.b.lastWaitP)
	}
}

// Matcher compiles a substring or regex predicate, returns the matched line for
// context, and validates the pattern.
func TestWaitForOutputMatcher(t *testing.T) {
	sub, err := WaitForOutputParams{Pattern: "DONE"}.Matcher()
	if err != nil {
		t.Fatalf("substring matcher: %v", err)
	}
	if line, ok := sub("building\n  all DONE here  \nnext"); !ok || line != "all DONE here" {
		t.Fatalf("substring match: line=%q ok=%v", line, ok)
	}
	if _, ok := sub("nothing to see"); ok {
		t.Fatalf("substring should not match")
	}

	re, err := WaitForOutputParams{Pattern: `exit code \d+`, Regex: true}.Matcher()
	if err != nil {
		t.Fatalf("regex matcher: %v", err)
	}
	if line, ok := re("run\nexit code 42\n"); !ok || line != "exit code 42" {
		t.Fatalf("regex match: line=%q ok=%v", line, ok)
	}

	if _, err := (WaitForOutputParams{}).Matcher(); err == nil {
		t.Fatalf("empty pattern should error")
	}
	if _, err := (WaitForOutputParams{Pattern: "(", Regex: true}).Matcher(); err == nil {
		t.Fatalf("bad regex should error")
	}
}

// --- worktree.* / config.* ----------------------------------------------------

// worktree.list is result-only: with no reply channel it short-circuits before
// starting the git round-trip; with one it forwards the caller's responder and
// does not resolve synchronously.
func TestDispatchWorktreeList(t *testing.T) {
	silent := newCmdHarness(t)
	r := &fakeResponder{log: silent.log, wants: false}
	silent.d.Dispatch(CmdWorktreeList, noParams(), r)
	if len(*silent.log) != 0 || silent.b.lastWtList != nil {
		t.Fatalf("id-less worktree.list should do nothing, log=%v", *silent.log)
	}

	h := newCmdHarness(t)
	rr := h.resp()
	h.d.Dispatch(CmdWorktreeList, noParams(), rr)
	if rr.okCall || rr.failCall {
		t.Fatalf("worktree.list must not resolve synchronously")
	}
	if got := *h.log; len(got) != 1 || got[0] != "wtList" || h.b.lastWtList != Responder(rr) {
		t.Fatalf("worktree.list effects = %v", got)
	}
}

// worktree.create forwards its params (all optional — the backend defaults the
// branch and path) and resolves asynchronously.
func TestDispatchWorktreeCreateForwards(t *testing.T) {
	h := newCmdHarness(t)
	r := h.resp()
	p := WorktreeCreateParams{Branch: "worktree/brave-river-0001", Path: "/w/repo/brave-river"}

	h.d.Dispatch(CmdWorktreeCreate, params(t, p), r)

	if r.okCall || r.failCall {
		t.Fatalf("worktree.create must not resolve synchronously")
	}
	if h.b.lastWtCreate != Responder(r) || h.b.lastWtCreP != p {
		t.Fatalf("worktree.create not forwarded: %+v", h.b.lastWtCreP)
	}
}

// The required-field gates: worktree.open needs a path, worktree.remove a
// workspace id — both fail before reaching the backend.
func TestDispatchWorktreeRequiredFields(t *testing.T) {
	for _, tc := range []struct {
		name, cmd, want string
	}{
		{"open", CmdWorktreeOpen, "path is required"},
		{"remove", CmdWorktreeRemove, "workspace is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newCmdHarness(t)
			r := h.resp()
			h.d.Dispatch(tc.cmd, noParams(), r)
			if !r.failCall || !strings.Contains(r.errMsg, tc.want) {
				t.Fatalf("fail=%v msg=%q, want %q", r.failCall, r.errMsg, tc.want)
			}
			if h.b.lastWtOpen != nil || h.b.lastWtRemove != nil {
				t.Fatalf("missing required field must not reach the backend")
			}
		})
	}
}

// worktree.open / worktree.remove forward their params to the backend.
func TestDispatchWorktreeOpenRemoveForward(t *testing.T) {
	h := newCmdHarness(t)
	r := h.resp()
	h.d.Dispatch(CmdWorktreeOpen, params(t, WorktreeOpenParams{Path: "/w/repo/x"}), r)
	if h.b.lastWtOpen != Responder(r) || h.b.lastWtOpenP.Path != "/w/repo/x" {
		t.Fatalf("worktree.open not forwarded: %+v", h.b.lastWtOpenP)
	}

	h = newCmdHarness(t)
	r = h.resp()
	h.d.Dispatch(CmdWorktreeRemove, params(t, WorktreeRemoveParams{Workspace: "w2", Force: true}), r)
	if h.b.lastWtRemove != Responder(r) || h.b.lastWtRemP != (WorktreeRemoveParams{Workspace: "w2", Force: true}) {
		t.Fatalf("worktree.remove not forwarded: %+v", h.b.lastWtRemP)
	}
}

// config.get is result-only (short-circuits with no reply channel); config.set
// forwards the decoded sections.
func TestDispatchConfig(t *testing.T) {
	silent := newCmdHarness(t)
	r := &fakeResponder{log: silent.log, wants: false}
	silent.d.Dispatch(CmdConfigGet, noParams(), r)
	if len(*silent.log) != 0 {
		t.Fatalf("id-less config.get should do nothing, log=%v", *silent.log)
	}

	h := newCmdHarness(t)
	rr := h.resp()
	h.d.Dispatch(CmdConfigGet, noParams(), rr)
	if !rr.okCall {
		t.Fatalf("config.get should resolve through the backend")
	}
	if res, ok := rr.data.(ConfigGetResult); !ok || res.Path != "/cfg" {
		t.Fatalf("config.get data = %#v", rr.data)
	}

	h = newCmdHarness(t)
	rr = h.resp()
	h.d.Dispatch(CmdConfigSet, params(t, ConfigSetParams{Theme: &ConfigTheme{Font: "monospace"}}), rr)
	if !rr.okCall || h.b.lastCfgSetP.Theme == nil || h.b.lastCfgSetP.Theme.Font != "monospace" {
		t.Fatalf("config.set not forwarded: %+v", h.b.lastCfgSetP)
	}
}

// An unknown command name fails with the not-supported message.
func TestDispatchUnknownCommand(t *testing.T) {
	h := newCmdHarness(t)
	r := h.resp()

	h.d.Dispatch("pane.teleport", noParams(), r)

	if !r.failCall || r.errMsg != `command "pane.teleport" not supported yet (WS2 in progress)` {
		t.Fatalf("unknown command: fail=%v msg=%q", r.failCall, r.errMsg)
	}
}
