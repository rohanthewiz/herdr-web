//go:build ghostty

package main

import (
	"bufio"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/rohanthewiz/herdr-web/internal/orchestration"
)

// hookOrch builds an orch with the first pane's runtime registered (newOrch
// leaves o.panes empty until reconcile; hook tests need the runtime).
func hookOrch(t *testing.T) (*orch, *paneRuntime, string) {
	t.Helper()
	o, err := newOrch(filepath.Join(t.TempDir(), "s.sock"), t.TempDir())
	if err != nil {
		t.Fatalf("newOrch: %v", err)
	}
	id := o.session.AllPaneIDs()[0]
	pid := uint32(id)
	rt := o.panes[pid]
	if rt == nil {
		rt = &paneRuntime{id: pid}
		o.panes[pid] = rt
	}
	pub, ok := o.session.PublicPaneID(id)
	if !ok {
		t.Fatal("no public pane id")
	}
	return o, rt, pub
}

func seq(n uint64) *uint64 { return &n }

// A full-lifecycle hook (hermes) owns the pane's agent state over detection,
// and release hands it back — with a suppression window against late packets.
func TestHookAuthorityLifecycle(t *testing.T) {
	o, rt, pub := hookOrch(t)

	// Detection sees a plain shell.
	o.onPaneAgent(orchestration.PaneAgent{PaneID: rt.id, Agent: "", State: "unknown"})

	if herr := o.applyHookReport(methodReportAgent, hookReportParams{
		PaneID: pub, Source: "herdr:hermes", Agent: "hermes", State: "working", Seq: seq(10),
	}); herr != nil {
		t.Fatalf("report_agent: %+v", herr)
	}
	if a, s := rt.effectiveAgent(); a != "hermes" || s != "working" {
		t.Fatalf("effective = %s/%s, want hermes/working", a, s)
	}

	// Detection idling does not downgrade the live authority.
	o.onPaneAgent(orchestration.PaneAgent{PaneID: rt.id, Agent: "hermes", State: "idle"})
	if a, s := rt.effectiveAgent(); a != "hermes" || s != "working" {
		t.Fatalf("after detection idle: effective = %s/%s, want hermes/working", a, s)
	}

	// Stale seq is silently dropped.
	if herr := o.applyHookReport(methodReportAgent, hookReportParams{
		PaneID: pub, Source: "herdr:hermes", Agent: "hermes", State: "idle", Seq: seq(10),
	}); herr != nil {
		t.Fatalf("stale seq should reply ok: %+v", herr)
	}
	if _, s := rt.effectiveAgent(); s != "working" {
		t.Fatalf("stale seq mutated state to %s", s)
	}

	// Release clears the authority; detection drives again.
	if herr := o.applyHookReport(methodReleaseAgent, hookReportParams{
		PaneID: pub, Source: "herdr:hermes", Agent: "hermes", Seq: seq(20),
	}); herr != nil {
		t.Fatalf("release: %+v", herr)
	}
	if rt.hook != nil {
		t.Fatal("release left hook authority")
	}
	if a, s := rt.effectiveAgent(); a != "hermes" || s != "idle" {
		t.Fatalf("after release: effective = %s/%s, want detection hermes/idle", a, s)
	}

	// A late duplicate (same no-session conversation, newer seq) cannot
	// resurrect the released agent…
	if herr := o.applyHookReport(methodReportAgent, hookReportParams{
		PaneID: pub, Source: "herdr:hermes", Agent: "hermes", State: "working", Seq: seq(30),
	}); herr != nil {
		t.Fatalf("suppressed report: %+v", herr)
	}
	if rt.hook != nil {
		t.Fatal("suppressed report re-acquired authority")
	}
	// …but a new conversation (different session ref) clears the suppression.
	if herr := o.applyHookReport(methodReportAgent, hookReportParams{
		PaneID: pub, Source: "herdr:hermes", Agent: "hermes", State: "working", Seq: seq(40),
		AgentSessionID: "conv-2",
	}); herr != nil {
		t.Fatalf("new-session report: %+v", herr)
	}
	if rt.hook == nil {
		t.Fatal("new-session report should re-acquire authority")
	}
}

