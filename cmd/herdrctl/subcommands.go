package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/rohanthewiz/herdr-web/internal/app"
	"github.com/rohanthewiz/herdr-web/internal/ctlproto"
)

// This file is herdrctl's ergonomic subcommand layer: short verbs that take
// positional arguments and build the §7 command's JSON params for the caller,
// e.g. `herdrctl split h 2` in place of
// `herdrctl pane.split --params '{"direction":"h","pane":2}'`. Each verb maps to
// exactly one method and reuses the app.*Params structs so the wire shape can
// never drift from the server's. The raw `<method> [--params json]` path in
// main.go stays the full-coverage escape hatch (and the only way to reach the
// rarely-scripted options like read's rect or capture's ansi/unwrap).

// subcommand is one ergonomic verb. build turns the verb's positional args into
// the method's params (nil for a no-params command); a usageErr from build means
// the args were malformed and the synopsis should be shown.
type subcommand struct {
	verb     string
	method   string
	synopsis string // argument shape, e.g. "split [h|v] [pane]"
	summary  string // one-line description for help
	build    func(args []string) (json.RawMessage, error)
}

// usageErr reports malformed subcommand arguments; its message is the verb's
// synopsis so the CLI can point the user at the right shape.
type usageErr struct{ synopsis string }

func (e usageErr) Error() string { return "usage: herdrctl " + e.synopsis }

// subcommands is the ordered ergonomic verb table (ordering drives help output).
// Grouped queries → pane → tab → workspace → misc, mirroring §7.
var subcommands = []subcommand{
	// Liveness.
	{"ping", ctlproto.MethodPing, "ping", "check the server is reachable", noParams},

	// Read-only queries.
	{"session", app.CmdSessionGet, "session", "session summary", noParams},
	{"workspaces", app.CmdWorkspaceList, "workspaces", "list workspaces", noParams},
	{"tabs", app.CmdTabList, "tabs [workspace]", "list tabs (active workspace by default)", buildTabList},
	{"panes", app.CmdPaneList, "panes", "list all panes", noParams},
	{"pane", app.CmdPaneGet, "pane [pane]", "describe one pane (focused by default)", buildOptPane},
	{"events", ctlproto.MethodEventsSubscribe, "events [pane]", "stream pane events until interrupted (Ctrl-C)", buildEvents},

	// Pane commands.
	{"split", app.CmdPaneSplit, "split [h|v] [pane]", "split a pane (h by default)", buildSplit},
	{"close", app.CmdPaneClose, "close [pane]", "close a pane (focused by default)", buildOptPane},
	{"focus", app.CmdPaneFocus, "focus <pane>", "focus a pane", buildPane},
	{"focus-dir", app.CmdPaneFocusDirection, "focus-dir <left|right|up|down>", "focus the neighbour in a direction", buildDir},
	{"cycle", app.CmdPaneCycle, "cycle [prev]", "focus the next pane (prev for previous)", buildCycle},
	{"last", app.CmdPaneLast, "last", "focus the previously focused pane", noParams},
	{"swap", app.CmdPaneSwap, "swap <left|right|up|down>", "swap with the neighbour in a direction", buildDir},
	{"zoom", app.CmdPaneZoom, "zoom [pane]", "toggle pane zoom (focused by default)", buildOptPane},
	{"rename-pane", app.CmdPaneRename, "rename-pane <pane> <name...>", "rename a pane (empty name clears)", buildRenamePane},
	{"resize", app.CmdPaneResizeBorder, "resize <border> <ratio>", "set a split border's ratio", buildResize},
	{"scroll", app.CmdScroll, "scroll <pane> <delta>", "scroll a pane by delta lines (negative = up)", buildScroll},
	{"capture", app.CmdCapture, "capture <pane> [lines]", "capture a pane's text (whole buffer, or last N lines)", buildCapture},
	{"read", app.CmdRead, "read <pane> <r0> <c0> <r1> <c1>", "read the text between two [row,col] points", buildRead},
	{"wait", app.CmdWaitForOutput, "wait <pane> <pattern> [timeout_secs]", "wait until a pane's output contains a pattern", buildWait},

	// Tab commands.
	{"tab", app.CmdTabFocus, "tab <num>", "focus a tab", buildTabFocus},
	{"new-tab", app.CmdTabCreate, "new-tab", "create a tab", noParams},
	{"close-tab", app.CmdTabClose, "close-tab [num]", "close a tab (active by default)", buildOptTab},
	{"rename-tab", app.CmdTabRename, "rename-tab <num> <name...>", "rename a tab (empty name clears)", buildRenameTab},

	// Workspace commands.
	{"ws", app.CmdWorkspaceFocus, "ws <id>", "focus a workspace", buildWorkspace},
	{"new-ws", app.CmdWorkspaceCreate, "new-ws", "create a workspace", noParams},
	{"close-ws", app.CmdWorkspaceClose, "close-ws [id]", "close a workspace (active by default)", buildOptWorkspace},
	{"rename-ws", app.CmdWorkspaceRename, "rename-ws <id> <name...>", "rename a workspace (empty name clears)", buildRenameWorkspace},

	// Misc.
	{"agent", app.CmdAgentFocus, "agent <pane>", "reveal an agent's pane", buildPane},
	{"reload", app.CmdServerReloadConfig, "reload", "reload server config", noParams},
	{"stop", app.CmdServerStop, "stop", "stop the server (terminals survive)", noParams},
}

