package app

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/rohanthewiz/herdr-web/internal/layout"
)

// This file is the protocol-neutral §7 command vocabulary: the command names,
// their parameter/result structs, and the small string→enum mappings the
// dispatcher needs. It lives in internal/app (not browserproto) so the one
// command table can serve both the browser WebSocket protocol and a future
// CLI/control-API. browserproto re-exports these as aliases for wire use; the
// json tags here are inert unless a json ParamDecoder consults them.

// Command names (§7): the control-API vocabulary. The dispatcher implements one
// command table serving both this protocol and the CLI/API.
const (
	CmdPaneSplit          = "pane.split"
	CmdPaneClose          = "pane.close"
	CmdPaneFocus          = "pane.focus"
	CmdPaneFocusDirection = "pane.focus_direction"
	CmdPaneCycle          = "pane.cycle"
	CmdPaneLast           = "pane.last"
	CmdPaneSwap           = "pane.swap"
	CmdPaneZoom           = "pane.zoom"
	CmdPaneRename         = "pane.rename"
	CmdPaneResizeBorder   = "pane.resize_border"
	CmdScroll             = "scroll"
	CmdRead               = "read"
	CmdCapture            = "capture"
	CmdWaitForOutput      = "pane.wait_for_output"
	CmdTabCreate          = "tab.create"
	CmdTabClose           = "tab.close"
	CmdTabFocus           = "tab.focus"
	CmdTabRename          = "tab.rename"
	CmdWorkspaceCreate    = "workspace.create"
	CmdWorkspaceClose     = "workspace.close"
	CmdWorkspaceFocus     = "workspace.focus"
	CmdWorkspaceRename    = "workspace.rename"
	CmdAgentFocus         = "agent.focus"
	CmdServerReloadConfig = "server.reload_config"
	CmdServerStop         = "server.stop"

	// Read-only query commands (§7): they return a snapshot of session state
	// and mutate nothing, so the dispatcher answers them straight from the
	// Session with no Backend effects.
	CmdSessionGet    = "session.get"
	CmdWorkspaceList = "workspace.list"
	CmdTabList       = "tab.list"
	CmdPaneList      = "pane.list"
	CmdPaneGet       = "pane.get"
)

// CommandNames returns every §7 command name Dispatcher.Dispatch accepts, in a
// stable order. Front-ends enumerate/validate the vocabulary against it — a CLI's
// help text, a control-API client — without re-listing the commands. Keep it in
// sync with the Dispatch switch; TestCommandNamesAllRouted guards against drift.
func CommandNames() []string {
	return []string{
		CmdPaneSplit, CmdPaneClose, CmdPaneFocus, CmdPaneFocusDirection,
		CmdPaneCycle, CmdPaneLast, CmdPaneSwap, CmdPaneZoom, CmdPaneRename,
		CmdPaneResizeBorder, CmdScroll, CmdRead, CmdCapture, CmdWaitForOutput,
		CmdTabCreate, CmdTabClose, CmdTabFocus, CmdTabRename,
		CmdWorkspaceCreate, CmdWorkspaceClose, CmdWorkspaceFocus, CmdWorkspaceRename,
		CmdAgentFocus, CmdServerReloadConfig, CmdServerStop,
		CmdSessionGet, CmdWorkspaceList, CmdTabList, CmdPaneList, CmdPaneGet,
	}
}

// Split direction wire values (pane.split).
const (
	SplitH = "h" // side-by-side (layout.Horizontal)
	SplitV = "v" // top/bottom (layout.Vertical)
)

// SplitDirection maps a wire direction value onto layout.Direction.
func SplitDirection(s string) (layout.Direction, bool) {
	switch s {
	case SplitH:
		return layout.Horizontal, true
	case SplitV:
		return layout.Vertical, true
	}
	return 0, false
}

// Cardinal direction wire values (pane.focus_direction, pane.swap).
const (
	DirLeft  = "left"
	DirRight = "right"
	DirUp    = "up"
	DirDown  = "down"
)

// NavDirection maps a wire cardinal value onto layout.NavDirection.
func NavDirection(s string) (layout.NavDirection, bool) {
	switch s {
	case DirLeft:
		return layout.Left, true
	case DirRight:
		return layout.Right, true
	case DirUp:
		return layout.Up, true
	case DirDown:
		return layout.Down, true
	}
	return 0, false
}

// BorderPath decodes a border id ("r" + one '0'/'1' per split step, e.g. "r01",
// produced by browserproto.BorderID) back into a split path for
// layout.TileLayout.SetRatioAt. Reports false for malformed ids. The "r01"
// format is a contract shared with browserproto's BuildLayout emitter.
func BorderPath(id string) ([]bool, bool) {
	if len(id) == 0 || id[0] != 'r' {
		return nil, false
	}
	path := make([]bool, 0, len(id)-1)
	for _, c := range id[1:] {
		switch c {
		case '0':
			path = append(path, false)
		case '1':
			path = append(path, true)
		default:
			return nil, false
		}
	}
	return path, true
}

