//go:build ghostty

package main

import (
	"encoding/json"
	"testing"

	"github.com/rohanthewiz/herdr-web/internal/browserproto"
)

// read and capture are the only §7 commands that round-trip through the daemon:
// handleCmd sends a β request and returns without replying, and the matching
// reply — or a timeout / daemon drop — later resolves the browser's cmd_result.
// These tests drive that shared resolution logic directly (resolvePending /
// timeoutPending / flushPending) against a bare orch and a fake client, so they
// need no daemon.

func newPendingHarness() (*orch, *client) {
	o := &orch{
		conns:       map[*client]struct{}{},
		pendingReqs: map[reqKey][]*pending{},
	}
	c := &client{o: o, out: make(chan []byte, 8)}
	o.conns[c] = struct{}{}
	return o, c
}

// pend builds a pending whose Responder replies to client c under id — the same
// browserResponder the dispatch uses, so results land on c.out for recvResult.
func pend(o *orch, c *client, id string) *pending {
	return &pending{resp: browserResponder{o: o, c: c, id: id}}
}

// recvResult pops one queued cmd_result off the client, failing if none is there.
func recvResult(t *testing.T, c *client) *browserproto.CmdResult {
	t.Helper()
	select {
	case b := <-c.out:
		msg, err := browserproto.DecodeDown(b)
		if err != nil {
			t.Fatalf("decode down: %v", err)
		}
		r, ok := msg.(*browserproto.CmdResult)
		if !ok {
			t.Fatalf("want *CmdResult, got %T", msg)
		}
		return r
	default:
		t.Fatal("no cmd_result queued")
		return nil
	}
}

// resultText extracts the {text} payload common to ReadResult and CaptureResult.
func resultText(t *testing.T, r *browserproto.CmdResult) string {
	t.Helper()
	var rr struct {
		Text string `json:"text"`
	}
	if len(r.Data) > 0 {
		if err := json.Unmarshal(r.Data, &rr); err != nil {
			t.Fatalf("bad result data: %v", err)
		}
	}
	return rr.Text
}

// selKey/txtKey are the two round-trip queues for a pane.
func selKey(pane uint32) reqKey { return reqKey{pane, reqSelection} }
func txtKey(pane uint32) reqKey { return reqKey{pane, reqText} }

// resolvePending completes the oldest pending request for a (pane, kind), in FIFO
// order — the daemon replies to requests in the order it received them, and the
// reply carries no command id to match on.
func TestResolvePendingFIFO(t *testing.T) {
	o, c := newPendingHarness()
	o.pendingReqs[selKey(7)] = []*pending{pend(o, c, "A"), pend(o, c, "B")}

	o.resolvePending(selKey(7), browserproto.ReadResult{Text: "first"})
	if r := recvResult(t, c); r.ID != "A" || !r.Ok || resultText(t, r) != "first" {
		t.Fatalf("first resolve: got id=%q ok=%v text=%q", r.ID, r.Ok, resultText(t, r))
	}
	o.resolvePending(selKey(7), browserproto.ReadResult{Text: "second"})
	if r := recvResult(t, c); r.ID != "B" || resultText(t, r) != "second" {
		t.Fatalf("second resolve: got id=%q text=%q", r.ID, resultText(t, r))
	}
	if _, ok := o.pendingReqs[selKey(7)]; ok {
		t.Fatalf("pane queue should be gone once drained, have %v", o.pendingReqs[selKey(7)])
	}
	// A reply with nothing outstanding is dropped, not a panic.
	o.resolvePending(selKey(7), browserproto.ReadResult{Text: "extra"})
	if len(c.out) != 0 {
		t.Fatalf("resolve on empty queue should send nothing, queued %d", len(c.out))
	}
}

// Requests on different panes are independent queues; a reply for one pane leaves
// the other's pending.
func TestResolvePendingPerPane(t *testing.T) {
	o, c := newPendingHarness()
	o.pendingReqs[selKey(1)] = []*pending{pend(o, c, "X")}
	o.pendingReqs[selKey(2)] = []*pending{pend(o, c, "Y")}

	o.resolvePending(selKey(2), browserproto.ReadResult{Text: "two"})
	if r := recvResult(t, c); r.ID != "Y" || resultText(t, r) != "two" {
		t.Fatalf("pane 2 resolve: got id=%q text=%q", r.ID, resultText(t, r))
	}
	if len(o.pendingReqs[selKey(1)]) != 1 {
		t.Fatalf("pane 1 should still be pending, have %v", o.pendingReqs[selKey(1)])
	}
	o.resolvePending(selKey(1), browserproto.ReadResult{Text: "one"})
	if r := recvResult(t, c); r.ID != "X" || resultText(t, r) != "one" {
		t.Fatalf("pane 1 resolve: got id=%q text=%q", r.ID, resultText(t, r))
	}
}