// lookupSubcommand finds an ergonomic verb by name.
func lookupSubcommand(verb string) (subcommand, bool) {
	for _, sc := range subcommands {
		if sc.verb == verb {
			return sc, true
		}
	}
	return subcommand{}, false
}

// --- param builders ----------------------------------------------------------

// noParams accepts a no-argument verb.
func noParams(args []string) (json.RawMessage, error) {
	if len(args) != 0 {
		return nil, usageErr{"<verb> (takes no arguments)"}
	}
	return nil, nil
}

// buildSplit: split [h|v] [pane].
func buildSplit(args []string) (json.RawMessage, error) {
	if len(args) > 2 {
		return nil, usageErr{"split [h|v] [pane]"}
	}
	p := app.SplitParams{Direction: app.SplitH}
	if len(args) >= 1 {
		if args[0] != app.SplitH && args[0] != app.SplitV {
			return nil, usageErr{"split [h|v] [pane]"}
		}
		p.Direction = args[0]
	}
	if len(args) == 2 {
		n, err := parsePane(args[1])
		if err != nil {
			return nil, err
		}
		p.Pane = &n
	}
	return marshal(p)
}

// buildPane: <verb> <pane> — a required pane id (focus, agent).
func buildPane(args []string) (json.RawMessage, error) {
	if len(args) != 1 {
		return nil, usageErr{"<verb> <pane>"}
	}
	n, err := parsePane(args[0])
	if err != nil {
		return nil, err
	}
	return marshal(app.PaneParams{Pane: n})
}

// buildOptPane: <verb> [pane] — an optional pane id (close, zoom, pane.get).
func buildOptPane(args []string) (json.RawMessage, error) {
	if len(args) > 1 {
		return nil, usageErr{"<verb> [pane]"}
	}
	if len(args) == 0 {
		return nil, nil
	}
	n, err := parsePane(args[0])
	if err != nil {
		return nil, err
	}
	return marshal(app.OptPaneParams{Pane: &n})
}

// buildDir: <verb> <direction> — focus-dir, swap.
func buildDir(args []string) (json.RawMessage, error) {
	if len(args) != 1 {
		return nil, usageErr{"<verb> <left|right|up|down>"}
	}
	if _, ok := app.NavDirection(args[0]); !ok {
		return nil, usageErr{"<verb> <left|right|up|down>"}
	}
	return marshal(app.DirParams{Dir: args[0]})
}

// buildCycle: cycle [prev].
func buildCycle(args []string) (json.RawMessage, error) {
	next := true
	switch {
	case len(args) == 0:
	case len(args) == 1 && args[0] == "prev":
		next = false
	case len(args) == 1 && args[0] == "next":
	default:
		return nil, usageErr{"cycle [prev]"}
	}
	return marshal(app.CycleParams{Next: next})
}

