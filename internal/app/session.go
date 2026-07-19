// Package app is the WS2 orchestrator's domain layer: the session state (an
// ordered set of workspaces, WS1) plus the command table (§7 of
// ai_docs/phase-c-ws9-protocol.md) that mutates it. It sits ABOVE the daemon
// seam (internal/orchestration) — it owns *what* the session looks like and
// *how* commands change it, never *how* PTYs are driven. That keeps it pure:
// no daemon, no I/O, no goroutines, so it unit-tests like the layout/workspace
// models it composes (the Rust src/app actions are the spec).
//
// The orchestrator runtime (the event-loop actor in cmd/gateway) owns exactly
// one Session and is its only caller, so Session needs no synchronization.
package app

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/rohanthewiz/herdr-web/internal/layout"
	"github.com/rohanthewiz/herdr-web/internal/workspace"
)

// Session is the multi-workspace session state. active indexes the workspace
// whose active tab is the current viewport (§8). All panes across all
// workspaces/tabs are live PTYs on the daemon; only the viewport's panes stream
// frames to the browser.
type Session struct {
	spawner    workspace.PaneSpawner
	cwd        string
	workspaces []*workspace.Workspace
	active     int
}

// NewSession starts a session with one workspace (one tab, one pane).
func NewSession(spawner workspace.PaneSpawner, cwd string) (*Session, error) {
	ws, err := workspace.New(spawner, cwd, workspace.SpawnSpec{})
	if err != nil {
		return nil, err
	}
	return &Session{spawner: spawner, cwd: cwd, workspaces: []*workspace.Workspace{ws}}, nil
}

// --- Queries -----------------------------------------------------------------

// Workspaces returns the ordered workspaces (for BuildLayout / the sidebar).
func (s *Session) Workspaces() []*workspace.Workspace { return s.workspaces }

// ActiveIndex is the active workspace's position.
func (s *Session) ActiveIndex() int { return s.active }

// ActiveWorkspace returns the active workspace.
func (s *Session) ActiveWorkspace() *workspace.Workspace { return s.workspaces[s.active] }

// Cwd is the session's default working directory for new panes.
func (s *Session) Cwd() string { return s.cwd }

// FocusedPane resolves the active workspace's active tab's focused pane.
func (s *Session) FocusedPane() (layout.PaneID, bool) {
	return s.ActiveWorkspace().FocusedPaneID()
}

// AllPaneIDs lists every pane across all workspaces and tabs — the panes the
// daemon must hold PTYs for.
func (s *Session) AllPaneIDs() []layout.PaneID {
	var ids []layout.PaneID
	for _, ws := range s.workspaces {
		for _, tab := range ws.Tabs {
			ids = append(ids, tab.Layout.PaneIDs()...)
		}
	}
	return ids
}

// VisiblePaneIDs lists panes in the current viewport (active workspace's active
// tab) — the only panes whose frames stream to the browser (§8). A zoomed tab
// shows only its focused pane, so that is the whole viewport.
func (s *Session) VisiblePaneIDs() []layout.PaneID {
	tab := s.ActiveWorkspace().ActiveTab()
	if tab == nil {
		return nil
	}
	if tab.Zoomed {
		return []layout.PaneID{tab.Layout.Focused()}
	}
	return tab.Layout.PaneIDs()
}

// PublicPaneID resolves a pane's public handle ("w1:p3") from whichever
// workspace owns it.
func (s *Session) PublicPaneID(id layout.PaneID) (string, bool) {
	for _, ws := range s.workspaces {
		if pub, ok := ws.PublicPaneID(id); ok {
			return pub, true
		}
	}
	return "", false
}

