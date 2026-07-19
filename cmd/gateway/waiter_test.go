//go:build ghostty

package main

import (
	"path/filepath"
	"testing"

	"github.com/rohanthewiz/herdr-web/internal/app"
	"github.com/rohanthewiz/herdr-web/internal/browserproto"
	"github.com/rohanthewiz/herdr-web/internal/layout"
)

// pane.wait_for_output waiters and control-API event subscribers both live on the
// orchestrator loop and resolve/emit purely from state the loop owns. These tests
// drive that logic directly against a bare orch — no daemon, no browser — the way
// pending_test drives the read/capture resolution.

func newWaiterHarness() *orch {
	return &orch{
		waiters:     map[uint32][]*waiter{},
		waiterCheck: map[uint32]bool{},
		outAccum:    map[uint32]*outputScanner{},
		subs:        map[*ctlSubscriber]struct{}{},
	}
}

// recWaiter records a waiter's terminal resolution (the app.Responder seam).
type recWaiter struct {
	ok, fail bool
	res      app.WaitForOutputResult
	errMsg   string
}

func (*recWaiter) WantsReply() bool { return true }
func (r *recWaiter) OK(data any) {
	r.ok = true
	if v, ok := data.(app.WaitForOutputResult); ok {
		r.res = v
	}
}
func (r *recWaiter) Fail(msg string) { r.fail = true; r.errMsg = msg }

func matcher(t *testing.T, pattern string) func(string) (string, bool) {
	t.Helper()
	m, err := app.WaitForOutputParams{Pattern: pattern}.Matcher()
	if err != nil {
		t.Fatalf("matcher: %v", err)
	}
	return m
}

// A capture-check whose text contains the pattern resolves the waiter Matched:true
// with the matched line, clears the in-flight flag, and drops the waiter.
func TestWaiterMatchResolves(t *testing.T) {
	o := newWaiterHarness()
	rw := &recWaiter{}
	o.waiters[1] = []*waiter{{resp: rw, match: matcher(t, "READY")}}
	o.waiterCheck[1] = true

	o.onWaiterText(1, browserproto.CaptureResult{Text: "starting\nserver READY now\ntail"})

	if !rw.ok || !rw.res.Matched || rw.res.Text != "server READY now" {
		t.Fatalf("match: ok=%v res=%+v", rw.ok, rw.res)
	}
	if o.waiterCheck[1] {
		t.Fatalf("waiterCheck should be cleared after a check")
	}
	if _, ok := o.waiters[1]; ok {
		t.Fatalf("resolved waiter should be removed, have %v", o.waiters[1])
	}
}

// A raw pane_output chunk is stripped of VT escapes and matched against the pane's
// waiters: a colour-wrapped pattern resolves the waiter Matched:true with a clean
// line, and the last waiter's departure tears down the accumulator.
func TestWaiterMatchesStream(t *testing.T) {
	o := newWaiterHarness()
	rw := &recWaiter{}
	o.waiters[1] = []*waiter{{resp: rw, match: matcher(t, "READY")}}
	o.outAccum[1] = &outputScanner{}

	// Escape sequences wrap the pattern and split it from its line context.
	o.onPaneOutput(1, []byte("boot\r\n\x1b[32mserver READY\x1b[0m now\r\n"))

	if !rw.ok || !rw.res.Matched || rw.res.Text != "server READY now" {
		t.Fatalf("stream match: ok=%v res=%+v", rw.ok, rw.res)
	}
	if _, ok := o.waiters[1]; ok {
		t.Fatalf("resolved waiter should be removed, have %v", o.waiters[1])
	}
	if _, ok := o.outAccum[1]; ok {
		t.Fatalf("accumulator should be dropped when the last waiter goes")
	}
}

// A pattern split across two pane_output chunks still matches: the accumulator
// carries text between chunks (and the escape state machine between them).
func TestWaiterMatchesStreamAcrossChunks(t *testing.T) {
	o := newWaiterHarness()
	rw := &recWaiter{}
	o.waiters[1] = []*waiter{{resp: rw, match: matcher(t, "DEPLOYED")}}
	o.outAccum[1] = &outputScanner{}

	o.onPaneOutput(1, []byte("status: DEP"))
	if rw.ok {
		t.Fatalf("partial pattern should not match yet: %+v", rw.res)
	}
	o.onPaneOutput(1, []byte("LOYED\r\n"))
	if !rw.ok || !rw.res.Matched || rw.res.Text != "status: DEPLOYED" {
		t.Fatalf("cross-chunk match: ok=%v res=%+v", rw.ok, rw.res)
	}
}

