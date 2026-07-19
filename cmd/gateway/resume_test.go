//go:build ghostty

package main

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"

	"github.com/rohanthewiz/herdr-web/internal/orchestration"
	"github.com/rohanthewiz/herdr-web/internal/persist"
)

func idRef(source, agent, value string) persist.AgentSession {
	return persist.AgentSession{Source: source, Agent: agent, Kind: "id", Value: value}
}

// The per-agent plan table, byte for byte (herdr agent_resume::plan): note
// copilot's joined --resume=<v> and cursor's binary name differing from its
// agent label.
func TestResumeArgvTable(t *testing.T) {
	cases := []struct {
		source, agent, kind, value string
		want                       []string
	}{
		{"herdr:claude", "claude", "id", "s1", []string{"claude", "--resume", "s1"}},
		{"herdr:codex", "codex", "id", "s2", []string{"codex", "resume", "s2"}},
		{"herdr:copilot", "copilot", "id", "s3", []string{"copilot", "--resume=s3"}},
		{"herdr:droid", "droid", "id", "s4", []string{"droid", "--resume", "s4"}},
		{"herdr:kimi", "kimi", "id", "s5", []string{"kimi", "--session", "s5"}},
		{"herdr:pi", "pi", "id", "s6", []string{"pi", "--session", "s6"}},
		{"herdr:pi", "pi", "path", "/tmp/pi.sess", []string{"pi", "--session", "/tmp/pi.sess"}},
		{"herdr:hermes", "hermes", "id", "s7", []string{"hermes", "--resume", "s7"}},
		{"herdr:opencode", "opencode", "id", "s8", []string{"opencode", "--session", "s8"}},
		{"herdr:qodercli", "qodercli", "id", "s9", []string{"qodercli", "--resume", "s9"}},
		{"herdr:kilo", "kilo", "id", "s10", []string{"kilo", "--session", "s10"}},
		{"herdr:cursor", "cursor", "id", "s11", []string{"cursor-agent", "--resume", "s11"}},
	}
	for _, c := range cases {
		if got := resumeArgv(c.source, c.agent, c.kind, c.value); !slices.Equal(got, c.want) {
			t.Errorf("resumeArgv(%s, %s): got %v want %v", c.source, c.kind, got, c.want)
		}
	}
}

// Invalid or unofficial refs plan nothing: a corrupted state file must never
// become a malformed exec.
func TestResumeArgvRejects(t *testing.T) {
	long := make([]byte, 513)
	for i := range long {
		long[i] = 'a'
	}
	cases := []struct {
		name                       string
		source, agent, kind, value string
	}{
		{"unofficial source", "custom:claude", "claude", "id", "s1"},
		{"source/agent mismatch", "herdr:claude", "codex", "id", "s1"},
		{"path for non-pi", "herdr:claude", "claude", "path", "/tmp/x"},
		{"relative pi path", "herdr:pi", "pi", "path", "tmp/x"},
		{"empty value", "herdr:claude", "claude", "id", ""},
		{"oversized id", "herdr:claude", "claude", "id", string(long)},
		{"control chars", "herdr:claude", "claude", "id", "s\x001"},
		{"unknown kind", "herdr:claude", "claude", "ref", "s1"},
	}
	for _, c := range cases {
		if got := resumeArgv(c.source, c.agent, c.kind, c.value); got != nil {
			t.Errorf("%s: got %v want nil", c.name, got)
		}
	}
}

