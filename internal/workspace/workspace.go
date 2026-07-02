package workspace

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/rohanthewiz/herdr-web/internal/layout"
)

// ErrPaneNotFound is returned by pane-addressed operations when no tab in the
// workspace contains the pane (Rust returns Option::None).
var ErrPaneNotFound = errors.New("workspace: pane not found")

// AheadBehind is a cached ahead/behind count against the branch upstream.
type AheadBehind struct {
	Ahead, Behind int
}

// TerminalCwdLookup resolves a terminal's current working directory. It
// stands in for Rust's (terminals map + TerminalRuntimeRegistry) pair so
// identity resolution stays free of backend imports.
type TerminalCwdLookup func(TerminalID) (string, bool)

// Workspace is a named collection of tabs (cf. workspace.rs). Rust's
// Deref<Target=Tab> onto the active tab is replaced by the explicit
// ActiveTab() accessor.
type Workspace struct {
	// ID is the stable public workspace identity ("w1"), independent of
	// display order.
	ID string
	// CustomName is a user-provided override; if set, auto-derived identity
	// stops updating. "" = auto-named.
	CustomName string
	// IdentityCwd is the fallback workspace identity source for tests, old
	// snapshots, or missing runtimes.
	IdentityCwd string
	// CachedGitBranch / CachedGitAheadBehind are plain optionals fed by the
	// deferred GitProvider seam (WS1 Stage 4); nil until wired.
	CachedGitBranch      *string
	CachedGitAheadBehind *AheadBehind
	// PublicPaneNumbers are the public pane numbers within this workspace.
	// Closed pane numbers are not reused.
	PublicPaneNumbers    map[layout.PaneID]int
	nextPublicPaneNumber int
	nextPublicTabNumber  int
	Tabs                 []*Tab
	activeTab            int
	spawner              PaneSpawner
}

// New creates a workspace with one tab and one root pane spawned through the
// seam. spec.PublicPaneID/PaneID/Cwd are filled in.
func New(s PaneSpawner, initialCwd string, spec SpawnSpec) (*Workspace, error) {
	id := GenerateWorkspaceID()
	spec.PublicPaneID = publicPaneIDForNumber(id, 1)
	tab, err := NewTab(s, 1, initialCwd, spec)
	if err != nil {
		return nil, err
	}
	return &Workspace{
		ID:          id,
		IdentityCwd: initialCwd,
		// CachedGitBranch stays nil until the GitProvider seam lands (Rust
		// calls git_branch(&initial_cwd) here).
		PublicPaneNumbers:    map[layout.PaneID]int{tab.RootPane: 1},
		nextPublicPaneNumber: 2,
		nextPublicTabNumber:  2,
		Tabs:                 []*Tab{tab},
		spawner:              s,
	}, nil
}

// publicPaneIDForNumber renders the stable public pane handle, e.g. "w1:p3".
func publicPaneIDForNumber(workspaceID string, paneNumber int) string {
	return workspaceID + ":p" + EncodePublicNumber(paneNumber)
}

// ActiveTab returns the active tab, or nil if the workspace has no tabs
// (which a live workspace never does).
func (w *Workspace) ActiveTab() *Tab {
	if w.activeTab < len(w.Tabs) {
		return w.Tabs[w.activeTab]
	}
	return nil
}

// ActiveTabIndex returns the active tab's position.
func (w *Workspace) ActiveTabIndex() int {
	return w.activeTab
}

// ActiveTabDisplayName returns the active tab's display name.
func (w *Workspace) ActiveTabDisplayName() (string, bool) {
	tab := w.ActiveTab()
	if tab == nil {
		return "", false
	}
	return tab.DisplayName(), true
}

// SwitchTab activates the tab at idx and marks all its panes seen.
func (w *Workspace) SwitchTab(idx int) {
	if idx < len(w.Tabs) {
		w.activeTab = idx
		for _, pane := range w.Tabs[idx].Panes {
			pane.Seen = true
		}
	}
}

