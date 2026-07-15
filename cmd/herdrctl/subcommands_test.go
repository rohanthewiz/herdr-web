package main

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/rohanthewiz/herdr-web/internal/app"
	"github.com/rohanthewiz/herdr-web/internal/ctlproto"
)

// buildOK runs a verb's builder and unmarshals its params into want's type,
// asserting the result equals want. A nil want means the builder must emit no
// params (a no-params command).
func buildOK[T any](t *testing.T, verb string, args []string, want T) {
	t.Helper()
	sc, ok := lookupSubcommand(verb)
	if !ok {
		t.Fatalf("no such verb %q", verb)
	}
	raw, err := sc.build(args)
	if err != nil {
		t.Fatalf("%s %v: unexpected error %v", verb, args, err)
	}
	// An optional command with all-default operands emits no params at all; the
	// dispatcher treats that as the zero value, so want must be the zero too.
	if len(raw) == 0 {
		var zero T
		if mustJSON(t, want) != mustJSON(t, zero) {
			t.Fatalf("%s %v: builder emitted no params, but want %s", verb, args, mustJSON(t, want))
		}
		return
	}
	var got T
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("%s %v: params %s not a %T: %v", verb, args, raw, got, err)
	}
	if b, _ := json.Marshal(got); string(b) != mustJSON(t, want) {
		t.Fatalf("%s %v: params = %s, want %s", verb, args, b, mustJSON(t, want))
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal want: %v", err)
	}
	return string(b)
}

// buildErr asserts a verb's builder rejects the given args.
func buildErr(t *testing.T, verb string, args []string) {
	t.Helper()
	sc, ok := lookupSubcommand(verb)
	if !ok {
		t.Fatalf("no such verb %q", verb)
	}
	if raw, err := sc.build(args); err == nil {
		t.Fatalf("%s %v: expected error, got params %s", verb, args, raw)
	}
}

func u32(v uint32) *uint32 { return &v }

// Split: direction defaults to h, an optional pane becomes a pointer, bad
// direction and extra args are rejected.
func TestBuildSplit(t *testing.T) {
	buildOK(t, "split", nil, app.SplitParams{Direction: "h"})
	buildOK(t, "split", []string{"v"}, app.SplitParams{Direction: "v"})
	buildOK(t, "split", []string{"h", "2"}, app.SplitParams{Direction: "h", Pane: u32(2)})
	buildErr(t, "split", []string{"diagonal"})
	buildErr(t, "split", []string{"h", "notanumber"})
	buildErr(t, "split", []string{"h", "2", "3"})
}

// Required vs optional pane operands.
func TestBuildPaneOperands(t *testing.T) {
	buildOK(t, "focus", []string{"3"}, app.PaneParams{Pane: 3})
	buildErr(t, "focus", nil) // pane required
	buildErr(t, "focus", []string{"1", "2"})

	buildOK(t, "close", nil, app.OptPaneParams{}) // focused
	buildOK(t, "close", []string{"5"}, app.OptPaneParams{Pane: u32(5)})
	buildErr(t, "close", []string{"1", "2"})
}

// Direction verbs validate against the cardinal set.
func TestBuildDir(t *testing.T) {
	buildOK(t, "focus-dir", []string{"left"}, app.DirParams{Dir: "left"})
	buildOK(t, "swap", []string{"up"}, app.DirParams{Dir: "up"})
	buildErr(t, "swap", []string{"sideways"})
	buildErr(t, "focus-dir", nil)
}

// cycle: next by default, prev flips it.
func TestBuildCycle(t *testing.T) {
	buildOK(t, "cycle", nil, app.CycleParams{Next: true})
	buildOK(t, "cycle", []string{"next"}, app.CycleParams{Next: true})
	buildOK(t, "cycle", []string{"prev"}, app.CycleParams{Next: false})
	buildErr(t, "cycle", []string{"sideways"})
}