// PaneByPublicID is the reverse of PublicPaneID: it resolves a public handle
// back to the internal pane id. It accepts the two handle forms panes are given
// in their environment (HERDR_PANE_ID): the public "w1:p3" form, and the
// "p_<raw>" fallback that embeds the internal id directly (herdr's
// apply_pane_env emits it when no public id is known). Reports false for a
// handle that resolves to no live pane.
func (s *Session) PaneByPublicID(handle string) (layout.PaneID, bool) {
	if raw, ok := strings.CutPrefix(handle, "p_"); ok {
		n, err := strconv.ParseUint(raw, 10, 32)
		if err != nil {
			return 0, false
		}
		id := layout.PaneID(n)
		if _, ws := s.workspaceIndexOf(id); ws == nil {
			return 0, false
		}
		return id, true
	}
	for _, ws := range s.workspaces {
		for _, tab := range ws.Tabs {
			for _, id := range tab.Layout.PaneIDs() {
				if pub, ok := ws.PublicPaneID(id); ok && pub == handle {
					return id, true
				}
			}
		}
	}
	return 0, false
}

// --- Pane commands (§7) ------------------------------------------------------

// FocusPane focuses a pane within its owning tab (browser click-to-focus).
func (s *Session) FocusPane(id layout.PaneID) error {
	idx, ws := s.workspaceIndexOf(id)
	if ws == nil {
		return fmt.Errorf("unknown pane %d", id)
	}
	tabIdx, _ := ws.FindTabIndexForPane(id)
	ws.Tabs[tabIdx].Layout.FocusPane(id)
	s.active = idx
	return nil
}

// RevealPane brings a pane into the active viewport and focuses it: it makes the
// pane's owning workspace active, switches that workspace to the pane's tab, and
// focuses the pane within the tab. Unlike FocusPane (click-to-focus, always
// already within the current viewport), RevealPane may cross workspace AND tab
// boundaries — the agents sidebar is global (§8), so agent.focus can target a
// pane the browser cannot currently see.
func (s *Session) RevealPane(id layout.PaneID) error {
	idx, ws := s.workspaceIndexOf(id)
	if ws == nil {
		return fmt.Errorf("unknown pane %d", id)
	}
	tabIdx, _ := ws.FindTabIndexForPane(id)
	ws.SwitchTab(tabIdx)
	ws.Tabs[tabIdx].Layout.FocusPane(id)
	s.active = idx
	return nil
}

// FocusPaneDirection moves focus to the nearest pane in the given cardinal
// direction within the active tab, resolving neighbours from the viewport
// geometry (area). It reports whether focus actually moved: false with no error
// means no pane lies that way (a no-op). Like FocusPane it stays within the
// current viewport, so it never changes the active workspace/tab.
func (s *Session) FocusPaneDirection(nav layout.NavDirection, area layout.Rect) (bool, error) {
	tab := s.ActiveWorkspace().ActiveTab()
	if tab == nil {
		return false, errors.New("no active tab")
	}
	panes := tab.Layout.Panes(area)
	var focused *layout.PaneInfo
	for i := range panes {
		if panes[i].IsFocused {
			focused = &panes[i]
			break
		}
	}
	if focused == nil {
		return false, errors.New("no focused pane")
	}
	target, ok := layout.FindInDirection(focused, nav, panes)
	if !ok {
		return false, nil // no neighbour in that direction
	}
	tab.Layout.FocusPane(target)
	return true, nil
}

// CyclePane moves focus to the next (next=true) or previous pane in the active
// tab's in-order pane list, wrapping around. Reports whether focus moved (false
// only when the tab has a single pane). Like FocusPane it stays within the
// viewport.
func (s *Session) CyclePane(next bool) bool {
	tab := s.ActiveWorkspace().ActiveTab()
	if tab == nil {
		return false
	}
	ids := tab.Layout.PaneIDs()
	if len(ids) < 2 {
		return false
	}
	pos := slices.Index(ids, tab.Layout.Focused())
	if pos < 0 {
		pos = 0
	}
	n := len(ids)
	step := 1
	if !next {
		step = -1
	}
	tab.Layout.FocusPane(ids[(pos+step+n)%n])
	return true
}