// buildRenamePane: rename-pane <pane> <name...>.
func buildRenamePane(args []string) (json.RawMessage, error) {
	if len(args) < 2 {
		return nil, usageErr{"rename-pane <pane> <name...>"}
	}
	n, err := parsePane(args[0])
	if err != nil {
		return nil, err
	}
	return marshal(app.RenamePaneParams{Pane: n, Name: strings.Join(args[1:], " ")})
}

// buildResize: resize <border> <ratio>.
func buildResize(args []string) (json.RawMessage, error) {
	if len(args) != 2 {
		return nil, usageErr{"resize <border> <ratio>"}
	}
	ratio, err := strconv.ParseFloat(args[1], 32)
	if err != nil {
		return nil, fmt.Errorf("ratio %q is not a number", args[1])
	}
	return marshal(app.ResizeBorderParams{Border: args[0], Ratio: float32(ratio)})
}

// buildScroll: scroll <pane> <delta>.
func buildScroll(args []string) (json.RawMessage, error) {
	if len(args) != 2 {
		return nil, usageErr{"scroll <pane> <delta>"}
	}
	pane, err := parsePane(args[0])
	if err != nil {
		return nil, err
	}
	delta, err := strconv.Atoi(args[1])
	if err != nil {
		return nil, fmt.Errorf("delta %q is not an integer", args[1])
	}
	return marshal(app.ScrollParams{Pane: pane, Delta: delta})
}

// buildCapture: capture <pane> [lines]. With lines, captures the last N rows of
// scrollback (scope "recent"); without, the whole buffer (recent, lines 0).
func buildCapture(args []string) (json.RawMessage, error) {
	if len(args) < 1 || len(args) > 2 {
		return nil, usageErr{"capture <pane> [lines]"}
	}
	pane, err := parsePane(args[0])
	if err != nil {
		return nil, err
	}
	p := app.CaptureParams{Pane: pane, Scope: 1} // 1 = recent; lines 0 = whole buffer
	if len(args) == 2 {
		lines, err := strconv.ParseUint(args[1], 10, 32)
		if err != nil {
			return nil, fmt.Errorf("lines %q is not a number", args[1])
		}
		p.Lines = uint32(lines)
	}
	return marshal(p)
}

// buildRead: read <pane> <r0> <c0> <r1> <c1>.
func buildRead(args []string) (json.RawMessage, error) {
	if len(args) != 5 {
		return nil, usageErr{"read <pane> <r0> <c0> <r1> <c1>"}
	}
	nums := make([]uint32, 5)
	for i, a := range args {
		v, err := strconv.ParseUint(a, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("%q is not a number", a)
		}
		nums[i] = uint32(v)
	}
	return marshal(app.ReadParams{
		Pane:   nums[0],
		Anchor: [2]uint32{nums[1], nums[2]},
		Cursor: [2]uint32{nums[3], nums[4]},
	})
}

// buildWait: wait <pane> <pattern> [timeout_secs]. The pattern is a plain
// substring; the raw path (pane.wait_for_output --params) reaches regex/lines.
// timeout_secs accepts fractions (e.g. 0.5); omitted uses the server default.
func buildWait(args []string) (json.RawMessage, error) {
	if len(args) < 2 || len(args) > 3 {
		return nil, usageErr{"wait <pane> <pattern> [timeout_secs]"}
	}
	pane, err := parsePane(args[0])
	if err != nil {
		return nil, err
	}
	p := app.WaitForOutputParams{Pane: pane, Pattern: args[1]}
	if len(args) == 3 {
		secs, err := strconv.ParseFloat(args[2], 64)
		if err != nil || secs < 0 {
			return nil, fmt.Errorf("timeout %q is not a non-negative number of seconds", args[2])
		}
		p.TimeoutMs = uint32(secs * 1000)
	}
	return marshal(p)
}