// Reserved native agents (claude) get session identity recorded but never state
// authority, and their releases are no-ops — detection drives their state.
func TestHookReservedNativeSessionOnly(t *testing.T) {
	o, rt, pub := hookOrch(t)

	if herr := o.applyHookReport(methodReportAgentSession, hookReportParams{
		PaneID: pub, Source: "herdr:claude", Agent: "claude", Seq: seq(1),
		AgentSessionID: "sess-abc",
	}); herr != nil {
		t.Fatalf("report_agent_session: %+v", herr)
	}
	if rt.agentSession == nil || rt.agentSession.value != "sess-abc" || rt.agentSession.kind != "id" {
		t.Fatalf("session ref = %+v", rt.agentSession)
	}
	if rt.hook != nil {
		t.Fatal("session report must not create authority")
	}

	// A state report from a reserved source is downgraded to a session record —
	// and a *different* id for the held conversation is ignored (herdr's
	// conflicting_current_session_ref: a nested/sub-agent session must not
	// clobber the resumable parent session; the held ref stays until detection
	// or release clears it).
	if herr := o.applyHookReport(methodReportAgent, hookReportParams{
		PaneID: pub, Source: "herdr:claude", Agent: "claude", State: "working", Seq: seq(2),
		AgentSessionID: "sess-def",
	}); herr != nil {
		t.Fatalf("report_agent: %+v", herr)
	}
	if rt.hook != nil {
		t.Fatal("reserved native source must not acquire state authority")
	}
	if rt.agentSession == nil || rt.agentSession.value != "sess-abc" {
		t.Fatalf("conflicting session id must be ignored: %+v", rt.agentSession)
	}

	// Release from a reserved source is a no-op.
	if herr := o.applyHookReport(methodReleaseAgent, hookReportParams{
		PaneID: pub, Source: "herdr:claude", Agent: "claude", Seq: seq(3),
	}); herr != nil {
		t.Fatalf("release: %+v", herr)
	}
	if rt.agentSession == nil {
		t.Fatal("reserved-source release must not clear the session ref")
	}
}

// A detected visible blocker upgrades a non-full-lifecycle hook's non-blocked
// state (the user is being asked something the hook didn't know about), but
// never a full-lifecycle hook's.
func TestHookVisibleBlockerOverride(t *testing.T) {
	o, rt, pub := hookOrch(t)

	// kimi is official but not full-lifecycle → override applies.
	if herr := o.applyHookReport(methodReportAgent, hookReportParams{
		PaneID: pub, Source: "herdr:kimi", Agent: "kimi", State: "working", Seq: seq(1),
	}); herr != nil {
		t.Fatalf("report_agent: %+v", herr)
	}
	o.onPaneAgent(orchestration.PaneAgent{PaneID: rt.id, Agent: "kimi", State: "blocked", VisibleBlocker: true})
	if _, s := rt.effectiveAgent(); s != "blocked" {
		t.Fatalf("visible blocker should override: state = %s", s)
	}

	// hermes is full-lifecycle → its authority stands.
	rt.hook = nil
	rt.hookSeqs = nil
	if herr := o.applyHookReport(methodReportAgent, hookReportParams{
		PaneID: pub, Source: "herdr:hermes", Agent: "hermes", State: "working", Seq: seq(1),
	}); herr != nil {
		t.Fatalf("report_agent: %+v", herr)
	}
	o.onPaneAgent(orchestration.PaneAgent{PaneID: rt.id, Agent: "hermes", State: "blocked", VisibleBlocker: true})
	if _, s := rt.effectiveAgent(); s != "working" {
		t.Fatalf("full-lifecycle authority must stand: state = %s", s)
	}
}