// SwapPaneDirection swaps the focused pane with its nearest neighbour in the
// given direction within the active tab: the focused pane travels to the
// neighbour's slot and keeps focus. Reports whether a swap happened (false with
// no error means no neighbour that way). Needs the viewport geometry to resolve
// the neighbour.
func (s *Session) SwapPaneDirection(nav layout.NavDirection, area layout.Rect) (bool, error) {
	tab := s.ActiveWorkspace().ActiveTab()
	if tab == nil {
		return false, errors.New("no active tab")
	}
	panes := tab.Layout.Panes(area)
	var focused *layout.PaneInfo
	for i := range panes {
		if panes[i].IsFocused {
			focused = &panes[i]
			break
		}
	}
	if focused == nil {
		return false, errors.New("no focused pane")
	}
	target, ok := layout.FindInDirection(focused, nav, panes)
	if !ok {
		return false, nil // no neighbour in that direction
	}
	tab.Layout.SwapPanes(focused.ID, target)
	return true, nil
}

// ToggleZoom flips the active tab's zoom. When zooming, target (or the focused
// pane if nil) becomes the sole visible pane at full size; when already zoomed,
// it unzooms (target ignored). Reports the resulting zoom state.
func (s *Session) ToggleZoom(target *layout.PaneID) (bool, error) {
	tab := s.ActiveWorkspace().ActiveTab()
	if tab == nil {
		return false, errors.New("no active tab")
	}
	if !tab.Zoomed && target != nil {
		if !slices.Contains(tab.Layout.PaneIDs(), *target) {
			return false, fmt.Errorf("pane %d not in the active tab", *target)
		}
		tab.Layout.FocusPane(*target)
	}
	tab.Zoomed = !tab.Zoomed
	return tab.Zoomed, nil
}

// ResizeBorder sets the first-child ratio of the split identified by path
// (decoded from the wire border id) in the active tab, changing the sizes of
// the panes either side. A path that resolves to no split is a silent no-op.
func (s *Session) ResizeBorder(path []bool, ratio float32) error {
	tab := s.ActiveWorkspace().ActiveTab()
	if tab == nil {
		return errors.New("no active tab")
	}
	tab.Layout.SetRatioAt(path, ratio)
	return nil
}

// FocusLastPane toggles focus back to the active tab's previously-focused pane
// (LastPane). Reports whether focus moved. A focus-only change, like FocusPane.
func (s *Session) FocusLastPane() bool {
	tab := s.ActiveWorkspace().ActiveTab()
	if tab == nil {
		return false
	}
	return tab.Layout.FocusLast()
}

// RenamePane pins (or clears, with "") a pane's custom title, overriding the
// terminal-reported one. The pane may live in any workspace/tab.
func (s *Session) RenamePane(id layout.PaneID, name string) error {
	if st := s.paneState(id); st != nil {
		st.CustomName = name
		return nil
	}
	return fmt.Errorf("unknown pane %d", id)
}

// PaneCustomName returns a pane's custom title and whether the pane exists.
func (s *Session) PaneCustomName(id layout.PaneID) (string, bool) {
	if st := s.paneState(id); st != nil {
		return st.CustomName, true
	}
	return "", false
}

// PaneWorkspace returns the workspace owning a pane (nil when unknown) — the
// runtime resolves per-workspace spawn cwds through it.
func (s *Session) PaneWorkspace(id layout.PaneID) *workspace.Workspace {
	_, ws := s.workspaceIndexOf(id)
	return ws
}

// paneState finds a pane's viewport state across every workspace and tab.
func (s *Session) paneState(id layout.PaneID) *workspace.PaneState {
	for _, ws := range s.workspaces {
		for _, tab := range ws.Tabs {
			if st := tab.Panes[id]; st != nil {
				return st
			}
		}
	}
	return nil
}

// SplitPane splits target (the focused pane if nil) in dir, focusing the new
// pane, and returns its id.
func (s *Session) SplitPane(target *layout.PaneID, dir layout.Direction) (layout.PaneID, error) {
	id, err := s.resolvePaneTarget(target)
	if err != nil {
		return 0, err
	}
	_, ws := s.workspaceIndexOf(id)
	_, np, err := ws.SplitPane(id, dir, true, workspace.SpawnSpec{})
	if err != nil {
		return 0, err
	}
	return np.PaneID, nil
}