// CreateTab appends a new tab (next public tab number, root pane numbered
// through the workspace counter) and returns its index. Mirroring Rust, the
// tab number is consumed even when the spawn fails.
func (w *Workspace) CreateTab(cwd string, spec SpawnSpec) (int, error) {
	number := w.nextPublicTabNumber
	w.nextPublicTabNumber++
	paneNumber := w.nextPublicPaneNumber
	spec.PublicPaneID = publicPaneIDForNumber(w.ID, paneNumber)

	tab, err := NewTab(w.spawner, number, cwd, spec)
	if err != nil {
		return 0, err
	}
	w.registerNewPaneWithNumber(tab.RootPane, paneNumber)
	w.Tabs = append(w.Tabs, tab)
	return len(w.Tabs) - 1, nil
}

// CloseTab removes the tab at idx (never the last tab), unregistering its
// panes and fixing up the active index. The caller despawns the terminals.
func (w *Workspace) CloseTab(idx int) bool {
	if len(w.Tabs) <= 1 || idx >= len(w.Tabs) {
		return false
	}
	tab := w.Tabs[idx]
	w.Tabs = append(w.Tabs[:idx], w.Tabs[idx+1:]...)
	for paneID := range tab.Panes {
		w.unregisterPane(paneID)
	}
	if w.activeTab >= len(w.Tabs) {
		w.activeTab = len(w.Tabs) - 1
	} else if idx <= w.activeTab && w.activeTab > 0 {
		w.activeTab--
	}
	return true
}

// MoveTab moves the tab at sourceIdx to insertIdx (an insertion point, so
// insertIdx == len(Tabs) means "to the end"). The active tab keeps its
// identity (tracked by root pane) across the move.
func (w *Workspace) MoveTab(sourceIdx, insertIdx int) bool {
	if sourceIdx >= len(w.Tabs) || insertIdx > len(w.Tabs) {
		return false
	}

	targetIdx := insertIdx
	if sourceIdx < insertIdx {
		targetIdx = insertIdx - 1
	}
	targetIdx = min(targetIdx, len(w.Tabs)-1)

	if sourceIdx == targetIdx {
		return false
	}

	var activeRootPane layout.PaneID
	haveActive := false
	if w.activeTab < len(w.Tabs) {
		activeRootPane = w.Tabs[w.activeTab].RootPane
		haveActive = true
	}
	tab := w.Tabs[sourceIdx]
	w.Tabs = append(w.Tabs[:sourceIdx], w.Tabs[sourceIdx+1:]...)
	w.Tabs = append(w.Tabs[:targetIdx], append([]*Tab{tab}, w.Tabs[targetIdx:]...)...)

	w.activeTab = targetIdx
	if haveActive {
		for i, t := range w.Tabs {
			if t.RootPane == activeRootPane {
				w.activeTab = i
				break
			}
		}
	}
	return true
}

// CloseActiveTab closes the active tab.
func (w *Workspace) CloseActiveTab() bool {
	return w.CloseTab(w.activeTab)
}

// SplitFocused splits the active tab's focused pane, assigning the next
// public pane number.
func (w *Workspace) SplitFocused(direction layout.Direction, spec SpawnSpec) (NewPane, error) {
	return w.splitActive(direction, nil, spec)
}

// SplitFocusedWithRatio is SplitFocused with a custom first-child ratio.
func (w *Workspace) SplitFocusedWithRatio(direction layout.Direction, ratio float32, spec SpawnSpec) (NewPane, error) {
	return w.splitActive(direction, &ratio, spec)
}

func (w *Workspace) splitActive(direction layout.Direction, ratio *float32, spec SpawnSpec) (NewPane, error) {
	paneNumber := w.nextPublicPaneNumber
	spec.PublicPaneID = publicPaneIDForNumber(w.ID, paneNumber)
	tab := w.ActiveTab()
	if tab == nil {
		return NewPane{}, errors.New("workspace: no active tab")
	}
	newPane, err := tab.splitFocusedWithSpawner(w.spawner, direction, ratio, spec)
	if err != nil {
		return NewPane{}, err
	}
	w.registerNewPaneWithNumber(newPane.PaneID, paneNumber)
	return newPane, nil
}

