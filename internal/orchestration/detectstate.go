package orchestration

// Stage C — driver parity. The per-pane detection loop (detectPump in host.go)
// classifies an agent's state on a timer. A naive "emit on every change" loop
// flickers: agents repaint spinners, transient blank frames read as idle, and
// startup splash screens look like work. This file ports herdr's flicker-smoothing
// state machine (Rust: src/pane/agent_detection.rs) so the Go backend publishes the
// same debounced signal:
//
//   - startup grace: a freshly-acquired agent is held at Idle for a window before
//     its screen is scanned, so startup paint doesn't register as Working.
//   - pending-idle debounce: a Working→plain-Idle drop must survive several
//     confirmations (capped in time) before it publishes, so a momentary blank
//     between spinner frames doesn't bounce the state to Idle and back.
//   - content-change skip: while Idle with no new PTY bytes, the screen scan is
//     skipped entirely (cheap no-op).
//   - stable-signal refresh: a persistent visible blocker is periodically
//     re-published so a downstream consumer that missed the edge still learns of it.
//
// These helpers are pure (no ghostty build tag) so they unit-test without the
// emulator toolchain, mirroring the detect package.

import (
	"time"

	"github.com/rohanthewiz/herdr-web/internal/detect"
)

// Tuning, matched to herdr's pane/agent_detection.rs constants.
const (
	// detectInterval is the base cadence for probing a pane's foreground agent
	// and (when due) scanning its screen.
	detectInterval = 300 * time.Millisecond
	// detectPendingIdleRecheck is the faster cadence used while a Working→Idle
	// transition is being confirmed, so the debounce resolves quickly.
	detectPendingIdleRecheck = 100 * time.Millisecond
	// detectPendingIdleCap bounds how long a pending Working→Idle may be held
	// before it publishes regardless of confirmation count.
	detectPendingIdleCap = 700 * time.Millisecond
	// detectPendingIdleConfirmations is how many recheck ticks a Working→Idle
	// must persist before it publishes.
	detectPendingIdleConfirmations = 3
	// detectStableVisibleRefresh is how often a steady visible blocker is
	// re-emitted even without an edge.
	detectStableVisibleRefresh = 800 * time.Millisecond
	// detectStartupGrace is how long a newly-acquired agent is pinned to Idle
	// before its screen is first scanned.
	detectStartupGrace = 3 * time.Second
)

// publishState is the quartet that decides whether a pane_agent event is worth
// emitting: the agent's state plus its three "visible signal" flags. detect.Detect
// already gates each visible flag by state (e.g. VisibleBlocker is only set when
// state == Blocked), so these are taken verbatim from its result.
type publishState struct {
	state          detect.State
	visibleIdle    bool
	visibleBlocker bool
	visibleWorking bool
}

// pendingIdle debounces the Working→plain-Idle transition (the one prone to
// flicker between spinner frames). A "plain" idle is Idle with no visible-idle
// marker on screen — i.e. inferred absence of work, not a positive idle signal.
// Ports herdr's PendingIdleConfirmation.
type pendingIdle struct {
	started       bool
	startedAt     time.Time
	confirmations int
}

func (p *pendingIdle) active() bool { return p.started }

func (p *pendingIdle) clear() {
	p.started = false
	p.startedAt = time.Time{}
	p.confirmations = 0
}

// shouldHoldWorkingToIdle reports whether this tick's Working→Idle drop should be
// withheld from publishing. It returns true (hold) until the transition has been
// confirmed detectPendingIdleConfirmations times or detectPendingIdleCap elapses,
// whichever comes first; any non-matching transition clears the pending state.
func (p *pendingIdle) shouldHoldWorkingToIdle(prev, next publishState, agentChanged, processExited bool, now time.Time) bool {
	isWorkingToPlainIdle := prev.state == detect.StateWorking &&
		next.state == detect.StateIdle &&
		!next.visibleIdle &&
		!next.visibleBlocker &&
		!agentChanged &&
		!processExited
	if !isWorkingToPlainIdle {
		p.clear()
		return false
	}
	if !p.started {
		p.started = true
		p.startedAt = now
		p.confirmations = 0
		return true
	}
	if now.Sub(p.startedAt) >= detectPendingIdleCap {
		p.clear()
		return false
	}
	p.confirmations++
	if p.confirmations >= detectPendingIdleConfirmations {
		p.clear()
		return false
	}
	return true
}

// shouldSkipIdleScreenScan reports whether the (relatively expensive) screen
// snapshot+scan can be skipped this tick: only when we're sitting in Idle with an
// agent present, no transition in flight, and the PTY content sequence is
// unchanged since the last scan. Ports herdr's should_skip_idle_screen_scan.
func shouldSkipIdleScreenScan(state detect.State, agentPresent, pendingActive, agentChanged, processExited bool, currentSeq, lastSeq uint64, hasLastSeq bool) bool {
	if state != detect.StateIdle || !agentPresent || pendingActive || agentChanged || processExited {
		return false
	}
	return hasLastSeq && lastSeq == currentSeq
}

// stableVisibleSignalRefreshDue reports whether a steady visible blocker (present
// both before and after this tick) is due for a refresh re-emit.
// Ports herdr's stable_visible_signal_refresh_due.
func stableVisibleSignalRefreshDue(prev, next publishState, lastRefresh time.Time, hasRefresh bool, now time.Time) bool {
	stable := next.visibleBlocker && prev.visibleBlocker
	if !stable {
		return false
	}
	return !hasRefresh || now.Sub(lastRefresh) >= detectStableVisibleRefresh
}

// shouldPublishDetectionUpdate reports whether next differs from prev in any way
// worth emitting. Ports herdr's should_publish_detection_update.
func shouldPublishDetectionUpdate(prev, next publishState, agentChanged, processExited, stableRefreshDue bool) bool {
	return next.state != prev.state ||
		next.visibleIdle != prev.visibleIdle ||
		next.visibleBlocker != prev.visibleBlocker ||
		next.visibleWorking != prev.visibleWorking ||
		agentChanged ||
		processExited ||
		(stableRefreshDue && next.visibleBlocker && prev.visibleBlocker)
}

// decideDetectionTransition combines the pending-idle hold with the publish test:
// it returns true when next should be published now. It mutates pending as a side
// effect (advancing or clearing the confirmation count). Ports herdr's
// decide_detection_transition.
func decideDetectionTransition(prev, next publishState, agentChanged, processExited, stableRefreshDue bool, now time.Time, pending *pendingIdle) bool {
	if pending.shouldHoldWorkingToIdle(prev, next, agentChanged, processExited, now) {
		return false
	}
	return shouldPublishDetectionUpdate(prev, next, agentChanged, processExited, stableRefreshDue)
}