// A pane_output chunk arriving after the last waiter resolved (accumulator already
// dropped) is ignored — the daemon's set_output_stream(false) races the tail of
// the stream.
func TestWaiterStreamAfterResolveIgnored(t *testing.T) {
	o := newWaiterHarness()
	o.onPaneOutput(7, []byte("late bytes")) // no waiter, no accumulator
	if len(o.waiters) != 0 || len(o.outAccum) != 0 {
		t.Fatalf("stream with no waiter should be a no-op, have %v / %v", o.waiters, o.outAccum)
	}
}

// A non-matching check leaves the waiter pending (only clears the in-flight flag)
// so the next frame retries.
func TestWaiterNoMatchKeepsWaiting(t *testing.T) {
	o := newWaiterHarness()
	rw := &recWaiter{}
	o.waiters[1] = []*waiter{{resp: rw, match: matcher(t, "READY")}}
	o.waiterCheck[1] = true

	o.onWaiterText(1, browserproto.CaptureResult{Text: "still booting"})

	if rw.ok || rw.fail {
		t.Fatalf("no match should not resolve: ok=%v fail=%v", rw.ok, rw.fail)
	}
	if len(o.waiters[1]) != 1 {
		t.Fatalf("waiter should remain pending, have %v", o.waiters[1])
	}
	if o.waiterCheck[1] {
		t.Fatalf("in-flight flag should clear so the next frame retries")
	}
}

// Two waiters on one pane resolve independently: a check that satisfies one leaves
// the other pending.
func TestWaiterMultiplePerPane(t *testing.T) {
	o := newWaiterHarness()
	first, second := &recWaiter{}, &recWaiter{}
	o.waiters[1] = []*waiter{
		{resp: first, match: matcher(t, "one")},
		{resp: second, match: matcher(t, "two")},
	}
	o.onWaiterText(1, browserproto.CaptureResult{Text: "phase one complete"})
	if !first.ok || first.fail {
		t.Fatalf("first waiter should resolve: %+v", first)
	}
	if second.ok || second.fail {
		t.Fatalf("second waiter should still wait: %+v", second)
	}
	if len(o.waiters[1]) != 1 || o.waiters[1][0].resp != second {
		t.Fatalf("only the unmatched waiter should remain, have %v", o.waiters[1])
	}
}

// A pane exit resolves any still-pending waiters Matched:false (no more output).
func TestWaiterExitResolvesUnmatched(t *testing.T) {
	o := newWaiterHarness()
	rw := &recWaiter{}
	o.waiters[2] = []*waiter{{resp: rw, match: matcher(t, "never")}}

	o.resolveWaitersOnExit(2)

	if !rw.ok || rw.res.Matched {
		t.Fatalf("exit should resolve Matched:false, ok=%v res=%+v", rw.ok, rw.res)
	}
	if _, ok := o.waiters[2]; ok {
		t.Fatalf("pane's waiter entry should be gone")
	}
}

// A daemon drop fails every active waiter (an infra error, not a non-match) and
// clears the maps.
func TestWaiterFlushFails(t *testing.T) {
	o := newWaiterHarness()
	rw := &recWaiter{}
	o.waiters[3] = []*waiter{{resp: rw, match: matcher(t, "x")}}
	o.waiterCheck[3] = true

	o.flushWaiters("termhost connection lost")

	if !rw.fail || rw.errMsg != "termhost connection lost" {
		t.Fatalf("flush should Fail: fail=%v msg=%q", rw.fail, rw.errMsg)
	}
	if len(o.waiters) != 0 || len(o.waiterCheck) != 0 {
		t.Fatalf("flush should clear waiters/waiterCheck, have %v / %v", o.waiters, o.waiterCheck)
	}
}

// recSub records the events a control-API subscriber is Send()t (name + payload);
// full simulates a slow/backed-up reader (Send returns false).
type recSub struct {
	names []string
	datas []any
	full  bool
}

func (r *recSub) Send(event string, data any) bool {
	if r.full {
		return false
	}
	r.names = append(r.names, event)
	r.datas = append(r.datas, data)
	return true
}

// findRef returns the payload of the first event named name, failing if none.
func findRef(t *testing.T, sub *recSub, name string) app.PaneRefEvent {
	t.Helper()
	for i, n := range sub.names {
		if n == name {
			ref, ok := sub.datas[i].(app.PaneRefEvent)
			if !ok {
				t.Fatalf("%s payload is %T, want app.PaneRefEvent", name, sub.datas[i])
			}
			return ref
		}
	}
	t.Fatalf("no %s event emitted; got %v", name, sub.names)
	return app.PaneRefEvent{}
}

