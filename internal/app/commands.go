package app

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/rohanthewiz/herdr-web/internal/layout"
)

// This file is the protocol-neutral §7 command dispatcher. It mutates the
// app.Session domain model and drives runtime effects through the Backend seam,
// replying to the caller through a Responder. gateway's orch implements Backend
// (browser WebSocket effects) and a Responder over one connection; a future
// CLI/control-API implements the same two interfaces differently. The dispatcher
// itself is libghostty-free and unit-testable with fakes.

// Backend is the runtime-effect seam the dispatcher drives. Every method runs on
// the caller's single actor-loop goroutine (the same one that owns the Session),
// so implementations need no locking.
type Backend interface {
	// Area is the current viewport grid; directional nav resolves against it.
	Area() layout.Rect

	// ApplyModel reconciles pane PTYs with the session and rebroadcasts the
	// viewport (layout + agents + newly-visible chrome/frames). Called after a
	// command that changed the pane set or sizes.
	ApplyModel()
	// BroadcastLayout rebroadcasts just the viewport layout — for commands that
	// moved focus or renamed without changing the pane set.
	BroadcastLayout()
	// BroadcastPaneTitle pushes a pane's effective title to observers if the pane
	// is currently on screen (else it rides the chrome resend when next visible).
	BroadcastPaneTitle(pane uint32)

	// ScrollPane passes a scrollback delta straight to the pane's PTY; it errors
	// if the pane is unknown.
	ScrollPane(pane uint32, delta int) error

	// PaneExists / DaemonConnected gate the async round-trip commands.
	PaneExists(pane uint32) bool
	DaemonConnected() bool
	// StartRead / StartCapture begin a daemon round-trip and resolve r when the
	// reply (or a timeout / disconnect) arrives — the dispatch returns first.
	StartRead(r Responder, p ReadParams)
	StartCapture(r Responder, p CaptureParams)
	// StartWaitForOutput registers a waiter that resolves r when the pane's output
	// matches p (WaitForOutputResult{Matched:true}), or on the wait's own timeout /
	// pane exit (Matched:false). The dispatch returns first; the waiter matches
	// against the pane's live output stream (plus a one-shot seed of the current
	// screen).
	StartWaitForOutput(r Responder, p WaitForOutputParams)

	// StartWorktreeList / StartWorktreeCreate / StartWorktreeOpen /
	// StartWorktreeRemove run the git-worktree commands (WS8 dialogs). The git
	// subprocess work happens off the loop goroutine and r resolves later;
	// worktree.open needs no git and may resolve synchronously. The backend owns
	// pane-cwd resolution, the worktree root, and the workspace effects
	// (create/focus/close + reconcile).
	StartWorktreeList(r Responder, p WorktreeListParams)
	StartWorktreeCreate(r Responder, p WorktreeCreateParams)
	StartWorktreeOpen(r Responder, p WorktreeOpenParams)
	StartWorktreeRemove(r Responder, p WorktreeRemoveParams)

	// ConfigGet resolves the live configuration snapshot (config.get); ConfigSet
	// validates, persists, and applies a change (config.set). Both resolve r
	// synchronously — the config file is small and local.
	ConfigGet(r Responder)
	ConfigSet(r Responder, p ConfigSetParams)

	// ReloadConfig acknowledges a config reload (a no-op today).
	ReloadConfig() error
	// Shutdown notifies observers the server is going away and triggers the quit.
	Shutdown()
}

// Responder delivers a command's terminal result to its caller. For the browser
// it marshals a cmd_result on that connection; a CLI/API caller implements its
// own. It is storable in a pending round-trip for the async commands.
type Responder interface {
	// WantsReply reports whether the caller can receive a result. read/capture
	// short-circuit when false, so they never register an unresolvable pending.
	WantsReply() bool
	// OK completes the command successfully; data is command-specific
	// (ReadResult/CaptureResult) or nil.
	OK(data any)
	// Fail completes the command with an error message.
	Fail(errMsg string)
}

