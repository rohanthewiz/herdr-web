//go:build ghostty

package main

import (
	"strconv"
	"time"

	"github.com/rohanthewiz/herdr-web/internal/app"
	"github.com/rohanthewiz/herdr-web/internal/browserproto"
	"github.com/rohanthewiz/herdr-web/internal/layout"
	"github.com/rohanthewiz/herdr-web/internal/orchestration"
)

// Agent notifications (WS6) — the port of herdr's agent-notification decisions
// (app/actions.rs). A pane_agent state transition that warrants attention
// becomes a browserproto notify (toast + permission-gated native notification
// in the front-end) and a pane_notify control-API event. Suppression is the
// front-end's job: unlike herdr, which knows its host terminal's focus, the
// gateway serves many browsers and each knows its own focus/visibility — so
// the server always sends and each client decides (herdr's rule: suppress when
// the pane is on screen and the window is focused). Loop-goroutine only.

// onPaneAgent caches the daemon's detection result for a pane and republishes
// the arbitrated agent state. Detection is one of two inputs — the hook-report
// API (hooks.go) is the other — so the shared publish path below owns the
// broadcast/notify decisions for both.
func (o *orch) onPaneAgent(ev orchestration.PaneAgent) {
	rt := o.panes[ev.PaneID]
	if rt == nil {
		return
	}
	// A resumable session ref is dropped when detection contradicts it
	// (herdr's set_detected_state rules): a different agent is on screen, or
	// the ref's own agent just changed/disappeared — its conversation ended,
	// so it must not resume on restore.
	if sess := rt.agentSession; sess != nil {
		prev := ""
		if rt.agent != nil {
			prev = rt.agent.Agent
		}
		if (ev.Agent != "" && ev.Agent != sess.agent) || (ev.Agent != prev && prev == sess.agent) {
			rt.agentSession = nil
			o.noteSessionRefChanged(rt)
		}
	}
	rt.agent = &ev
	rt.agentAt = time.Now()
	// A hook authority whose agent conflicts with a newly detected agent is
	// dropped (herdr: the detector is looking at the live screen; the hook
	// claim is stale).
	if rt.hook != nil && ev.Agent != "" && ev.Agent != rt.hook.agent {
		rt.hook = nil
	}
	o.publishAgent(rt)
}

// publishAgent forwards a pane's arbitrated agent state to browsers and event
// subscribers, and emits a notification on a notification-worthy transition.
// The previous *published* pair — not the raw detection — feeds the transition
// check, so hook-driven and detection-driven changes dedupe against each other.
func (o *orch) publishAgent(rt *paneRuntime) {
	agent, state := rt.effectiveAgent()
	prevState, prevAgent := rt.pubState, rt.pubAgent
	if prevState == "" {
		prevState = "unknown"
	}
	rt.pubAgent, rt.pubState = agent, state
	if o.visible[rt.id] {
		o.broadcast(browserproto.NewPaneAgent(rt.id, agent, state, true))
	}
	o.broadcast(o.agentsMsg())
	o.emitEvent(app.EventPaneAgent, rt.id, app.PaneAgentEvent{Pane: rt.id, Agent: agent, State: state})

	kind := notifyKind(prevState, prevAgent, state, agent)
	if kind == "" {
		return
	}
	msg := agent + " " + notifyEventText(kind)
	n := browserproto.NewNotify(kind, msg, o.notifyContext(rt.id))
	n.Pane = rt.id
	n.Pub, _ = o.session.PublicPaneID(layout.PaneID(rt.id))
	o.broadcast(n)
	o.emitEvent(app.EventPaneNotify, rt.id,
		app.PaneNotifyEvent{Pane: rt.id, Agent: agent, Kind: kind, Message: msg})
}

// notifyKind classifies an agent state transition (herdr's
// notification_toast_for_state_change_with_agent_labels):
//   - any change into blocked ⇒ "attention" — the agent is waiting on the user;
//   - a completion into idle ⇒ "finished" — from working/blocked, or from
//     unknown when it is the same agent (detection briefly lost it mid-run).
//
// A pane with no detected agent never notifies, and an unchanged state is
// never a transition (resync replays are deduped by this).
func notifyKind(prevState, prevAgent, state, agent string) string {
	if agent == "" || state == prevState {
		return ""
	}
	switch {
	case state == "blocked":
		return "attention"
	case state == "idle" && (prevState == "working" || prevState == "blocked"):
		return "finished"
	case state == "idle" && prevState == "unknown" && prevAgent != "" && prevAgent == agent:
		return "finished"
	}
	return ""
}

// notifyEventText is the human phrase for a notification kind.
func notifyEventText(kind string) string {
	if kind == "attention" {
		return "needs attention"
	}
	return "finished"
}

// notifyContext locates a pane and renders herdr's notification context:
// "workspace · N", plus "· tab" when the workspace has more than one tab.
func (o *orch) notifyContext(pid uint32) string {
	for i, ws := range o.session.Workspaces() {
		tabIdx, ok := ws.FindTabIndexForPane(layout.PaneID(pid))
		if !ok {
			continue
		}
		ctx := ws.DisplayName() + " · " + strconv.Itoa(i+1)
		if len(ws.Tabs) > 1 {
			ctx += " · " + ws.Tabs[tabIdx].DisplayName()
		}
		return ctx
	}
	return ""
}
