//go:build ghostty

package main

import (
	"encoding/json"
	"testing"

	"github.com/rohanthewiz/herdr-web/internal/browserproto"
)

// The read command is the only §7 command that round-trips through the daemon:
// handleCmd sends a RequestSelection and returns without replying, and the
// pane_selection reply — or a timeout / daemon drop — later resolves the
// browser's cmd_result. These tests drive that resolution logic directly
// (resolveRead / timeoutRead / flushPendingReads) against a bare orch and a fake
// client, so they need no daemon.

func newReadHarness() (*orch, *client) {
	o := &orch{
		conns:        map[*client]struct{}{},
		pendingReads: map[uint32][]*pendingRead{},
	}
	c := &client{o: o, out: make(chan []byte, 8)}
	o.conns[c] = struct{}{}
	return o, c
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

func readText(t *testing.T, r *browserproto.CmdResult) string {
	t.Helper()
	var rr browserproto.ReadResult
	if len(r.Data) > 0 {
		if err := json.Unmarshal(r.Data, &rr); err != nil {
			t.Fatalf("bad read result data: %v", err)
		}
	}
	return rr.Text
}

// resolveRead completes the oldest pending read for a pane, in FIFO order — the
// daemon replies to requests in the order it received them, and pane_selection
// carries no command id to match on.
func TestResolveReadFIFO(t *testing.T) {
	o, c := newReadHarness()
	o.pendingReads[7] = []*pendingRead{{c: c, id: "A"}, {c: c, id: "B"}}

	o.resolveRead(7, "first")
	if r := recvResult(t, c); r.ID != "A" || !r.Ok || readText(t, r) != "first" {
		t.Fatalf("first resolve: got id=%q ok=%v text=%q", r.ID, r.Ok, readText(t, r))
	}
	o.resolveRead(7, "second")
	if r := recvResult(t, c); r.ID != "B" || readText(t, r) != "second" {
		t.Fatalf("second resolve: got id=%q text=%q", r.ID, readText(t, r))
	}
	if _, ok := o.pendingReads[7]; ok {
		t.Fatalf("pane queue should be gone once drained, have %v", o.pendingReads[7])
	}
	// A pane_selection with nothing outstanding is dropped, not a panic.
	o.resolveRead(7, "extra")
	if len(c.out) != 0 {
		t.Fatalf("resolve on empty queue should send nothing, queued %d", len(c.out))
	}
}

// Reads on different panes are independent queues; a reply for one pane leaves
// the other's pending.
func TestResolveReadPerPane(t *testing.T) {
	o, c := newReadHarness()
	o.pendingReads[1] = []*pendingRead{{c: c, id: "X"}}
	o.pendingReads[2] = []*pendingRead{{c: c, id: "Y"}}

	o.resolveRead(2, "two")
	if r := recvResult(t, c); r.ID != "Y" || readText(t, r) != "two" {
		t.Fatalf("pane 2 resolve: got id=%q text=%q", r.ID, readText(t, r))
	}
	if len(o.pendingReads[1]) != 1 {
		t.Fatalf("pane 1 should still be pending, have %v", o.pendingReads[1])
	}
	o.resolveRead(1, "one")
	if r := recvResult(t, c); r.ID != "X" || readText(t, r) != "one" {
		t.Fatalf("pane 1 resolve: got id=%q text=%q", r.ID, readText(t, r))
	}
}

// A timed-out read is removed by identity and fails with an error result; the
// FIFO continues with the surviving read, and a late reply for the timed-out
// entry no longer matches it.
func TestTimeoutRead(t *testing.T) {
	o, c := newReadHarness()
	prA := &pendingRead{c: c, id: "A"}
	prB := &pendingRead{c: c, id: "B"}
	o.pendingReads[5] = []*pendingRead{prA, prB}

	o.timeoutRead(5, prA)
	if r := recvResult(t, c); r.ID != "A" || r.Ok || r.Error == "" {
		t.Fatalf("timeout: got id=%q ok=%v err=%q", r.ID, r.Ok, r.Error)
	}
	if len(o.pendingReads[5]) != 1 || o.pendingReads[5][0] != prB {
		t.Fatalf("prB should be the sole survivor, have %v", o.pendingReads[5])
	}
	o.resolveRead(5, "b-text")
	if r := recvResult(t, c); r.ID != "B" || readText(t, r) != "b-text" {
		t.Fatalf("survivor resolve: got id=%q text=%q", r.ID, readText(t, r))
	}
	// Timing out an already-resolved read is a harmless no-op.
	o.timeoutRead(5, prB)
	if len(c.out) != 0 {
		t.Fatalf("timeout of resolved read should send nothing, queued %d", len(c.out))
	}
}

// A dropped daemon connection fails every in-flight read so no browser cmd hangs.
func TestFlushPendingReads(t *testing.T) {
	o, c := newReadHarness()
	o.pendingReads[1] = []*pendingRead{{c: c, id: "a"}}
	o.pendingReads[2] = []*pendingRead{{c: c, id: "b"}, {c: c, id: "c"}}

	o.flushPendingReads("gone")

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
			t.Fatalf("read %q was not flushed", id)
		}
	}
	if len(o.pendingReads) != 0 {
		t.Fatalf("pendingReads should be empty after flush, have %v", o.pendingReads)
	}
}

// A read issued without a command id has no result channel, so nothing is sent.
func TestReplyReadNoID(t *testing.T) {
	o, c := newReadHarness()
	o.replyRead(&pendingRead{c: c, id: ""}, "ignored", "")
	if len(c.out) != 0 {
		t.Fatalf("id-less read should send nothing, queued %d", len(c.out))
	}
}
