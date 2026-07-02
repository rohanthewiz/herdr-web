package workspace

import (
	"os"
	"strconv"

	"github.com/rohanthewiz/herdr-web/internal/layout"
)

// PaneState is the viewport state for a pane (cf. pane/state.rs). Terminal
// identity, cwd, labels, and agent metadata live with the terminal backend.
type PaneState struct {
	AttachedTerminalID TerminalID
	// Seen is whether the user has seen this pane since its last state
	// change to Idle. False = "Done" (agent finished while the user was in
	// another workspace).
	Seen bool
}

// NewPaneState returns a pane state attached to the given terminal, marked seen.
func NewPaneState(attached TerminalID) *PaneState {
	return &PaneState{AttachedTerminalID: attached, Seen: true}
}

// Tab is one pane tree within a workspace (cf. workspace/tab.rs). It holds
// pane identity and layout only; terminals live behind the PaneSpawner.
type Tab struct {
	// CustomName overrides the numeric display name; "" = auto-named.
	CustomName string
	// Number is the stable public tab number (not reused after close).
	Number int
	// RootPane is the identity source for this tab's pane tree.
	RootPane layout.PaneID
	Layout   *layout.TileLayout
	// Panes holds pane viewport state — always present, testable without PTYs.
	Panes  map[layout.PaneID]*PaneState
	Zoomed bool
}

// NewTab creates a tab with a single root pane, spawning its terminal via
// the seam (cf. Tab::new/new_with_runtime). spec.PaneID/Cwd are filled in.
func NewTab(s PaneSpawner, number int, initialCwd string, spec SpawnSpec) (*Tab, error) {
	lay, rootID := layout.New()
	spec.PaneID = rootID
	spec.Cwd = initialCwd
	terminalID, err := s.Spawn(spec)
	if err != nil {
		return nil, err
	}
	return &Tab{
		Number:   number,
		RootPane: rootID,
		Layout:   lay,
		Panes:    map[layout.PaneID]*PaneState{rootID: NewPaneState(terminalID)},
	}, nil
}

// DisplayName returns the custom name, or the public tab number.
func (t *Tab) DisplayName() string {
	if t.CustomName != "" {
		return t.CustomName
	}
	return strconv.Itoa(t.Number)
}

// IsAutoNamed reports whether the tab still derives its name from its number.
func (t *Tab) IsAutoNamed() bool {
	return t.CustomName == ""
}

// SetCustomName pins the tab's display name.
func (t *Tab) SetCustomName(name string) {
	t.CustomName = name
}

// SplitFocused splits the focused pane 50/50 and spawns a terminal for the
// new pane.
func (t *Tab) SplitFocused(s PaneSpawner, direction layout.Direction, spec SpawnSpec) (NewPane, error) {
	return t.splitFocusedWithSpawner(s, direction, nil, spec)
}

// SplitFocusedWithRatio splits the focused pane with a custom first-child ratio.
func (t *Tab) SplitFocusedWithRatio(s PaneSpawner, direction layout.Direction, ratio float32, spec SpawnSpec) (NewPane, error) {
	return t.splitFocusedWithSpawner(s, direction, &ratio, spec)
}

// splitFocusedWithSpawner is the single split path (cf.
// split_focused_with_runtime). On spawn failure the layout change is rolled
// back and the previous focus restored.
func (t *Tab) splitFocusedWithSpawner(s PaneSpawner, direction layout.Direction, ratio *float32, spec SpawnSpec) (NewPane, error) {
	previousFocus := t.Layout.Focused()
	var newID layout.PaneID
	if ratio != nil {
		newID = t.Layout.SplitFocusedWithRatio(direction, *ratio)
	} else {
		newID = t.Layout.SplitFocused(direction)
	}
	if spec.Cwd == "" {
		wd, err := os.Getwd()
		if err != nil {
			wd = "/"
		}
		spec.Cwd = wd
	}
	spec.PaneID = newID
	terminalID, err := s.Spawn(spec)
	if err != nil {
		t.Layout.CloseFocused()
		t.Layout.FocusPane(previousFocus)
		return NewPane{}, err
	}
	t.Panes[newID] = NewPaneState(terminalID)
	t.Zoomed = false
	return NewPane{PaneID: newID, TerminalID: terminalID}, nil
}

// CloseFocused detaches the focused pane. The caller owns the returned
// terminal (despawn it or hand it off).
func (t *Tab) CloseFocused() (DetachedPane, bool) {
	return t.detachPane(t.Layout.Focused())
}

// ClosePane detaches a specific pane.
func (t *Tab) ClosePane(paneID layout.PaneID) (DetachedPane, bool) {
	return t.detachPane(paneID)
}

// RemovePane detaches a specific pane without any intent to terminate its
// terminal (e.g. moving it elsewhere). Identical bookkeeping to ClosePane;
// the difference is what the caller does with the terminal.
func (t *Tab) RemovePane(paneID layout.PaneID) (DetachedPane, bool) {
	return t.detachPane(paneID)
}

// detachPane removes a pane from layout + state, promoting a new root pane
// if the tab's identity pane was closed. Returns false for the last pane.
func (t *Tab) detachPane(paneID layout.PaneID) (DetachedPane, bool) {
	if t.Layout.PaneCount() <= 1 {
		return DetachedPane{}, false
	}

	nextRoot, promote := t.promotedRootIfNeeded(paneID)

	if t.Layout.Focused() == paneID {
		t.Layout.CloseFocused()
	} else {
		prevFocus := t.Layout.Focused()
		t.Layout.FocusPane(paneID)
		t.Layout.CloseFocused()
		t.Layout.FocusPane(prevFocus)
	}

	pane, ok := t.Panes[paneID]
	if !ok {
		return DetachedPane{}, false
	}
	delete(t.Panes, paneID)
	t.Zoomed = false
	if promote {
		t.RootPane = nextRoot
	}
	return DetachedPane{PaneID: paneID, TerminalID: pane.AttachedTerminalID}, true
}

// promotedRootIfNeeded picks the surviving pane that becomes the tab's
// identity source when the current root pane is closing.
func (t *Tab) promotedRootIfNeeded(closing layout.PaneID) (layout.PaneID, bool) {
	if t.RootPane != closing {
		return 0, false
	}
	for _, id := range t.Layout.PaneIDs() {
		if id != closing {
			return id, true
		}
	}
	return 0, false
}

// TerminalIDFor returns the terminal attached to the given pane.
func (t *Tab) TerminalIDFor(paneID layout.PaneID) (TerminalID, bool) {
	pane, ok := t.Panes[paneID]
	if !ok {
		return "", false
	}
	return pane.AttachedTerminalID, true
}
