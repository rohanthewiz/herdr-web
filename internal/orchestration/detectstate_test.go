package orchestration

import (
	"testing"
	"time"

	"github.com/rohanthewiz/herdr-web/internal/detect"
)

func ps(state detect.State) publishState { return publishState{state: state} }

// pending-idle holds a Working→plain-Idle drop for three rechecks, then releases.
func TestPendingIdleHoldsWorkingToPlainIdle(t *testing.T) {
	now := time.Now()
	prev := ps(detect.StateWorking)
	next := ps(detect.StateIdle)
	var p pendingIdle

	for i, at := range []time.Duration{0, detectPendingIdleRecheck, detectPendingIdleRecheck * 2} {
		if !p.shouldHoldWorkingToIdle(prev, next, false, false, now.Add(at)) {
			t.Fatalf("recheck %d: expected hold", i)
		}
		if !p.active() {
			t.Fatalf("recheck %d: expected pending active", i)
		}
	}
	if p.shouldHoldWorkingToIdle(prev, next, false, false, now.Add(detectPendingIdleRecheck*3)) {
		t.Fatal("4th recheck: expected release (publish), got hold")
	}
	if p.active() {
		t.Fatal("expected pending cleared after release")
	}
}

// The 700ms cap releases the hold even before the confirmation count is reached.
func TestPendingIdleCapReleases(t *testing.T) {
	now := time.Now()
	prev := ps(detect.StateWorking)
	next := ps(detect.StateIdle)
	var p pendingIdle

	if !p.shouldHoldWorkingToIdle(prev, next, false, false, now) {
		t.Fatal("first tick: expected hold")
	}
	if p.shouldHoldWorkingToIdle(prev, next, false, false, now.Add(detectPendingIdleCap)) {
		t.Fatal("at cap: expected release")
	}
}

// A visible-idle signal is a positive idle marker, not flicker — it bypasses the hold.
func TestVisibleIdleBypassesHold(t *testing.T) {
	now := time.Now()
	prev := ps(detect.StateWorking)
	next := publishState{state: detect.StateIdle, visibleIdle: true}
	var p pendingIdle

	if p.shouldHoldWorkingToIdle(prev, next, false, false, now) {
		t.Fatal("visible-idle should publish immediately")
	}
}

// agent change and process exit both bypass the hold.
func TestHoldBypassedByAgentChangeAndExit(t *testing.T) {
	now := time.Now()
	prev := ps(detect.StateWorking)
	next := ps(detect.StateIdle)

	var p1 pendingIdle
	if p1.shouldHoldWorkingToIdle(prev, next, true, false, now) {
		t.Fatal("agent change should bypass hold")
	}
	var p2 pendingIdle
	if p2.shouldHoldWorkingToIdle(prev, next, false, true, now) {
		t.Fatal("process exit should bypass hold")
	}
}

func TestShouldSkipIdleScreenScan(t *testing.T) {
	// Idle, agent present, no transition, unchanged seq → skip.
	if !shouldSkipIdleScreenScan(detect.StateIdle, true, false, false, false, 7, 7, true) {
		t.Fatal("steady idle with unchanged content should skip")
	}
	// Changed seq → read.
	if shouldSkipIdleScreenScan(detect.StateIdle, true, false, false, false, 8, 7, true) {
		t.Fatal("changed content should not skip")
	}
	// No prior scan → read.
	if shouldSkipIdleScreenScan(detect.StateIdle, true, false, false, false, 7, 0, false) {
		t.Fatal("missing last-scan seq should not skip")
	}
	// Non-idle / pending / agent-change / exit each force a read.
	if shouldSkipIdleScreenScan(detect.StateWorking, true, false, false, false, 7, 7, true) {
		t.Fatal("non-idle should not skip")
	}
	if shouldSkipIdleScreenScan(detect.StateIdle, false, false, false, false, 7, 7, true) {
		t.Fatal("no agent should not skip")
	}
	if shouldSkipIdleScreenScan(detect.StateIdle, true, true, false, false, 7, 7, true) {
		t.Fatal("pending-active should not skip")
	}
	if shouldSkipIdleScreenScan(detect.StateIdle, true, false, true, false, 7, 7, true) {
		t.Fatal("agent-change should not skip")
	}
	if shouldSkipIdleScreenScan(detect.StateIdle, true, false, false, true, 7, 7, true) {
		t.Fatal("process-exit should not skip")
	}
}