// read and capture on the SAME pane are independent queues keyed by kind: a
// pane_selection reply must complete the read, a pane_text reply the capture, and
// neither must steal the other's cmd_result even though both are on one pane.
func TestResolvePendingPerKind(t *testing.T) {
	o, c := newPendingHarness()
	o.pendingReqs[selKey(3)] = []*pending{pend(o, c, "sel")}
	o.pendingReqs[txtKey(3)] = []*pending{pend(o, c, "txt")}

	// A pane_text reply resolves the capture, leaving the read pending.
	o.resolvePending(txtKey(3), browserproto.CaptureResult{Text: "buffer"})
	if r := recvResult(t, c); r.ID != "txt" || resultText(t, r) != "buffer" {
		t.Fatalf("capture resolve: got id=%q text=%q", r.ID, resultText(t, r))
	}
	if len(o.pendingReqs[selKey(3)]) != 1 {
		t.Fatalf("read should still be pending, have %v", o.pendingReqs[selKey(3)])
	}
	// The read's own reply resolves it.
	o.resolvePending(selKey(3), browserproto.ReadResult{Text: "selection"})
	if r := recvResult(t, c); r.ID != "sel" || resultText(t, r) != "selection" {
		t.Fatalf("read resolve: got id=%q text=%q", r.ID, resultText(t, r))
	}
}

// A timed-out request is removed by identity and fails with an error result; the
// FIFO continues with the survivor, and a late reply for the timed-out entry no
// longer matches it. The error text names the command (capture here).
func TestTimeoutPending(t *testing.T) {
	o, c := newPendingHarness()
	prA := pend(o, c, "A")
	prB := pend(o, c, "B")
	o.pendingReqs[txtKey(5)] = []*pending{prA, prB}

	o.timeoutPending(txtKey(5), prA)
	if r := recvResult(t, c); r.ID != "A" || r.Ok || r.Error != "capture timed out" {
		t.Fatalf("timeout: got id=%q ok=%v err=%q", r.ID, r.Ok, r.Error)
	}
	if len(o.pendingReqs[txtKey(5)]) != 1 || o.pendingReqs[txtKey(5)][0] != prB {
		t.Fatalf("prB should be the sole survivor, have %v", o.pendingReqs[txtKey(5)])
	}
	o.resolvePending(txtKey(5), browserproto.CaptureResult{Text: "b-text"})
	if r := recvResult(t, c); r.ID != "B" || resultText(t, r) != "b-text" {
		t.Fatalf("survivor resolve: got id=%q text=%q", r.ID, resultText(t, r))
	}
	// Timing out an already-resolved request is a harmless no-op.
	o.timeoutPending(txtKey(5), prB)
	if len(c.out) != 0 {
		t.Fatalf("timeout of resolved request should send nothing, queued %d", len(c.out))
	}
}

// A dropped daemon connection fails every in-flight request (both kinds) so no
// browser cmd hangs.
func TestFlushPending(t *testing.T) {
	o, c := newPendingHarness()
	o.pendingReqs[selKey(1)] = []*pending{pend(o, c, "a")}
	o.pendingReqs[txtKey(2)] = []*pending{pend(o, c, "b"), pend(o, c, "c")}

	o.flushPending("gone")

	got := map[string]bool{}
	for i := 0; i < 3; i++ {
		r := recvResult(t, c)
		if r.Ok || r.Error != "gone" {
			t.Fatalf("flush result %s: ok=%v err=%q", r.ID, r.Ok, r.Error)
		}
		got[r.ID] = true
	}
	for _, id := range []string{"a", "b", "c"} {
		if !got[id] {
			t.Fatalf("request %q was not flushed", id)
		}
	}
	if len(o.pendingReqs) != 0 {
		t.Fatalf("pendingReqs should be empty after flush, have %v", o.pendingReqs)
	}
}

// A request issued without a command id has no result channel, so nothing is sent.
func TestReplyPendingNoID(t *testing.T) {
	o, c := newPendingHarness()
	o.replyPending(pend(o, c, ""), browserproto.ReadResult{Text: "ignored"}, "")
	if len(c.out) != 0 {
		t.Fatalf("id-less request should send nothing, queued %d", len(c.out))
	}
}