// Detection of a different agent drops a live hook authority.
func TestHookConflictingDetectionDropsAuthority(t *testing.T) {
	o, rt, pub := hookOrch(t)
	if herr := o.applyHookReport(methodReportAgent, hookReportParams{
		PaneID: pub, Source: "herdr:opencode", Agent: "opencode", State: "working", Seq: seq(1),
	}); herr != nil {
		t.Fatalf("report_agent: %+v", herr)
	}
	o.onPaneAgent(orchestration.PaneAgent{PaneID: rt.id, Agent: "codex", State: "working"})
	if rt.hook != nil {
		t.Fatal("conflicting detected agent should drop the authority")
	}
	if a, _ := rt.effectiveAgent(); a != "codex" {
		t.Fatalf("effective agent = %s, want codex", a)
	}
}

// Pane exit wipes the authority so a late in-flight packet cannot resurrect it.
func TestHookClearedOnExit(t *testing.T) {
	o, rt, pub := hookOrch(t)
	if herr := o.applyHookReport(methodReportAgent, hookReportParams{
		PaneID: pub, Source: "herdr:kilo", Agent: "kilo", State: "working", Seq: seq(1),
	}); herr != nil {
		t.Fatalf("report_agent: %+v", herr)
	}
	o.clearHookOnExit(rt)
	if rt.hook != nil {
		t.Fatal("exit should clear authority")
	}
	if herr := o.applyHookReport(methodReportAgent, hookReportParams{
		PaneID: pub, Source: "herdr:kilo", Agent: "kilo", State: "working", Seq: seq(2),
	}); herr != nil {
		t.Fatalf("late report: %+v", herr)
	}
	if rt.hook != nil {
		t.Fatal("late packet resurrected a dead agent")
	}
}

// Error surface: unknown pane, empty agent, bad state, missing fields.
func TestHookReportErrors(t *testing.T) {
	o, _, pub := hookOrch(t)
	cases := []struct {
		name   string
		method string
		p      hookReportParams
		code   string
	}{
		{"unknown pane", methodReportAgent,
			hookReportParams{PaneID: "w9:pz", Source: "herdr:hermes", Agent: "hermes", State: "working"}, "pane_not_found"},
		{"empty agent", methodReportAgent,
			hookReportParams{PaneID: pub, Source: "herdr:hermes", Agent: "  ", State: "working"}, "invalid_agent"},
		{"bad state", methodReportAgent,
			hookReportParams{PaneID: pub, Source: "herdr:x", Agent: "x", State: "sleeping"}, "invalid_request"},
		{"missing source", methodReportAgent,
			hookReportParams{PaneID: pub, Agent: "hermes", State: "working"}, "invalid_request"},
	}
	for _, c := range cases {
		herr := o.applyHookReport(c.method, c.p)
		if herr == nil || herr.Code != c.code {
			t.Errorf("%s: got %+v, want code %s", c.name, herr, c.code)
		}
	}
}

// Per-source seq: monotonic per source, missing seq only accepted first.
func TestAcceptHookSeq(t *testing.T) {
	rt := &paneRuntime{}
	if !rt.acceptHookSeq("a", nil) {
		t.Error("first no-seq report should be accepted")
	}
	if !rt.acceptHookSeq("a", seq(5)) {
		t.Error("first seq should be accepted")
	}
	if rt.acceptHookSeq("a", seq(5)) {
		t.Error("equal seq should be dropped")
	}
	if rt.acceptHookSeq("a", seq(4)) {
		t.Error("older seq should be dropped")
	}
	if rt.acceptHookSeq("a", nil) {
		t.Error("no-seq after a seq should be dropped")
	}
	if !rt.acceptHookSeq("a", seq(6)) {
		t.Error("newer seq should be accepted")
	}
	if !rt.acceptHookSeq("b", seq(1)) {
		t.Error("sources are independent")
	}
}