func TestShouldPublishDetectionUpdate(t *testing.T) {
	idle := ps(detect.StateIdle)
	working := ps(detect.StateWorking)

	if !shouldPublishDetectionUpdate(idle, working, false, false, false) {
		t.Fatal("state change should publish")
	}
	if shouldPublishDetectionUpdate(idle, idle, false, false, false) {
		t.Fatal("no change should not publish")
	}
	if !shouldPublishDetectionUpdate(idle, idle, true, false, false) {
		t.Fatal("agent change should publish")
	}
	if !shouldPublishDetectionUpdate(idle, idle, false, true, false) {
		t.Fatal("process exit should publish")
	}
	// A steady visible blocker republishes only when the refresh is due.
	blocked := publishState{state: detect.StateBlocked, visibleBlocker: true}
	if shouldPublishDetectionUpdate(blocked, blocked, false, false, false) {
		t.Fatal("steady blocker without refresh-due should not publish")
	}
	if !shouldPublishDetectionUpdate(blocked, blocked, false, false, true) {
		t.Fatal("steady blocker with refresh-due should publish")
	}
}

func TestStableVisibleSignalRefreshDue(t *testing.T) {
	now := time.Now()
	blocked := publishState{state: detect.StateBlocked, visibleBlocker: true}

	// Not stable across the edge → never due.
	if stableVisibleSignalRefreshDue(ps(detect.StateIdle), blocked, time.Time{}, false, now) {
		t.Fatal("fresh blocker (not stable) should not be refresh-due")
	}
	// Stable, never refreshed → due.
	if !stableVisibleSignalRefreshDue(blocked, blocked, time.Time{}, false, now) {
		t.Fatal("stable blocker never refreshed should be due")
	}
	// Stable, refreshed recently → not due.
	if stableVisibleSignalRefreshDue(blocked, blocked, now, true, now.Add(detectStableVisibleRefresh/2)) {
		t.Fatal("recently refreshed should not be due")
	}
	// Stable, refreshed long ago → due.
	if !stableVisibleSignalRefreshDue(blocked, blocked, now, true, now.Add(detectStableVisibleRefresh)) {
		t.Fatal("stale refresh should be due")
	}
}

// decideDetectionTransition composes the hold and the publish test: a flickering
// Working→Idle is withheld, then published once the debounce resolves.
func TestDecideDetectionTransition(t *testing.T) {
	now := time.Now()
	working := ps(detect.StateWorking)
	idle := ps(detect.StateIdle)
	var p pendingIdle

	if decideDetectionTransition(working, idle, false, false, false, now, &p) {
		t.Fatal("first Working→Idle should be held, not published")
	}
	// Drive past the confirmations.
	decideDetectionTransition(working, idle, false, false, false, now.Add(detectPendingIdleRecheck), &p)
	decideDetectionTransition(working, idle, false, false, false, now.Add(detectPendingIdleRecheck*2), &p)
	if !decideDetectionTransition(working, idle, false, false, false, now.Add(detectPendingIdleRecheck*3), &p) {
		t.Fatal("Working→Idle should publish after confirmations")
	}

	// A clear state change (Idle→Blocked) publishes immediately.
	blocked := publishState{state: detect.StateBlocked, visibleBlocker: true}
	if !decideDetectionTransition(idle, blocked, false, false, false, now, &p) {
		t.Fatal("Idle→Blocked should publish immediately")
	}
}
