//go:build ghostty

package main

import (
	"fmt"

	"github.com/rohanthewiz/herdr-web/internal/browserproto"
	"github.com/rohanthewiz/herdr-web/internal/layout"
	"github.com/rohanthewiz/herdr-web/internal/orchestration"
)

// handleCmd dispatches one §7 command against the session (WS2's command
// table). Model-mutating commands call an app.Session method, then applyModel
// reconciles the daemon and broadcasts the new viewport; pure focus/rename just
// rebroadcast the layout; scroll passes straight through to the daemon.
// Loop-goroutine only.
func (o *orch) handleCmd(c *client, m *browserproto.Cmd) {
	reply := func(ok bool, errMsg string) {
		if m.ID == "" {
			return
		}
		if r, err := browserproto.NewCmdResult(m.ID, ok, errMsg, nil); err == nil {
			o.send(c, r)
		}
	}

	switch m.Name {
	case browserproto.CmdPaneFocus:
		var p browserproto.PaneParams
		if err := unmarshalParams(m.Params, &p); err != nil {
			reply(false, "bad params: "+err.Error())
			return
		}
		if err := o.session.FocusPane(layout.PaneID(p.Pane)); err != nil {
			reply(false, err.Error())
			return
		}
		o.broadcast(o.viewportLayout()) // focus flag moved; pane set unchanged
		reply(true, "")

	case browserproto.CmdPaneFocusDirection:
		var p browserproto.DirParams
		if err := unmarshalParams(m.Params, &p); err != nil {
			reply(false, "bad params: "+err.Error())
			return
		}
		nav, ok := browserproto.NavDirection(p.Dir)
		if !ok {
			reply(false, fmt.Sprintf("bad direction %q", p.Dir))
			return
		}
		moved, err := o.session.FocusPaneDirection(nav, o.area)
		if err != nil {
			reply(false, err.Error())
			return
		}
		if moved {
			o.broadcast(o.viewportLayout()) // focus flag moved; pane set unchanged
		}
		reply(true, "")

	case browserproto.CmdPaneCycle:
		var p browserproto.CycleParams
		if err := unmarshalParams(m.Params, &p); err != nil {
			reply(false, "bad params: "+err.Error())
			return
		}
		if o.session.CyclePane(p.Next) {
			o.broadcast(o.viewportLayout()) // focus flag moved; pane set unchanged
		}
		reply(true, "")

	case browserproto.CmdPaneSwap:
		var p browserproto.DirParams
		if err := unmarshalParams(m.Params, &p); err != nil {
			reply(false, "bad params: "+err.Error())
			return
		}
		nav, ok := browserproto.NavDirection(p.Dir)
		if !ok {
			reply(false, fmt.Sprintf("bad direction %q", p.Dir))
			return
		}
		swapped, err := o.session.SwapPaneDirection(nav, o.area)
		if err != nil {
			reply(false, err.Error())
			return
		}
		if swapped {
			o.applyModel() // panes changed slots/sizes
		}
		reply(true, "")

	case browserproto.CmdPaneZoom:
		var p browserproto.OptPaneParams
		if err := optUnmarshalParams(m.Params, &p); err != nil {
			reply(false, "bad params: "+err.Error())
			return
		}
		if _, err := o.session.ToggleZoom(optPaneID(p.Pane)); err != nil {
			reply(false, err.Error())
			return
		}
		o.applyModel() // viewport pane set + zoomed pane size changed
		reply(true, "")

	case browserproto.CmdPaneResizeBorder:
		var p browserproto.ResizeBorderParams
		if err := unmarshalParams(m.Params, &p); err != nil {
			reply(false, "bad params: "+err.Error())
			return
		}
		path, ok := browserproto.BorderPath(p.Border)
		if !ok {
			reply(false, fmt.Sprintf("bad border id %q", p.Border))
			return
		}
		if err := o.session.ResizeBorder(path, p.Ratio); err != nil {
			reply(false, err.Error())
			return
		}
		o.applyModel() // split ratio changed → panes resize
		reply(true, "")

	case browserproto.CmdPaneLast:
		if o.session.FocusLastPane() {
			o.broadcast(o.viewportLayout()) // focus flag moved; pane set unchanged
		}
		reply(true, "")

	case browserproto.CmdPaneRename:
		var p browserproto.RenamePaneParams
		if err := unmarshalParams(m.Params, &p); err != nil {
			reply(false, "bad params: "+err.Error())
			return
		}
		if err := o.session.RenamePane(layout.PaneID(p.Pane), p.Name); err != nil {
			reply(false, err.Error())
			return
		}
		// Push the new effective title if the pane is on screen; otherwise it
		// rides the chrome resend when the pane next becomes visible.
		if o.visible[p.Pane] {
			o.broadcast(browserproto.NewPaneTitle(p.Pane, o.effectiveTitle(p.Pane)))
		}
		reply(true, "")

	case browserproto.CmdPaneSplit:
		var sp browserproto.SplitParams
		if err := unmarshalParams(m.Params, &sp); err != nil {
			reply(false, "bad params: "+err.Error())
			return
		}
		dir, ok := browserproto.SplitDirection(sp.Direction)
		if !ok {
			reply(false, fmt.Sprintf("bad split direction %q", sp.Direction))
			return
		}
		if _, err := o.session.SplitPane(optPaneID(sp.Pane), dir); err != nil {
			reply(false, err.Error())
			return
		}
		o.applyModel()
		reply(true, "")

	case browserproto.CmdPaneClose:
		var cp browserproto.OptPaneParams
		if err := optUnmarshalParams(m.Params, &cp); err != nil {
			reply(false, "bad params: "+err.Error())
			return
		}
		if _, err := o.session.ClosePane(optPaneID(cp.Pane)); err != nil {
			reply(false, err.Error())
			return
		}
		o.applyModel()
		reply(true, "")

	case browserproto.CmdScroll:
		var p browserproto.ScrollParams
		if err := unmarshalParams(m.Params, &p); err != nil {
			reply(false, "bad params: "+err.Error())
			return
		}
		if o.panes[p.Pane] == nil {
			reply(false, fmt.Sprintf("unknown pane %d", p.Pane))
			return
		}
		o.daemon.send(orchestration.NewScrollViewport(p.Pane, int32(p.Delta)))
		reply(true, "")

	case browserproto.CmdRead:
		var p browserproto.ReadParams
		if err := unmarshalParams(m.Params, &p); err != nil {
			reply(false, "bad params: "+err.Error())
			return
		}
		if m.ID == "" {
			return // read yields only a result; with no id there's nowhere to send it
		}
		if o.panes[p.Pane] == nil {
			reply(false, fmt.Sprintf("unknown pane %d", p.Pane))
			return
		}
		if !o.daemon.connected() {
			reply(false, "termhost daemon not connected")
			return
		}
		o.startRead(c, m.ID, p) // async: resolveRead sends the cmd_result when the daemon replies

	case browserproto.CmdTabCreate:
		if _, err := o.session.CreateTab(); err != nil {
			reply(false, err.Error())
			return
		}
		o.applyModel()
		reply(true, "")

	case browserproto.CmdTabClose:
		var p browserproto.OptTabParams
		if err := optUnmarshalParams(m.Params, &p); err != nil {
			reply(false, "bad params: "+err.Error())
			return
		}
		if err := o.session.CloseTab(p.Num); err != nil {
			reply(false, err.Error())
			return
		}
		o.applyModel()
		reply(true, "")

	case browserproto.CmdTabFocus:
		var p browserproto.TabParams
		if err := unmarshalParams(m.Params, &p); err != nil {
			reply(false, "bad params: "+err.Error())
			return
		}
		if err := o.session.FocusTab(p.Num); err != nil {
			reply(false, err.Error())
			return
		}
		o.applyModel()
		reply(true, "")

	case browserproto.CmdTabRename:
		var p browserproto.RenameTabParams
		if err := unmarshalParams(m.Params, &p); err != nil {
			reply(false, "bad params: "+err.Error())
			return
		}
		if err := o.session.RenameTab(p.Num, p.Name); err != nil {
			reply(false, err.Error())
			return
		}
		o.broadcast(o.viewportLayout())
		reply(true, "")

	case browserproto.CmdWorkspaceCreate:
		if _, err := o.session.CreateWorkspace(); err != nil {
			reply(false, err.Error())
			return
		}
		o.applyModel()
		reply(true, "")

	case browserproto.CmdWorkspaceClose:
		var p browserproto.WorkspaceParams
		_ = optUnmarshalParams(m.Params, &p) // id optional → active workspace
		var id *string
		if p.ID != "" {
			id = &p.ID
		}
		if err := o.session.CloseWorkspace(id); err != nil {
			reply(false, err.Error())
			return
		}
		o.applyModel()
		reply(true, "")

	case browserproto.CmdWorkspaceFocus:
		var p browserproto.WorkspaceParams
		if err := unmarshalParams(m.Params, &p); err != nil {
			reply(false, "bad params: "+err.Error())
			return
		}
		if err := o.session.FocusWorkspace(p.ID); err != nil {
			reply(false, err.Error())
			return
		}
		o.applyModel()
		reply(true, "")

	case browserproto.CmdWorkspaceRename:
		var p browserproto.RenameWorkspaceParams
		if err := unmarshalParams(m.Params, &p); err != nil {
			reply(false, "bad params: "+err.Error())
			return
		}
		if err := o.session.RenameWorkspace(p.ID, p.Name); err != nil {
			reply(false, err.Error())
			return
		}
		o.broadcast(o.viewportLayout())
		reply(true, "")

	default:
		reply(false, fmt.Sprintf("command %q not supported yet (WS2 in progress)", m.Name))
	}
}

// optPaneID converts an optional wire pane id into an optional layout.PaneID
// (nil = the focused pane).
func optPaneID(p *uint32) *layout.PaneID {
	if p == nil {
		return nil
	}
	id := layout.PaneID(*p)
	return &id
}