// ParamDecoder decodes a command's params into the typed struct v. The browser
// backend wraps the Cmd's json params; a CLI could bind parsed flags. Decode
// returns ErrNoParams when the caller supplied none, so the dispatcher decides
// per command (required ⇒ error, optional ⇒ zero value).
type ParamDecoder interface {
	Decode(v any) error
}

// ErrNoParams signals that the caller supplied no params for a command.
var ErrNoParams = errors.New("missing params")

// JSONParamDecoder is the ParamDecoder for any JSON-params protocol — the
// browser cmd envelope and the control-API request both carry their params as a
// json.RawMessage. Empty params report ErrNoParams so the dispatcher can treat
// them as the zero value for optional commands, or an error for required ones.
type JSONParamDecoder struct{ Raw json.RawMessage }

func (d JSONParamDecoder) Decode(v any) error {
	if len(d.Raw) == 0 {
		return ErrNoParams
	}
	return json.Unmarshal(d.Raw, v)
}

// Dispatcher runs the §7 command table against a Session and a Backend. It
// borrows the same *Session the backend holds (single-goroutine, no locking).
type Dispatcher struct {
	session *Session
	backend Backend
}

// NewDispatcher builds a dispatcher over a session and its runtime backend.
func NewDispatcher(s *Session, b Backend) *Dispatcher {
	return &Dispatcher{session: s, backend: b}
}