// emitEvent honours each subscriber's pane/event filter.
func TestEmitEventFilters(t *testing.T) {
	o := newWaiterHarness()
	all := &recSub{}
	p1 := uint32(1)
	pane1 := &recSub{}
	agentOnly := &recSub{}
	o.subs[&ctlSubscriber{sub: all}] = struct{}{}
	o.subs[&ctlSubscriber{sub: pane1, filter: app.EventsSubscribeParams{Pane: &p1}}] = struct{}{}
	o.subs[&ctlSubscriber{sub: agentOnly, filter: app.EventsSubscribeParams{Events: []string{app.EventPaneAgent}}}] = struct{}{}

	o.emitEvent(app.EventPaneExited, 2, app.PaneExitedEvent{Pane: 2})
	o.emitEvent(app.EventPaneAgent, 1, app.PaneAgentEvent{Pane: 1, Agent: "claude", State: "working"})

	if len(all.names) != 2 {
		t.Fatalf("unfiltered subscriber got %v, want both events", all.names)
	}
	if len(pane1.names) != 1 || pane1.names[0] != app.EventPaneAgent {
		t.Fatalf("pane filter got %v, want only the pane-1 agent event", pane1.names)
	}
	if len(agentOnly.names) != 1 || agentOnly.names[0] != app.EventPaneAgent {
		t.Fatalf("event filter got %v, want only pane_agent", agentOnly.names)
	}
}

// A subscriber that can't keep up (Send false) is dropped.
func TestEmitEventDropsSlowSubscriber(t *testing.T) {
	o := newWaiterHarness()
	o.subs[&ctlSubscriber{sub: &recSub{full: true}}] = struct{}{}
	o.emitEvent(app.EventPaneExited, 1, app.PaneExitedEvent{Pane: 1})
	if len(o.subs) != 0 {
		t.Fatalf("slow subscriber should be dropped, have %d", len(o.subs))
	}
}

// Structural events are derived by diffing the model: a split adds a pane,
// re-focusing emits focus_changed, and a close removes it. The pre-existing pane
// (seeded in newOrch) is never re-announced. Uses a real session over a bare orch
// — the daemon socket never connects (sends drop), which is fine here.
func TestStructuralEvents(t *testing.T) {
	o, err := newOrch(filepath.Join(t.TempDir(), "s.sock"), t.TempDir())
	if err != nil {
		t.Fatalf("newOrch: %v", err)
	}
	sub := &recSub{}
	o.subs[&ctlSubscriber{sub: sub}] = struct{}{}

	initial := map[uint32]bool{}
	for _, id := range o.session.AllPaneIDs() {
		initial[uint32(id)] = true
	}

	// Split → pane_added for the new pane only.
	if _, err := o.session.SplitPane(nil, layout.Horizontal); err != nil {
		t.Fatalf("split: %v", err)
	}
	o.emitStructuralEvents()
	added := findRef(t, sub, app.EventPaneAdded)
	if initial[added.Pane] {
		t.Fatalf("pane_added should name the new pane, got pre-existing %d", added.Pane)
	}
	if added.Handle == "" {
		t.Fatalf("pane_added should carry a public handle, got empty")
	}
	for i, n := range sub.names {
		if n == app.EventPaneAdded && sub.datas[i].(app.PaneRefEvent).Pane != added.Pane {
			t.Fatalf("a pre-existing pane was re-announced: %v", sub.datas[i])
		}
	}

	// Focus a pane other than whichever the split left focused → focus_changed.
	var other uint32
	cur := o.focusedPaneID()
	for _, id := range o.session.AllPaneIDs() {
		if uint32(id) != cur {
			other = uint32(id)
		}
	}
	sub.names, sub.datas = nil, nil
	if err := o.session.FocusPane(layout.PaneID(other)); err != nil {
		t.Fatalf("focus: %v", err)
	}
	o.emitStructuralEvents()
	if foc := findRef(t, sub, app.EventFocusChanged); foc.Pane != other {
		t.Fatalf("focus_changed should name %d, got %d", other, foc.Pane)
	}

	// Close the new pane → pane_removed for it (with the handle it last had).
	sub.names, sub.datas = nil, nil
	pid := layout.PaneID(added.Pane)
	if _, err := o.session.ClosePane(&pid); err != nil {
		t.Fatalf("close: %v", err)
	}
	o.emitStructuralEvents()
	if rem := findRef(t, sub, app.EventPaneRemoved); rem.Pane != added.Pane || rem.Handle == "" {
		t.Fatalf("pane_removed should name %d with a handle, got %+v", added.Pane, rem)
	}
}
