package app

import "slices"

// This file is the control-API streaming-event vocabulary (events.subscribe): the
// event names and their payload shapes the control API pushes to a subscribed
// client, plus the subscription filter. It lives in internal/app alongside the §7
// command vocabulary so the event contract has one home, but unlike the unary
// commands these are not routed through Dispatcher — the streaming transport
// (internal/ctlproto) carries them and the orchestrator (cmd/gateway2) emits them.
// They mirror the pane lifecycle/chrome the orchestrator already observes from the
// terminal backend, flattened into an automation-friendly JSON stream (herdrctl
// prints one per line); the browser's own richer down-messages live in browserproto.
//
// The Pane field on every event is the internal pane id — the same id every other
// §7 command addresses a pane by (get it from pane.list), so a subscriber can act
// on the pane an event names.

// Event names (events.subscribe). A subscription with no Events filter receives
// all of them.
const (
	EventPaneExited = "pane_exited" // the pane's child process exited
	EventPaneAgent  = "pane_agent"  // detected agent identity/state changed
	EventPaneTitle  = "pane_title"  // the program set the pane's title (OSC 0/2)
	EventPaneCwd    = "pane_cwd"    // the pane's working directory changed (OSC 7)
)

// EventNames returns every event name events.subscribe can emit, in a stable
// order — the vocabulary a client validates an Events filter against.
func EventNames() []string {
	return []string{EventPaneExited, EventPaneAgent, EventPaneTitle, EventPaneCwd}
}

// PaneExitedEvent is the payload for EventPaneExited.
type PaneExitedEvent struct {
	Pane     uint32 `json:"pane"`
	ExitCode int    `json:"exit_code"`
}

// PaneAgentEvent is the payload for EventPaneAgent. Agent is "" for a plain shell;
// State is one of idle|working|blocked|unknown.
type PaneAgentEvent struct {
	Pane  uint32 `json:"pane"`
	Agent string `json:"agent"`
	State string `json:"state"`
}

// PaneTitleEvent is the payload for EventPaneTitle. Title is "" on a title-clear.
type PaneTitleEvent struct {
	Pane  uint32 `json:"pane"`
	Title string `json:"title"`
}

// PaneCwdEvent is the payload for EventPaneCwd.
type PaneCwdEvent struct {
	Pane uint32 `json:"pane"`
	Cwd  string `json:"cwd"`
}

// EventsSubscribeParams is the params object for events.subscribe. Both fields are
// optional: an absent Pane matches every pane, an empty Events matches every event
// name. The orchestrator applies the filter server-side so a narrow subscription
// only carries the frames it wants.
type EventsSubscribeParams struct {
	Pane   *uint32  `json:"pane,omitempty"`
	Events []string `json:"events,omitempty"`
}

// Match reports whether an event of the given name for the given pane passes this
// filter.
func (f EventsSubscribeParams) Match(event string, pane uint32) bool {
	if f.Pane != nil && *f.Pane != pane {
		return false
	}
	if len(f.Events) > 0 && !slices.Contains(f.Events, event) {
		return false
	}
	return true
}