// SplitParams: pane.split. Pane nil = the focused pane.
type SplitParams struct {
	Pane      *uint32 `json:"pane,omitempty"`
	Direction string  `json:"direction"` // SplitH | SplitV
}

// PaneParams: pane.focus, agent.focus — commands addressing a specific pane.
type PaneParams struct {
	Pane uint32 `json:"pane"`
}

// OptPaneParams: pane.close, pane.zoom. Pane nil = the focused pane.
type OptPaneParams struct {
	Pane *uint32 `json:"pane,omitempty"`
}

// DirParams: pane.focus_direction, pane.swap.
type DirParams struct {
	Dir string `json:"dir"` // DirLeft | DirRight | DirUp | DirDown
}

// CycleParams: pane.cycle.
type CycleParams struct {
	Next bool `json:"next"`
}

// RenamePaneParams: pane.rename ("" clears the custom name).
type RenamePaneParams struct {
	Pane uint32 `json:"pane"`
	Name string `json:"name"`
}

// ResizeBorderParams: pane.resize_border. Border is the opaque id from the
// layout message's borders list; Ratio is the split's new first-child ratio.
type ResizeBorderParams struct {
	Border string  `json:"border"`
	Ratio  float32 `json:"ratio"`
}

// ScrollParams: scroll. Delta lines: negative scrolls up into history,
// positive back toward the live bottom (β ScrollViewport semantics).
type ScrollParams struct {
	Pane  uint32 `json:"pane"`
	Delta int    `json:"delta"`
}

// ReadParams: read — extract selection text. Anchor/Cursor are [row, col] in
// absolute screen-buffer coordinates (row from the top of scrollback, per
// β SelectionPoint; derive from the frame's Scroll). Rect selects a block
// region instead of a reading-order range.
type ReadParams struct {
	Pane   uint32    `json:"pane"`
	Anchor [2]uint32 `json:"anchor"`
	Cursor [2]uint32 `json:"cursor"`
	Rect   bool      `json:"rect,omitempty"`
}

// ReadResult is CmdResult.Data for a successful read.
type ReadResult struct {
	Text string `json:"text"`
}

// CaptureParams: capture — extract a pane's buffer text (β RequestText). Scope
// 0 = visible (the on-screen viewport), 1 = recent (the last Lines rows of
// scrollback+active, 0 = the whole buffer). Ansi keeps VT styling; Unwrap rejoins
// soft-wrapped lines. Unlike read, this needs no coordinates — it captures whole
// rows, e.g. for "copy scrollback" or feeding an agent the terminal contents.
type CaptureParams struct {
	Pane   uint32 `json:"pane"`
	Scope  uint8  `json:"scope,omitempty"`
	Lines  uint32 `json:"lines,omitempty"`
	Ansi   bool   `json:"ansi,omitempty"`
	Unwrap bool   `json:"unwrap,omitempty"`
}

// CaptureResult is CmdResult.Data for a successful capture.
type CaptureResult struct {
	Text string `json:"text"`
}

// WaitForOutputParams: pane.wait_for_output — block until the pane's recent
// buffer text matches Pattern (a substring, or a regexp when Regex is set), or
// until TimeoutMs elapses. Unlike read/capture (one round-trip), this rides the
// unary envelope but resolves only when the match appears: the backend re-scans
// the pane's captured text as it produces output. Lines bounds how many recent
// rows are scanned (0 = the whole buffer); the scan is over plain text (no VT
// styling, soft-wraps rejoined). TimeoutMs 0 uses the server default.
type WaitForOutputParams struct {
	Pane      uint32 `json:"pane"`
	Pattern   string `json:"pattern"`
	Regex     bool   `json:"regex,omitempty"`
	TimeoutMs uint32 `json:"timeout_ms,omitempty"`
	Lines     uint32 `json:"lines,omitempty"`
}

// WaitForOutputResult is CmdResult.Data for pane.wait_for_output. Matched reports
// whether Pattern appeared before the timeout (false = timed out or the pane
// exited first); Text is the buffer line the match landed on, for context.
type WaitForOutputResult struct {
	Matched bool   `json:"matched"`
	Text    string `json:"text,omitempty"`
}

// Matcher compiles Pattern into a predicate over a pane's captured text: it
// returns the matched line (trimmed, for the result's Text) and whether the
// pattern is present. It also validates the params — an empty pattern or an
// uncompilable regex is an error the dispatcher reports as bad params before it
// registers a waiter. The backend calls Matcher to get the live predicate.
func (p WaitForOutputParams) Matcher() (func(text string) (line string, ok bool), error) {
	if p.Pattern == "" {
		return nil, errors.New("wait_for_output: empty pattern")
	}
	if p.Regex {
		re, err := regexp.Compile(p.Pattern)
		if err != nil {
			return nil, fmt.Errorf("wait_for_output: bad regex %q: %w", p.Pattern, err)
		}
		return func(text string) (string, bool) {
			loc := re.FindStringIndex(text)
			if loc == nil {
				return "", false
			}
			return lineAround(text, loc[0]), true
		}, nil
	}
	return func(text string) (string, bool) {
		idx := strings.Index(text, p.Pattern)
		if idx < 0 {
			return "", false
		}
		return lineAround(text, idx), true
	}, nil
}