// Session refs: official sources only; pi takes the path form; id validation.
func TestSessionRefFromReport(t *testing.T) {
	if ref := sessionRefFromReport("herdr:claude", "claude", hookReportParams{AgentSessionID: "s1"}); ref == nil || ref.kind != "id" || ref.value != "s1" {
		t.Fatalf("claude id ref: %+v", ref)
	}
	if ref := sessionRefFromReport("custom:thing", "thing", hookReportParams{AgentSessionID: "s1"}); ref != nil {
		t.Fatalf("custom source must not produce a ref: %+v", ref)
	}
	if ref := sessionRefFromReport("herdr:pi", "pi", hookReportParams{AgentSessionPath: "/abs/path", AgentSessionID: "s1"}); ref == nil || ref.kind != "path" || ref.value != "/abs/path" {
		t.Fatalf("pi path ref: %+v", ref)
	}
	if ref := sessionRefFromReport("herdr:pi", "pi", hookReportParams{AgentSessionPath: "rel/path", AgentSessionID: "s1"}); ref == nil || ref.kind != "id" {
		t.Fatalf("relative pi path must fall back to id: %+v", ref)
	}
	if ref := sessionRefFromReport("herdr:codex", "codex", hookReportParams{AgentSessionPath: "/abs"}); ref != nil {
		t.Fatalf("non-pi path must be rejected: %+v", ref)
	}
	if ref := sessionRefFromReport("herdr:claude", "claude", hookReportParams{AgentSessionID: "bad\x01id"}); ref != nil {
		t.Fatalf("control chars must be rejected: %+v", ref)
	}
}

// paneEnvMap produces exactly the env the installed hooks read.
func TestPaneEnvMap(t *testing.T) {
	m := paneEnvMap("/tmp/h.sock", 7, "w1:p3")
	if m["HERDR_SOCKET_PATH"] != "/tmp/h.sock" || m["HERDR_PANE_ID"] != "w1:p3" || m["HERDR_ENV"] != "1" {
		t.Fatalf("env map: %v", m)
	}
	if m = paneEnvMap("/tmp/h.sock", 7, ""); m["HERDR_PANE_ID"] != "p_7" {
		t.Fatalf("fallback pane id: %v", m)
	}
	if paneEnvMap("", 7, "w1:p3") != nil {
		t.Fatal("no socket ⇒ no env")
	}
}

// End-to-end over the socket: herdr's exact framing and reply shapes.
func TestServeHooksEndToEnd(t *testing.T) {
	o, _, pub := hookOrch(t)
	go o.run()

	sock := filepath.Join(t.TempDir(), "hooks.sock")
	cleanup, err := serveHooks(o, sock)
	if err != nil {
		t.Fatalf("serveHooks: %v", err)
	}
	defer cleanup()

	send := func(line string) map[string]json.RawMessage {
		t.Helper()
		conn, err := net.DialTimeout("unix", sock, time.Second)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		if _, err := conn.Write([]byte(line + "\n")); err != nil {
			t.Fatalf("write: %v", err)
		}
		reply, err := bufio.NewReader(conn).ReadBytes('\n')
		if err != nil {
			t.Fatalf("read reply: %v", err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(reply, &m); err != nil {
			t.Fatalf("reply not JSON: %v (%s)", err, reply)
		}
		return m
	}

	// The exact request the claude SessionStart hook sends.
	m := send(`{"id":"herdr:claude:123:abc","method":"pane.report_agent_session","params":{"pane_id":"` + pub + `","source":"herdr:claude","agent":"claude","seq":42,"agent_session_id":"sess-1"}}`)
	if string(m["id"]) != `"herdr:claude:123:abc"` || string(m["result"]) != `{"type":"ok"}` {
		t.Fatalf("ok reply: %v", m)
	}

	// Unknown pane → herdr's error envelope.
	m = send(`{"id":"x","method":"pane.report_agent","params":{"pane_id":"w9:pz","source":"herdr:hermes","agent":"hermes","state":"working"}}`)
	var e struct{ Code, Message string }
	if err := json.Unmarshal(m["error"], &e); err != nil || e.Code != "pane_not_found" {
		t.Fatalf("error reply: %v (err %v)", m, err)
	}

	// Unknown method.
	m = send(`{"id":"y","method":"pane.nope","params":{}}`)
	if err := json.Unmarshal(m["error"], &e); err != nil || e.Code != "invalid_request" {
		t.Fatalf("unknown method reply: %v", m)
	}
}