// Multi-word names join the remaining args; an empty name clears.
func TestBuildRename(t *testing.T) {
	buildOK(t, "rename-pane", []string{"2", "build", "server"},
		app.RenamePaneParams{Pane: 2, Name: "build server"})
	buildOK(t, "rename-pane", []string{"2", ""}, app.RenamePaneParams{Pane: 2, Name: ""})
	buildErr(t, "rename-pane", []string{"2"}) // name required
	buildOK(t, "rename-ws", []string{"w1", "front", "end"},
		app.RenameWorkspaceParams{ID: "w1", Name: "front end"})
}

// scroll / resize / read parse their numeric operands.
func TestBuildNumeric(t *testing.T) {
	buildOK(t, "scroll", []string{"1", "-10"}, app.ScrollParams{Pane: 1, Delta: -10})
	buildErr(t, "scroll", []string{"1", "down"})
	buildOK(t, "resize", []string{"r0", "0.6"}, app.ResizeBorderParams{Border: "r0", Ratio: 0.6})
	buildErr(t, "resize", []string{"r0", "wide"})
	buildOK(t, "read", []string{"1", "0", "0", "2", "5"},
		app.ReadParams{Pane: 1, Anchor: [2]uint32{0, 0}, Cursor: [2]uint32{2, 5}})
	buildErr(t, "read", []string{"1", "0", "0", "2"}) // needs 5
}

// capture defaults to the whole buffer (recent scope, 0 lines); a line count
// bounds it.
func TestBuildCapture(t *testing.T) {
	buildOK(t, "capture", []string{"1"}, app.CaptureParams{Pane: 1, Scope: 1})
	buildOK(t, "capture", []string{"1", "100"}, app.CaptureParams{Pane: 1, Scope: 1, Lines: 100})
	buildErr(t, "capture", nil)
	buildErr(t, "capture", []string{"1", "lots"})
}

// tab.list / tab operands.
func TestBuildTabs(t *testing.T) {
	buildOK(t, "tabs", nil, app.TabListParams{})
	buildOK(t, "tabs", []string{"w1"}, app.TabListParams{Workspace: "w1"})
	buildOK(t, "tab", []string{"2"}, app.TabParams{Num: 2})
	buildErr(t, "tab", []string{"two"})
	buildOK(t, "close-tab", nil, app.OptTabParams{})
	buildOK(t, "rename-tab", []string{"2", "logs"}, app.RenameTabParams{Num: 2, Name: "logs"})
}

// No-params verbs emit no params and reject any argument.
func TestBuildNoParams(t *testing.T) {
	for _, verb := range []string{"session", "panes", "workspaces", "last", "new-tab", "new-ws", "reload", "stop", "ping"} {
		sc, ok := lookupSubcommand(verb)
		if !ok {
			t.Fatalf("no such verb %q", verb)
		}
		raw, err := sc.build(nil)
		if err != nil || raw != nil {
			t.Errorf("%s: want (nil, nil), got (%s, %v)", verb, raw, err)
		}
		buildErr(t, verb, []string{"x"})
	}
}

// Every ergonomic verb maps to a real §7 method (or ping), has a builder, and a
// unique name — the registry can't advertise a verb the server would reject.
func TestSubcommandRegistryIntegrity(t *testing.T) {
	seen := map[string]bool{}
	names := app.CommandNames()
	for _, sc := range subcommands {
		if seen[sc.verb] {
			t.Errorf("duplicate verb %q", sc.verb)
		}
		seen[sc.verb] = true
		if sc.build == nil {
			t.Errorf("verb %q has no builder", sc.verb)
		}
		if sc.method != ctlproto.MethodPing && !slices.Contains(names, sc.method) {
			t.Errorf("verb %q maps to unknown method %q", sc.verb, sc.method)
		}
		if sc.synopsis == "" || sc.summary == "" {
			t.Errorf("verb %q missing help text", sc.verb)
		}
	}
}
