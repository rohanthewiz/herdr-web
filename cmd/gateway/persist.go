//go:build ghostty

package main

import (
	"log"
	"time"

	"github.com/rohanthewiz/herdr-web/internal/browserproto"
	"github.com/rohanthewiz/herdr-web/internal/orchestration"
	"github.com/rohanthewiz/herdr-web/internal/persist"
	"github.com/rohanthewiz/herdr-web/internal/terminal"
)

// Session persistence & restore (WS3) — the orchestrator side. Two state files
// with different rhythms:
//
//   - session.json: the model snapshot (tree + ratios + focus + names + public
//     numbering) plus per-pane cwds. Small; written on every model mutation,
//     debounced. Restored at startup in place of the fresh 1-pane session.
//   - history.json: the latest ANSI scrollback capture per pane. Larger;
//     written by a periodic activity-gated sweep and a bounded final capture at
//     clean shutdown. Consumed only on a cold start (the daemon no longer holds
//     the pane): create_pane.initial_history replays it into the fresh
//     emulator, the analogue of herdr's seed_history_ansi.
//
// A gateway restart against a live daemon needs none of the history — the
// PTYs survive in the daemon and reconcile adopts them; the model snapshot
// alone restores the tree around them. History exists for the case where the
// daemon is gone too (first run, daemon crash, reboot).
//
// Everything here runs on the orchestrator loop goroutine, except the ticker
// goroutine (runHistoryCapture), which only posts closures onto it.

const (
	// saveDebounce coalesces bursts of model mutations (a mouse-drag resize
	// emits one per movement) into one session.json write.
	saveDebounce = 500 * time.Millisecond
	// histDebounce coalesces a capture sweep's per-pane replies into one
	// history.json write.
	histDebounce = time.Second
	// histCaptureInterval paces the periodic scrollback sweep. It bounds the
	// staleness of cold-restore seeds when nothing shuts down cleanly (a daemon
	// crash loses at most this much history).
	histCaptureInterval = 60 * time.Second
	// finalCaptureTimeout bounds how long a clean shutdown waits for the final
	// capture sweep before writing what it has and exiting anyway.
	finalCaptureTimeout = time.Second
)

// --- model snapshot (session.json) -------------------------------------------

// saveSoon arms the debounced session save. No-op when persistence is off or a
// save is already pending.
func (o *orch) saveSoon() {
	if o.sessionPath == "" || o.saveArmed {
		return
	}
	o.saveArmed = true
	time.AfterFunc(saveDebounce, func() {
		o.post(func() {
			o.saveArmed = false
			o.saveNow()
		})
	})
}

// saveNow writes the session snapshot synchronously (loop goroutine; the file
// is small and local). Pane cwds and agent-session refs merge the live values
// over any not-yet-respawned restored ones, so a restart before the daemon
// reconnects doesn't lose them.
func (o *orch) saveNow() {
	if o.sessionPath == "" {
		return
	}
	cwds := make(map[uint32]string)
	for pid, cwd := range o.restoredCwds {
		if o.panes[pid] != nil {
			cwds[pid] = cwd
		}
	}
	agents := make(map[uint32]persist.AgentSession)
	for pid, s := range o.restoredAgents {
		if o.panes[pid] != nil {
			agents[pid] = s
		}
	}
	for pid, rt := range o.panes {
		if rt.cwd != "" {
			cwds[pid] = rt.cwd
		}
		if ref := rt.agentSession; ref != nil {
			agents[pid] = persist.AgentSession{Source: ref.source, Agent: ref.agent, Kind: ref.kind, Value: ref.value}
		}
	}
	if err := persist.SaveSession(o.sessionPath, o.session.Snapshot(), cwds, agents); err != nil {
		log.Printf("gateway: save session state: %v", err)
	}
}

// --- scrollback capture (history.json) ---------------------------------------

// histResponder receives one pane's ANSI capture reply (via the shared pending
// machinery — the daemon replies FIFO per (pane, kind), so waiter checks,
// pane.capture commands, and history captures interleave safely). final marks a
// shutdown-sweep request, which counts down the final-capture barrier.
type histResponder struct {
	o     *orch
	pane  uint32
	final bool
}

func (histResponder) WantsReply() bool { return true }