// buildEvents: events [pane] — an optional pane filter for the event stream. The
// raw path reaches the events filter (`events.subscribe --params '{"events":[…]}'`).
func buildEvents(args []string) (json.RawMessage, error) {
	if len(args) > 1 {
		return nil, usageErr{"events [pane]"}
	}
	if len(args) == 0 {
		return nil, nil
	}
	pane, err := parsePane(args[0])
	if err != nil {
		return nil, err
	}
	return marshal(app.EventsSubscribeParams{Pane: &pane})
}

// buildTabFocus: tab <num>.
func buildTabFocus(args []string) (json.RawMessage, error) {
	if len(args) != 1 {
		return nil, usageErr{"tab <num>"}
	}
	num, err := strconv.Atoi(args[0])
	if err != nil {
		return nil, fmt.Errorf("tab number %q is not an integer", args[0])
	}
	return marshal(app.TabParams{Num: num})
}

// buildOptTab: close-tab [num].
func buildOptTab(args []string) (json.RawMessage, error) {
	if len(args) > 1 {
		return nil, usageErr{"close-tab [num]"}
	}
	if len(args) == 0 {
		return nil, nil
	}
	num, err := strconv.Atoi(args[0])
	if err != nil {
		return nil, fmt.Errorf("tab number %q is not an integer", args[0])
	}
	return marshal(app.OptTabParams{Num: &num})
}

// buildRenameTab: rename-tab <num> <name...>.
func buildRenameTab(args []string) (json.RawMessage, error) {
	if len(args) < 2 {
		return nil, usageErr{"rename-tab <num> <name...>"}
	}
	num, err := strconv.Atoi(args[0])
	if err != nil {
		return nil, fmt.Errorf("tab number %q is not an integer", args[0])
	}
	return marshal(app.RenameTabParams{Num: num, Name: strings.Join(args[1:], " ")})
}

// buildTabList: tabs [workspace].
func buildTabList(args []string) (json.RawMessage, error) {
	if len(args) > 1 {
		return nil, usageErr{"tabs [workspace]"}
	}
	if len(args) == 0 {
		return nil, nil
	}
	return marshal(app.TabListParams{Workspace: args[0]})
}

// buildWorkspace: ws <id>.
func buildWorkspace(args []string) (json.RawMessage, error) {
	if len(args) != 1 {
		return nil, usageErr{"ws <id>"}
	}
	return marshal(app.WorkspaceParams{ID: args[0]})
}

// buildOptWorkspace: close-ws [id].
func buildOptWorkspace(args []string) (json.RawMessage, error) {
	if len(args) > 1 {
		return nil, usageErr{"close-ws [id]"}
	}
	if len(args) == 0 {
		return nil, nil
	}
	return marshal(app.WorkspaceParams{ID: args[0]})
}

// buildRenameWorkspace: rename-ws <id> <name...>.
func buildRenameWorkspace(args []string) (json.RawMessage, error) {
	if len(args) < 2 {
		return nil, usageErr{"rename-ws <id> <name...>"}
	}
	return marshal(app.RenameWorkspaceParams{ID: args[0], Name: strings.Join(args[1:], " ")})
}

// --- helpers -----------------------------------------------------------------

// parsePane parses a pane id (the internal uint32 used to address panes; get it
// from `herdrctl panes`).
func parsePane(s string) (uint32, error) {
	n, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("pane %q is not a valid id (see `herdrctl panes`)", s)
	}
	return uint32(n), nil
}

// marshal encodes a params struct, wrapping the (practically impossible) error.
func marshal(v any) (json.RawMessage, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encode params: %w", err)
	}
	return b, nil
}

// subcommandsHelp renders the ergonomic verb table as aligned help lines.
func subcommandsHelp() string {
	width := 0
	for _, sc := range subcommands {
		if len(sc.synopsis) > width {
			width = len(sc.synopsis)
		}
	}
	var b strings.Builder
	for _, sc := range subcommands {
		fmt.Fprintf(&b, "  %-*s  %s\n", width, sc.synopsis, sc.summary)
	}
	return b.String()
}