// SplitPane splits a specific pane wherever it lives, optionally focusing
// the new pane. Returns the tab index alongside the new pane.
func (w *Workspace) SplitPane(paneID layout.PaneID, direction layout.Direction, focusNewPane bool, spec SpawnSpec) (int, NewPane, error) {
	return w.splitPaneWithSpawner(paneID, direction, nil, focusNewPane, spec)
}

// SplitPaneWithRatio is SplitPane with a custom first-child ratio.
func (w *Workspace) SplitPaneWithRatio(paneID layout.PaneID, direction layout.Direction, ratio float32, focusNewPane bool, spec SpawnSpec) (int, NewPane, error) {
	return w.splitPaneWithSpawner(paneID, direction, &ratio, focusNewPane, spec)
}

func (w *Workspace) splitPaneWithSpawner(paneID layout.PaneID, direction layout.Direction, ratio *float32, focusNewPane bool, spec SpawnSpec) (int, NewPane, error) {
	tabIdx, ok := w.FindTabIndexForPane(paneID)
	if !ok {
		return 0, NewPane{}, ErrPaneNotFound
	}
	paneNumber := w.nextPublicPaneNumber
	spec.PublicPaneID = publicPaneIDForNumber(w.ID, paneNumber)
	tab := w.Tabs[tabIdx]
	previousFocus := tab.Layout.Focused()
	tab.Layout.FocusPane(paneID)
	newPane, err := tab.splitFocusedWithSpawner(w.spawner, direction, ratio, spec)
	if err != nil {
		tab.Layout.FocusPane(previousFocus)
		return tabIdx, NewPane{}, err
	}
	if !focusNewPane {
		tab.Layout.FocusPane(previousFocus)
	}
	w.registerNewPaneWithNumber(newPane.PaneID, paneNumber)
	return tabIdx, newPane, nil
}

// CloseFocused closes the focused pane (or the active tab when it holds the
// last pane). Returns true if the workspace itself should close.
func (w *Workspace) CloseFocused() bool {
	paneCount := 0
	if tab := w.ActiveTab(); tab != nil {
		paneCount = tab.Layout.PaneCount()
	}
	if paneCount <= 1 {
		return len(w.Tabs) <= 1 || w.closeActiveTabAndReport()
	}

	if tab := w.ActiveTab(); tab != nil {
		if detached, ok := tab.CloseFocused(); ok {
			w.unregisterPane(detached.PaneID)
		}
	}
	return false
}

// ClosePane closes a specific pane wherever it lives, collapsing its tab if
// it was the last pane. Returns true if the workspace should close.
func (w *Workspace) ClosePane(paneID layout.PaneID) bool {
	return w.detachPaneFromWorkspace(paneID)
}

// RemovePane removes a specific pane without any intent to terminate its
// terminal. Returns true if the workspace should close.
func (w *Workspace) RemovePane(paneID layout.PaneID) bool {
	return w.detachPaneFromWorkspace(paneID)
}

func (w *Workspace) detachPaneFromWorkspace(paneID layout.PaneID) bool {
	tabIdx, ok := w.FindTabIndexForPane(paneID)
	if !ok {
		return false
	}
	paneCount := w.Tabs[tabIdx].Layout.PaneCount()
	if paneCount <= 1 {
		if len(w.Tabs) <= 1 {
			return true
		}
		w.Tabs = append(w.Tabs[:tabIdx], w.Tabs[tabIdx+1:]...)
		w.unregisterPane(paneID)
		if w.activeTab >= len(w.Tabs) {
			w.activeTab = len(w.Tabs) - 1
		} else if tabIdx <= w.activeTab && w.activeTab > 0 {
			w.activeTab--
		}
		return false
	}

	if detached, ok := w.Tabs[tabIdx].ClosePane(paneID); ok {
		w.unregisterPane(detached.PaneID)
	}
	return false
}

// PublicPaneNumber returns the stable public number for a pane.
func (w *Workspace) PublicPaneNumber(paneID layout.PaneID) (int, bool) {
	n, ok := w.PublicPaneNumbers[paneID]
	return n, ok
}