func (r histResponder) OK(data any) {
	if cr, ok := data.(browserproto.CaptureResult); ok && cr.Text != "" {
		r.o.capturedHist[r.pane] = cr.Text
		r.o.histSaveSoon()
	}
	if r.final {
		r.o.noteFinalCaptureDone()
	}
}

func (r histResponder) Fail(string) {
	if rt := r.o.panes[r.pane]; rt != nil {
		rt.histDirty = true // retry on the next sweep
	}
	if r.final {
		r.o.noteFinalCaptureDone()
	}
}

// runHistoryCapture is the periodic sweep pacer (own goroutine, started by main
// when persistence is on): it only posts onto the loop.
func (o *orch) runHistoryCapture() {
	t := time.NewTicker(histCaptureInterval)
	defer t.Stop()
	for range t.C {
		o.post(func() { o.captureHistory(false) })
	}
}

// captureHistory issues one ANSI capture per pane worth capturing — live,
// spawned, and (for the periodic sweep) with output since the last capture.
// Returns how many requests were issued. Loop goroutine.
func (o *orch) captureHistory(final bool) int {
	if o.historyPath == "" || !o.daemon.connected() {
		return 0
	}
	n := 0
	for pid, rt := range o.panes {
		if rt.exited != nil || !rt.created {
			continue
		}
		if !final && !rt.histDirty {
			continue
		}
		rt.histDirty = false
		o.registerPending(histResponder{o: o, pane: pid, final: final}, reqKey{pid, reqText})
		o.daemon.send(orchestration.NewRequestText(pid, uint8(terminal.TextRecent), o.histLines, true, false))
		n++
	}
	return n
}

// histSaveSoon arms the debounced history write.
func (o *orch) histSaveSoon() {
	if o.historyPath == "" || o.histArmed || o.finalCap != nil {
		return // a running final capture writes the file itself
	}
	o.histArmed = true
	time.AfterFunc(histDebounce, func() {
		o.post(func() {
			o.histArmed = false
			o.histSaveNow()
		})
	})
}

// histSaveNow writes the history file, pruned to panes still in the model, so
// closed panes' scrollback ages out of disk.
func (o *orch) histSaveNow() {
	if o.historyPath == "" {
		return
	}
	out := make(map[uint32]string)
	for _, id := range o.session.AllPaneIDs() {
		if h, ok := o.capturedHist[uint32(id)]; ok {
			out[uint32(id)] = h
		}
	}
	if err := persist.SaveHistory(o.historyPath, out); err != nil {
		log.Printf("gateway: save history state: %v", err)
	}
}

// --- clean-shutdown final capture --------------------------------------------

// finalCapture tracks the bounded capture sweep a clean shutdown runs before
// the process exits: remaining outstanding replies, and the continuation to
// fire once (all replies in, or the timeout).
type finalCapture struct {
	remaining int
	done      func()
	timer     *time.Timer
}

// beginFinalCapture starts the shutdown sweep and calls done when it completes
// or times out. With persistence off, no daemon, or no capturable panes, done
// fires synchronously (after flushing whatever history is already in memory).
func (o *orch) beginFinalCapture(done func()) {
	if o.historyPath == "" {
		done()
		return
	}
	fc := &finalCapture{done: done}
	o.finalCap = fc
	n := o.captureHistory(true)
	if n == 0 {
		o.finishFinalCapture()
		return
	}
	// Replies land as posted closures, so they cannot race this assignment —
	// the loop goroutine is still inside this one.
	fc.remaining = n
	fc.timer = time.AfterFunc(finalCaptureTimeout, func() {
		o.post(func() { o.finishFinalCapture() })
	})
}

// noteFinalCaptureDone counts down one shutdown-sweep reply.
func (o *orch) noteFinalCaptureDone() {
	fc := o.finalCap
	if fc == nil {
		return
	}
	fc.remaining--
	if fc.remaining <= 0 {
		o.finishFinalCapture()
	}
}

// finishFinalCapture writes the history file once and fires the continuation.
// Idempotent via the nil check (reply-complete races the timeout).
func (o *orch) finishFinalCapture() {
	fc := o.finalCap
	if fc == nil {
		return
	}
	o.finalCap = nil
	if fc.timer != nil {
		fc.timer.Stop()
	}
	o.histSaveNow()
	fc.done()
}