// planResume: first pane (ascending id) wins a shared conversation; the
// duplicate gets no plan and no rehydrated ref but still suppresses its stale
// scrollback; invalid entries drop entirely; resume off keeps refs without
// planning.
func TestPlanResume(t *testing.T) {
	saved := map[uint32]persist.AgentSession{
		3: idRef("herdr:claude", "claude", "shared"),
		1: idRef("herdr:claude", "claude", "shared"), // same conversation — lower id wins
		5: idRef("herdr:hermes", "hermes", "other"),
		7: idRef("custom:claude", "claude", "nope"), // unofficial — dropped
	}

	kept, plans, suppress := planResume(saved, true)
	if len(plans) != 2 || plans[1] == nil || plans[5] == nil {
		t.Fatalf("plans: %+v", plans)
	}
	if _, dup := kept[3]; dup || plans[3] != nil {
		t.Fatalf("duplicate pane must lose ref and plan: kept=%+v plans=%+v", kept, plans)
	}
	if !suppress[1] || !suppress[3] || !suppress[5] {
		t.Fatalf("winner and duplicate both suppress history: %+v", suppress)
	}
	if _, ok := kept[7]; ok || suppress[7] {
		t.Fatalf("invalid entry must drop entirely: kept=%+v suppress=%+v", kept, suppress)
	}

	kept, plans, suppress = planResume(saved, false)
	if len(plans) != 0 || len(suppress) != 0 {
		t.Fatalf("resume off must not plan: plans=%+v suppress=%+v", plans, suppress)
	}
	if len(kept) != 3 { // refs preserved (minus the invalid one), no dedupe
		t.Fatalf("resume off keeps valid refs: %+v", kept)
	}
}

// createPane must exec the resume argv in place of the shell, consume the plan
// exactly once, and put the restored ref live on the runtime so the next
// snapshot still carries it.
func TestCreatePaneConsumesResumePlan(t *testing.T) {
	o, err := newOrch(filepath.Join(t.TempDir(), "s.sock"), t.TempDir())
	if err != nil {
		t.Fatalf("newOrch: %v", err)
	}
	pd := newPipeDaemon(t, o)

	pid := uint32(o.session.AllPaneIDs()[0])
	o.resumePlans[pid] = []string{"claude", "--resume", "sess-live"}
	o.restoredAgents[pid] = idRef("herdr:claude", "claude", "sess-live")
	rt := o.panes[pid]
	rt.created = false

	synced := make(chan struct{})
	go func() { o.syncDaemon(); close(synced) }() // pipe writes block until the pump reads

	var cp orchestration.CreatePane
	if err := json.Unmarshal(pd.expect(t, orchestration.MsgCreatePane), &cp); err != nil {
		t.Fatalf("unmarshal create_pane: %v", err)
	}
	<-synced
	if cp.Command != "claude" || !slices.Equal(cp.Args, []string{"--resume", "sess-live"}) {
		t.Fatalf("create_pane command: %q %v", cp.Command, cp.Args)
	}
	if _, ok := o.resumePlans[pid]; ok {
		t.Fatal("plan not consumed")
	}
	if _, ok := o.restoredAgents[pid]; ok {
		t.Fatal("restored ref not moved onto the runtime")
	}
	if rt.agentSession == nil || rt.agentSession.value != "sess-live" || rt.agentSession.source != "herdr:claude" {
		t.Fatalf("runtime session ref: %+v", rt.agentSession)
	}
}

// With no daemon connection the plan must survive for reconcile's retry,
// exactly like seeds and cwds.
func TestCreatePaneKeepsPlanWhenDisconnected(t *testing.T) {
	o, err := newOrch(filepath.Join(t.TempDir(), "s.sock"), t.TempDir())
	if err != nil {
		t.Fatalf("newOrch: %v", err)
	}
	pid := uint32(o.session.AllPaneIDs()[0])
	o.resumePlans[pid] = []string{"claude", "--resume", "keep"}
	o.panes[pid].created = false

	o.syncDaemon() // disconnected: send dropped

	if o.resumePlans[pid] == nil {
		t.Fatal("plan must survive a dropped (disconnected) create")
	}
}