// PublicTabNumber returns the stable public number of the tab at tabIdx.
func (w *Workspace) PublicTabNumber(tabIdx int) (int, bool) {
	if tabIdx >= len(w.Tabs) {
		return 0, false
	}
	return w.Tabs[tabIdx].Number, true
}

// SetCustomName pins the workspace's display name.
func (w *Workspace) SetCustomName(name string) {
	w.CustomName = name
}

// ResolvedIdentityCwd returns the workspace identity directory.
func (w *Workspace) ResolvedIdentityCwd() string {
	return w.IdentityCwd
}

// ResolvedIdentityCwdFrom prefers the first tab's root-pane terminal cwd
// (via the lookup seam), falling back to IdentityCwd.
func (w *Workspace) ResolvedIdentityCwdFrom(cwdFor TerminalCwdLookup) string {
	if len(w.Tabs) > 0 {
		tab := w.Tabs[0]
		if terminalID, ok := tab.TerminalIDFor(tab.RootPane); ok {
			if cwd, ok := cwdFor(terminalID); ok {
				return cwd
			}
		}
	}
	return w.IdentityCwd
}

// DisplayName returns the custom name, or a label derived from the identity cwd.
func (w *Workspace) DisplayName() string {
	if w.CustomName != "" {
		return w.CustomName
	}
	return deriveLabelFromCwd(w.IdentityCwd)
}

// DisplayNameFrom is DisplayName with live cwd resolution through the seam.
func (w *Workspace) DisplayNameFrom(cwdFor TerminalCwdLookup) string {
	if w.CustomName != "" {
		return w.CustomName
	}
	return deriveLabelFromCwd(w.ResolvedIdentityCwdFrom(cwdFor))
}

// FindTabIndexForPane locates the tab containing the given pane.
func (w *Workspace) FindTabIndexForPane(paneID layout.PaneID) (int, bool) {
	for i, tab := range w.Tabs {
		if _, ok := tab.Panes[paneID]; ok {
			return i, true
		}
	}
	return 0, false
}

// PaneStateFor returns the viewport state for a pane in any tab.
func (w *Workspace) PaneStateFor(paneID layout.PaneID) (*PaneState, bool) {
	for _, tab := range w.Tabs {
		if pane, ok := tab.Panes[paneID]; ok {
			return pane, true
		}
	}
	return nil, false
}

// TerminalIDForPane returns the terminal attached to a pane in any tab.
func (w *Workspace) TerminalIDForPane(paneID layout.PaneID) (TerminalID, bool) {
	for _, tab := range w.Tabs {
		if terminalID, ok := tab.TerminalIDFor(paneID); ok {
			return terminalID, true
		}
	}
	return "", false
}

// FocusedPaneID returns the active tab's focused pane.
func (w *Workspace) FocusedPaneID() (layout.PaneID, bool) {
	tab := w.ActiveTab()
	if tab == nil {
		return 0, false
	}
	return tab.Layout.Focused(), true
}

// registerNewPaneWithNumber records a pane's public number and advances the
// counter (numbers are never reused, even after out-of-order registration).
func (w *Workspace) registerNewPaneWithNumber(paneID layout.PaneID, number int) {
	w.PublicPaneNumbers[paneID] = number
	w.nextPublicPaneNumber = max(w.nextPublicPaneNumber, number+1)
}

func (w *Workspace) unregisterPane(paneID layout.PaneID) {
	delete(w.PublicPaneNumbers, paneID)
}

func (w *Workspace) closeActiveTabAndReport() bool {
	if len(w.Tabs) <= 1 {
		return true
	}
	w.CloseActiveTab()
	return false
}

// deriveLabelFromCwd derives a short workspace label from a directory.
// The Rust original consults git_repo_root first (workspace/git/discovery.rs)
// — that arrives with the GitProvider seam (WS1 Stage 4); until then labels
// come from $HOME ("~") or the path's base name.
func deriveLabelFromCwd(cwd string) string {
	if home := os.Getenv("HOME"); home != "" && cwd == home {
		return "~"
	}
	base := filepath.Base(cwd)
	if base == "." || base == string(os.PathSeparator) || base == "" {
		return cwd
	}
	return base
}