// ClosePane closes target (the focused pane if nil), returning the closed id.
// The session always keeps at least one pane; closing a workspace's last pane
// drops that workspace (when another remains).
func (s *Session) ClosePane(target *layout.PaneID) (layout.PaneID, error) {
	id, err := s.resolvePaneTarget(target)
	if err != nil {
		return 0, err
	}
	if s.totalPanes() <= 1 {
		return 0, errors.New("cannot close the last pane")
	}
	idx, ws := s.workspaceIndexOf(id)
	if ws.ClosePane(id) {
		// The workspace closed its last pane in its last tab → drop it.
		s.dropWorkspace(idx)
	}
	return id, nil
}

// --- Tab commands (§7) — operate on the active workspace ---------------------

// CreateTab appends a tab to the active workspace and switches to it. Returns
// the new tab's public number. The tab spawns in the workspace's identity cwd
// (a worktree workspace's checkout), falling back to the session cwd.
func (s *Session) CreateTab() (int, error) {
	ws := s.ActiveWorkspace()
	cwd := ws.IdentityCwd
	if cwd == "" {
		cwd = s.cwd
	}
	idx, err := ws.CreateTab(cwd, workspace.SpawnSpec{})
	if err != nil {
		return 0, err
	}
	ws.SwitchTab(idx)
	num, _ := ws.PublicTabNumber(idx)
	return num, nil
}

// CloseTab closes a tab (the active tab if num is nil) of the active workspace.
// Closing a workspace's last tab drops the workspace (when another remains).
func (s *Session) CloseTab(num *int) error {
	ws := s.ActiveWorkspace()
	idx := ws.ActiveTabIndex()
	if num != nil {
		i, ok := s.tabIndexByNumber(ws, *num)
		if !ok {
			return fmt.Errorf("unknown tab %d", *num)
		}
		idx = i
	}
	if len(ws.Tabs) > 1 {
		ws.CloseTab(idx)
		return nil
	}
	if len(s.workspaces) <= 1 {
		return errors.New("cannot close the last tab")
	}
	s.dropWorkspace(s.active)
	return nil
}

// FocusTab switches the active workspace to the tab with the given public
// number (a viewport change).
func (s *Session) FocusTab(num int) error {
	ws := s.ActiveWorkspace()
	idx, ok := s.tabIndexByNumber(ws, num)
	if !ok {
		return fmt.Errorf("unknown tab %d", num)
	}
	ws.SwitchTab(idx)
	return nil
}

// RenameTab pins (or clears, with "") a tab's display name.
func (s *Session) RenameTab(num int, name string) error {
	ws := s.ActiveWorkspace()
	idx, ok := s.tabIndexByNumber(ws, num)
	if !ok {
		return fmt.Errorf("unknown tab %d", num)
	}
	ws.Tabs[idx].SetCustomName(name)
	return nil
}

// MoveTab moves the active workspace's tab with public number num to the
// insertion point idx (a gap position, 0..=len: len means "to the end").
// Reports whether the order actually changed (false = a no-op move).
func (s *Session) MoveTab(num, insertIdx int) (bool, error) {
	ws := s.ActiveWorkspace()
	srcIdx, ok := s.tabIndexByNumber(ws, num)
	if !ok {
		return false, fmt.Errorf("unknown tab %d", num)
	}
	if insertIdx < 0 || insertIdx > len(ws.Tabs) {
		return false, fmt.Errorf("bad insert index %d", insertIdx)
	}
	return ws.MoveTab(srcIdx, insertIdx), nil
}

// --- Workspace commands (§7) -------------------------------------------------

// CreateWorkspace appends a new workspace (one tab, one pane) rooted at the
// session cwd and makes it active. Returns its public id ("w2").
func (s *Session) CreateWorkspace() (string, error) {
	return s.CreateWorkspaceAt(s.cwd)
}

// CreateWorkspaceAt is CreateWorkspace with an explicit root directory — the
// worktree commands open workspaces on a checkout, and the cwd becomes the
// workspace's IdentityCwd so every pane spawned in it inherits the checkout.
func (s *Session) CreateWorkspaceAt(cwd string) (string, error) {
	ws, err := workspace.New(s.spawner, cwd, workspace.SpawnSpec{})
	if err != nil {
		return "", err
	}
	s.workspaces = append(s.workspaces, ws)
	s.active = len(s.workspaces) - 1
	return ws.ID, nil
}