// Dispatch runs one §7 command. Loop-goroutine only (it shares the session with
// the backend). Model-mutating commands call a Session method then reconcile via
// ApplyModel; pure focus/rename rebroadcast the layout; scroll passes through;
// read/capture start an async daemon round-trip; server.* are lifecycle.
func (d *Dispatcher) Dispatch(name string, dec ParamDecoder, r Responder) {
	// bad reports a malformed-params failure in the historical wording.
	bad := func(err error) { r.Fail("bad params: " + err.Error()) }

	switch name {
	case CmdPaneFocus:
		var p PaneParams
		if err := dec.Decode(&p); err != nil {
			bad(err)
			return
		}
		if err := d.session.FocusPane(layout.PaneID(p.Pane)); err != nil {
			r.Fail(err.Error())
			return
		}
		d.backend.BroadcastLayout() // focus flag moved; pane set unchanged
		r.OK(nil)

	case CmdPaneFocusDirection:
		var p DirParams
		if err := dec.Decode(&p); err != nil {
			bad(err)
			return
		}
		nav, ok := NavDirection(p.Dir)
		if !ok {
			r.Fail(fmt.Sprintf("bad direction %q", p.Dir))
			return
		}
		moved, err := d.session.FocusPaneDirection(nav, d.backend.Area())
		if err != nil {
			r.Fail(err.Error())
			return
		}
		if moved {
			d.backend.BroadcastLayout()
		}
		r.OK(nil)

	case CmdPaneCycle:
		var p CycleParams
		if err := dec.Decode(&p); err != nil {
			bad(err)
			return
		}
		if d.session.CyclePane(p.Next) {
			d.backend.BroadcastLayout()
		}
		r.OK(nil)

	case CmdPaneSwap:
		var p DirParams
		if err := dec.Decode(&p); err != nil {
			bad(err)
			return
		}
		nav, ok := NavDirection(p.Dir)
		if !ok {
			r.Fail(fmt.Sprintf("bad direction %q", p.Dir))
			return
		}
		swapped, err := d.session.SwapPaneDirection(nav, d.backend.Area())
		if err != nil {
			r.Fail(err.Error())
			return
		}
		if swapped {
			d.backend.ApplyModel() // panes changed slots/sizes
		}
		r.OK(nil)

	case CmdPaneZoom:
		var p OptPaneParams
		if err := decodeOptional(dec, &p); err != nil {
			bad(err)
			return
		}
		if _, err := d.session.ToggleZoom(optPaneID(p.Pane)); err != nil {
			r.Fail(err.Error())
			return
		}
		d.backend.ApplyModel() // viewport pane set + zoomed pane size changed
		r.OK(nil)

	case CmdPaneResizeBorder:
		var p ResizeBorderParams
		if err := dec.Decode(&p); err != nil {
			bad(err)
			return
		}
		path, ok := BorderPath(p.Border)
		if !ok {
			r.Fail(fmt.Sprintf("bad border id %q", p.Border))
			return
		}
		if err := d.session.ResizeBorder(path, p.Ratio); err != nil {
			r.Fail(err.Error())
			return
		}
		d.backend.ApplyModel() // split ratio changed → panes resize
		r.OK(nil)

	case CmdPaneLast:
		if d.session.FocusLastPane() {
			d.backend.BroadcastLayout()
		}
		r.OK(nil)

	case CmdPaneRename:
		var p RenamePaneParams
		if err := dec.Decode(&p); err != nil {
			bad(err)
			return
		}
		if err := d.session.RenamePane(layout.PaneID(p.Pane), p.Name); err != nil {
			r.Fail(err.Error())
			return
		}
		// Push the new effective title if the pane is on screen; otherwise it
		// rides the chrome resend when the pane next becomes visible.
		d.backend.BroadcastPaneTitle(p.Pane)
		r.OK(nil)

	case CmdPaneSplit:
		var sp SplitParams
		if err := dec.Decode(&sp); err != nil {
			bad(err)
			return
		}
		dir, ok := SplitDirection(sp.Direction)
		if !ok {
			r.Fail(fmt.Sprintf("bad split direction %q", sp.Direction))
			return
		}
		if _, err := d.session.SplitPane(optPaneID(sp.Pane), dir); err != nil {
			r.Fail(err.Error())
			return
		}
		d.backend.ApplyModel()
		r.OK(nil)

	case CmdPaneClose:
		var cp OptPaneParams
		if err := decodeOptional(dec, &cp); err != nil {
			bad(err)
			return
		}
		if _, err := d.session.ClosePane(optPaneID(cp.Pane)); err != nil {
			r.Fail(err.Error())
			return
		}
		d.backend.ApplyModel()
		r.OK(nil)

	case CmdScroll:
		var p ScrollParams
		if err := dec.Decode(&p); err != nil {
			bad(err)
			return
		}
		if err := d.backend.ScrollPane(p.Pane, p.Delta); err != nil {
			r.Fail(err.Error())
			return
		}
		r.OK(nil)

	case CmdRead:
		var p ReadParams
		if err := dec.Decode(&p); err != nil {
			bad(err)
			return
		}
		if !r.WantsReply() {
			return // read yields only a result; with no reply channel there's nowhere to send it
		}
		if !d.backend.PaneExists(p.Pane) {
			r.Fail(fmt.Sprintf("unknown pane %d", p.Pane))
			return
		}
		if !d.backend.DaemonConnected() {
			r.Fail("termhost daemon not connected")
			return
		}
		d.backend.StartRead(r, p) // async: the daemon reply resolves r later

	case CmdCapture:
		var p CaptureParams
		if err := dec.Decode(&p); err != nil {
			bad(err)
			return
		}
		if !r.WantsReply() {
			return // capture yields only a result; with no reply channel there's nowhere to send it
		}
		if !d.backend.PaneExists(p.Pane) {
			r.Fail(fmt.Sprintf("unknown pane %d", p.Pane))
			return
		}
		if !d.backend.DaemonConnected() {
			r.Fail("termhost daemon not connected")
			return
		}
		d.backend.StartCapture(r, p) // async: the daemon reply resolves r later

	case CmdWaitForOutput:
		var p WaitForOutputParams
		if err := dec.Decode(&p); err != nil {
			bad(err)
			return
		}
		if !r.WantsReply() {
			return // wait yields only a result; with no reply channel there's nothing to await
		}
		if _, err := p.Matcher(); err != nil {
			r.Fail(err.Error()) // empty pattern / bad regex
			return
		}
		if !d.backend.PaneExists(p.Pane) {
			r.Fail(fmt.Sprintf("unknown pane %d", p.Pane))
			return
		}
		if !d.backend.DaemonConnected() {
			r.Fail("termhost daemon not connected")
			return
		}
		d.backend.StartWaitForOutput(r, p) // async: a match / timeout / exit resolves r later

	case CmdTabCreate:
		if _, err := d.session.CreateTab(); err != nil {
			r.Fail(err.Error())
			return
		}
		d.backend.ApplyModel()
		r.OK(nil)

	case CmdTabClose:
		var p OptTabParams
		if err := decodeOptional(dec, &p); err != nil {
			bad(err)
			return
		}
		if err := d.session.CloseTab(p.Num); err != nil {
			r.Fail(err.Error())
			return
		}
		d.backend.ApplyModel()
		r.OK(nil)

	case CmdTabFocus:
		var p TabParams
		if err := dec.Decode(&p); err != nil {
			bad(err)
			return
		}
		if err := d.session.FocusTab(p.Num); err != nil {
			r.Fail(err.Error())
			return
		}
		d.backend.ApplyModel()
		r.OK(nil)

	case CmdTabRename:
		var p RenameTabParams
		if err := dec.Decode(&p); err != nil {
			bad(err)
			return
		}
		if err := d.session.RenameTab(p.Num, p.Name); err != nil {
			r.Fail(err.Error())
			return
		}
		d.backend.BroadcastLayout()
		r.OK(nil)

	case CmdTabMove:
		var p MoveTabParams
		if err := dec.Decode(&p); err != nil {
			bad(err)
			return
		}
		moved, err := d.session.MoveTab(p.Num, p.Index)
		if err != nil {
			r.Fail(err.Error())
			return
		}
		if moved {
			d.backend.BroadcastLayout() // order changed; pane set unchanged
		}
		r.OK(nil)

	case CmdWorkspaceCreate:
		if _, err := d.session.CreateWorkspace(); err != nil {
			r.Fail(err.Error())
			return
		}
		d.backend.ApplyModel()
		r.OK(nil)

	case CmdWorkspaceClose:
		var p WorkspaceParams
		_ = dec.Decode(&p) // id optional → active workspace; ignore any decode error
		var id *string
		if p.ID != "" {
			id = &p.ID
		}
		if err := d.session.CloseWorkspace(id); err != nil {
			r.Fail(err.Error())
			return
		}
		d.backend.ApplyModel()
		r.OK(nil)

	case CmdWorkspaceFocus:
		var p WorkspaceParams
		if err := dec.Decode(&p); err != nil {
			bad(err)
			return
		}
		if err := d.session.FocusWorkspace(p.ID); err != nil {
			r.Fail(err.Error())
			return
		}
		d.backend.ApplyModel()
		r.OK(nil)

	case CmdWorkspaceRename:
		var p RenameWorkspaceParams
		if err := dec.Decode(&p); err != nil {
			bad(err)
			return
		}
		if err := d.session.RenameWorkspace(p.ID, p.Name); err != nil {
			r.Fail(err.Error())
			return
		}
		d.backend.BroadcastLayout()
		r.OK(nil)

	case CmdWorkspaceMove:
		var p MoveWorkspaceParams
		if err := dec.Decode(&p); err != nil {
			bad(err)
			return
		}
		moved, err := d.session.MoveWorkspace(p.ID, p.Index)
		if err != nil {
			r.Fail(err.Error())
			return
		}
		if moved {
			d.backend.BroadcastLayout() // order changed; pane set unchanged
		}
		r.OK(nil)

	case CmdAgentFocus:
		var p PaneParams
		if err := dec.Decode(&p); err != nil {
			bad(err)
			return
		}
		// Unlike pane.focus, the agents sidebar is global (§8): the target pane
		// may live in another workspace/tab, so reveal it into the viewport.
		if err := d.session.RevealPane(layout.PaneID(p.Pane)); err != nil {
			r.Fail(err.Error())
			return
		}
		d.backend.ApplyModel() // viewport may have changed (different workspace/tab)
		r.OK(nil)

	case CmdWorktreeList:
		var p WorktreeListParams
		if err := decodeOptional(dec, &p); err != nil {
			bad(err)
			return
		}
		if !r.WantsReply() {
			return // list yields only a result; with no reply channel there's nowhere to send it
		}
		d.backend.StartWorktreeList(r, p) // async: git list resolves r later

	case CmdWorktreeCreate:
		var p WorktreeCreateParams
		if err := decodeOptional(dec, &p); err != nil {
			bad(err)
			return
		}
		d.backend.StartWorktreeCreate(r, p) // async: git add + workspace create resolve r later

	case CmdWorktreeOpen:
		var p WorktreeOpenParams
		if err := decodeOptional(dec, &p); err != nil {
			bad(err)
			return
		}
		if p.Path == "" {
			r.Fail("worktree.open: path is required")
			return
		}
		d.backend.StartWorktreeOpen(r, p)

	case CmdWorktreeRemove:
		var p WorktreeRemoveParams
		if err := decodeOptional(dec, &p); err != nil {
			bad(err)
			return
		}
		if p.Workspace == "" {
			r.Fail("worktree.remove: workspace is required")
			return
		}
		d.backend.StartWorktreeRemove(r, p) // async: git remove + workspace close resolve r later

	case CmdConfigGet:
		if !r.WantsReply() {
			return // config.get yields only a result
		}
		d.backend.ConfigGet(r)

	case CmdConfigSet:
		var p ConfigSetParams
		if err := decodeOptional(dec, &p); err != nil {
			bad(err)
			return
		}
		d.backend.ConfigSet(r, p)

	case CmdServerReloadConfig:
		if err := d.backend.ReloadConfig(); err != nil {
			r.Fail(err.Error())
			return
		}
		r.OK(nil)

	case CmdServerStop:
		// Reply first so the caller gets its result, then go away.
		r.OK(nil)
		d.backend.Shutdown()

	case CmdSessionGet:
		// Read-only queries below answer straight from the session — no Backend
		// effect, no async round-trip.
		r.OK(d.session.Info())

	case CmdWorkspaceList:
		r.OK(WorkspaceListResult{Workspaces: d.session.ListWorkspaces()})

	case CmdTabList:
		var p TabListParams
		if err := decodeOptional(dec, &p); err != nil {
			bad(err)
			return
		}
		tabs, resolved, ok := d.session.ListTabs(p.Workspace)
		if !ok {
			r.Fail(fmt.Sprintf("unknown workspace %q", p.Workspace))
			return
		}
		r.OK(TabListResult{Workspace: resolved, Tabs: tabs})

	case CmdPaneList:
		r.OK(PaneListResult{Panes: d.session.ListPanes()})

	case CmdPaneGet:
		var p OptPaneParams
		if err := decodeOptional(dec, &p); err != nil {
			bad(err)
			return
		}
		info, ok := d.session.PaneInfoFor(optPaneID(p.Pane))
		if !ok {
			r.Fail("no such pane")
			return
		}
		r.OK(info)

	default:
		r.Fail(fmt.Sprintf("command %q not supported yet (WS2 in progress)", name))
	}
}

// decodeOptional decodes params whose fields are all optional: no params decodes
// to the zero value rather than an error (mirrors the old optUnmarshalParams).
func decodeOptional(dec ParamDecoder, v any) error {
	if err := dec.Decode(v); err != nil && !errors.Is(err, ErrNoParams) {
		return err
	}
	return nil
}