// saveNow persists the arbitrated refs: live runtime refs win, restored
// not-yet-live refs fill in, cleared panes save nothing.
func TestSaveNowPersistsAgentSessions(t *testing.T) {
	o, rt, pub := hookOrch(t)
	o.sessionPath = persist.SessionPath(t.TempDir())

	if herr := o.applyHookReport(methodReportAgentSession, hookReportParams{
		PaneID: pub, Source: "herdr:claude", Agent: "claude", Seq: seq(1),
		AgentSessionID: "sess-live",
	}); herr != nil {
		t.Fatalf("report_agent_session: %+v", herr)
	}
	o.saveNow()

	_, _, agents, err := persist.LoadSession(o.sessionPath)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	want := idRef("herdr:claude", "claude", "sess-live")
	if agents[rt.id] != want {
		t.Fatalf("saved agents: %+v", agents)
	}
}

// A pane exit clears the resumable ref (for a resumed pane the root process IS
// the agent — its conversation ended) and supersedes any restored one.
func TestExitClearsAgentSession(t *testing.T) {
	o, rt, _ := hookOrch(t)
	rt.agentSession = &agentSessionRef{source: "herdr:claude", agent: "claude", kind: "id", value: "sess"}
	o.restoredAgents[rt.id] = idRef("herdr:claude", "claude", "sess")

	o.clearHookOnExit(rt)

	if rt.agentSession != nil {
		t.Fatalf("session ref must clear on exit: %+v", rt.agentSession)
	}
	if _, ok := o.restoredAgents[rt.id]; ok {
		t.Fatal("restored ref must not resurrect an exited pane's session")
	}
}

// Detection contradicting the held ref clears it (herdr's set_detected_state
// rules): the ref's own agent disappearing ends the conversation; a different
// agent on screen invalidates the claim; the same agent still running keeps it.
func TestDetectionClearsAgentSession(t *testing.T) {
	o, rt, _ := hookOrch(t)
	set := func() {
		rt.agent = &orchestration.PaneAgent{PaneID: rt.id, Agent: "claude", State: "working"}
		rt.agentSession = &agentSessionRef{source: "herdr:claude", agent: "claude", kind: "id", value: "sess"}
	}

	set()
	o.onPaneAgent(orchestration.PaneAgent{PaneID: rt.id, Agent: "claude", State: "idle"})
	if rt.agentSession == nil {
		t.Fatal("same agent still detected — ref must survive")
	}

	o.onPaneAgent(orchestration.PaneAgent{PaneID: rt.id, Agent: "", State: "unknown"})
	if rt.agentSession != nil {
		t.Fatal("agent disappeared — ref must clear")
	}

	set()
	o.onPaneAgent(orchestration.PaneAgent{PaneID: rt.id, Agent: "codex", State: "working"})
	if rt.agentSession != nil {
		t.Fatal("different agent detected — ref must clear")
	}
}

// reconcile adoption: a surviving pane must not resume (the agent never died) —
// its plan is dropped and the saved ref goes live on the runtime instead.
func TestReconcileAdoptionDropsPlanKeepsRef(t *testing.T) {
	o, err := newOrch(filepath.Join(t.TempDir(), "s.sock"), t.TempDir())
	if err != nil {
		t.Fatalf("newOrch: %v", err)
	}
	newPipeDaemon(t, o)
	go o.run()

	pid := uint32(o.session.AllPaneIDs()[0])
	done := make(chan struct{})
	o.post(func() {
		o.resumePlans[pid] = []string{"claude", "--resume", "sess"}
		o.restoredAgents[pid] = idRef("herdr:claude", "claude", "sess")
		close(done)
	})
	<-done

	o.daemon.reconcile([]uint32{pid})

	check := make(chan struct{})
	o.post(func() {
		defer close(check)
		if _, ok := o.resumePlans[pid]; ok {
			t.Error("adopted survivor must not keep a resume plan")
		}
		if _, ok := o.restoredAgents[pid]; ok {
			t.Error("adopted survivor's ref must move onto the runtime")
		}
		rt := o.panes[pid]
		if rt == nil || rt.agentSession == nil || rt.agentSession.value != "sess" {
			t.Errorf("runtime session ref: %+v", rt)
		}
	})
	<-check
}