// CloseWorkspace drops a workspace (the active one if id is nil); the session
// always keeps at least one.
func (s *Session) CloseWorkspace(id *string) error {
	if len(s.workspaces) <= 1 {
		return errors.New("cannot close the last workspace")
	}
	idx := s.active
	if id != nil {
		i, ok := s.workspaceIndexByID(*id)
		if !ok {
			return fmt.Errorf("unknown workspace %s", *id)
		}
		idx = i
	}
	s.dropWorkspace(idx)
	return nil
}

// FocusWorkspace makes the workspace with the given id active (a viewport
// change).
func (s *Session) FocusWorkspace(id string) error {
	i, ok := s.workspaceIndexByID(id)
	if !ok {
		return fmt.Errorf("unknown workspace %s", id)
	}
	s.active = i
	return nil
}

// RenameWorkspace pins (or clears, with "") a workspace's display name.
func (s *Session) RenameWorkspace(id, name string) error {
	i, ok := s.workspaceIndexByID(id)
	if !ok {
		return fmt.Errorf("unknown workspace %s", id)
	}
	s.workspaces[i].SetCustomName(name)
	return nil
}

// MoveWorkspace moves the workspace with the given public id to the insertion
// point idx (a gap position, 0..=len: len means "to the end"). The active
// workspace keeps its identity across the move. Reports whether the order
// actually changed (false = a no-op move).
func (s *Session) MoveWorkspace(id string, insertIdx int) (bool, error) {
	srcIdx, ok := s.workspaceIndexByID(id)
	if !ok {
		return false, fmt.Errorf("unknown workspace %s", id)
	}
	if insertIdx < 0 || insertIdx > len(s.workspaces) {
		return false, fmt.Errorf("bad insert index %d", insertIdx)
	}
	targetIdx := insertIdx
	if srcIdx < insertIdx {
		targetIdx = insertIdx - 1
	}
	targetIdx = min(targetIdx, len(s.workspaces)-1)
	if srcIdx == targetIdx {
		return false, nil
	}
	activeWS := s.workspaces[s.active]
	ws := s.workspaces[srcIdx]
	s.workspaces = append(s.workspaces[:srcIdx], s.workspaces[srcIdx+1:]...)
	s.workspaces = append(s.workspaces[:targetIdx],
		append([]*workspace.Workspace{ws}, s.workspaces[targetIdx:]...)...)
	for i, w := range s.workspaces {
		if w == activeWS {
			s.active = i
			break
		}
	}
	return true, nil
}

// --- Internal helpers --------------------------------------------------------

func (s *Session) resolvePaneTarget(target *layout.PaneID) (layout.PaneID, error) {
	if target != nil {
		if _, ws := s.workspaceIndexOf(*target); ws == nil {
			return 0, fmt.Errorf("unknown pane %d", *target)
		}
		return *target, nil
	}
	id, ok := s.FocusedPane()
	if !ok {
		return 0, errors.New("no focused pane")
	}
	return id, nil
}

func (s *Session) totalPanes() int {
	n := 0
	for _, ws := range s.workspaces {
		for _, tab := range ws.Tabs {
			n += tab.Layout.PaneCount()
		}
	}
	return n
}

func (s *Session) workspaceIndexOf(id layout.PaneID) (int, *workspace.Workspace) {
	for i, ws := range s.workspaces {
		if _, ok := ws.FindTabIndexForPane(id); ok {
			return i, ws
		}
	}
	return -1, nil
}

func (s *Session) workspaceIndexByID(id string) (int, bool) {
	for i, ws := range s.workspaces {
		if ws.ID == id {
			return i, true
		}
	}
	return -1, false
}

func (s *Session) tabIndexByNumber(ws *workspace.Workspace, num int) (int, bool) {
	for i, tab := range ws.Tabs {
		if tab.Number == num {
			return i, true
		}
	}
	return -1, false
}

// dropWorkspace removes the workspace at idx and keeps active valid.
func (s *Session) dropWorkspace(idx int) {
	s.workspaces = append(s.workspaces[:idx], s.workspaces[idx+1:]...)
	switch {
	case s.active >= len(s.workspaces):
		s.active = len(s.workspaces) - 1
	case idx < s.active:
		s.active--
	}
}