// lineAround returns the trimmed line of text containing byte index idx (the
// wait_for_output result's context line). idx is assumed in range.
func lineAround(text string, idx int) string {
	start := strings.LastIndexByte(text[:idx], '\n') + 1 // 0 if none
	end := idx + strings.IndexByte(text[idx:], '\n')
	if end < idx { // no trailing newline: run to the end
		end = len(text)
	}
	return strings.TrimSpace(text[start:end])
}

// Wait-timeout bounds shared by the backend waiter and any client sizing its own
// round-trip deadline, so both agree on how long a wait can run.
const (
	defaultWaitTimeout = 30 * time.Second
	// MaxWaitTimeout caps a single wait; the ctlproto server sizes its backstop
	// above this so a waiter always resolves on its own timer first.
	MaxWaitTimeout = 10 * time.Minute
)

// WaitTimeout resolves a wait's TimeoutMs into a duration: 0 ⇒ the default, and
// anything above MaxWaitTimeout is clamped.
func WaitTimeout(ms uint32) time.Duration {
	if ms == 0 {
		return defaultWaitTimeout
	}
	if d := time.Duration(ms) * time.Millisecond; d < MaxWaitTimeout {
		return d
	}
	return MaxWaitTimeout
}

// TabParams: tab.focus.
type TabParams struct {
	Num int `json:"num"`
}

// OptTabParams: tab.close. Num nil = the active tab.
type OptTabParams struct {
	Num *int `json:"num,omitempty"`
}

// RenameTabParams: tab.rename ("" clears the custom name).
type RenameTabParams struct {
	Num  int    `json:"num"`
	Name string `json:"name"`
}

// WorkspaceParams: workspace.focus, workspace.close.
type WorkspaceParams struct {
	ID string `json:"id"` // public workspace id, e.g. "w1"
}

// RenameWorkspaceParams: workspace.rename ("" reverts to auto-naming).
type RenameWorkspaceParams struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// --- Query params & results (§7 read-only commands) --------------------------

// TabListParams: tab.list. Workspace "" = the active workspace.
type TabListParams struct {
	Workspace string `json:"workspace,omitempty"`
}

// SessionInfoResult is CmdResult.Data for session.get: a one-shot snapshot of the
// whole session. FocusedPane is the public handle of the globally focused pane
// (the active workspace's active tab's focused pane), empty if there is none.
type SessionInfoResult struct {
	ActiveWorkspace string `json:"active_workspace"`
	FocusedPane     string `json:"focused_pane,omitempty"`
	Workspaces      int    `json:"workspaces"`
	Panes           int    `json:"panes"` // total live panes across all workspaces/tabs
	Cwd             string `json:"cwd"`
}

// WorkspaceInfo describes one workspace for workspace.list.
type WorkspaceInfo struct {
	ID     string `json:"id"`   // stable public handle, e.g. "w1"
	Name   string `json:"name"` // display name (custom or auto)
	Active bool   `json:"active"`
	Tabs   int    `json:"tabs"` // tab count
}

// WorkspaceListResult is CmdResult.Data for workspace.list.
type WorkspaceListResult struct {
	Workspaces []WorkspaceInfo `json:"workspaces"`
}

// TabInfo describes one tab for tab.list.
type TabInfo struct {
	Num    int    `json:"num"`  // stable public tab number
	Name   string `json:"name"` // display name (custom or the number)
	Active bool   `json:"active"`
	Zoomed bool   `json:"zoomed"`
	Panes  int    `json:"panes"` // pane count in this tab
}

// TabListResult is CmdResult.Data for tab.list. Workspace echoes the resolved
// workspace id (useful when the request omitted it and got the active one).
type TabListResult struct {
	Workspace string    `json:"workspace"`
	Tabs      []TabInfo `json:"tabs"`
}

// PaneInfo describes one pane for pane.list / pane.get. Pane is the internal id
// used to address the pane in every other command; Handle is its human public
// label ("w1:p3"). Focused marks the pane focused within its own tab (each tab
// has one); Visible marks the panes in the current viewport.
type PaneInfo struct {
	Pane    uint32 `json:"pane"`
	Handle  string `json:"handle,omitempty"`
	Name    string `json:"name,omitempty"` // custom name; empty if auto-named
	Focused bool   `json:"focused"`
	Visible bool   `json:"visible"`
}

// PaneListResult is CmdResult.Data for pane.list.
type PaneListResult struct {
	Panes []PaneInfo `json:"panes"`
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
