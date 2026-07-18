// Session persistence support (WS3): serializable snapshots of Workspace/Tab
// state and the restore constructors that rebuild them. Snapshots carry the
// full public-numbering state — the counters, not just the live numbers,
// because numbers are never reused after a close and max+1 would resurrect a
// closed pane's handle. Restore re-spawns every pane's terminal through the
// PaneSpawner seam, preserving the invariant that every attached TerminalID
// came from the spawner.
package workspace

import (
	"fmt"
	"maps"

	"github.com/rohanthewiz/herdr-web/internal/layout"
)

// PaneSnapshot is a pane's durable viewport state (PaneState minus the
// terminal attachment, which restore re-derives through the spawner).
type PaneSnapshot struct {
	CustomName string `json:"custom_name,omitempty"`
	Seen       bool   `json:"seen"`
}

// TabSnapshot is a tab's durable state: identity, the layout tree with its
// split ratios, focus, zoom, and per-pane state.
type TabSnapshot struct {
	Number     int                                `json:"number"`
	CustomName string                             `json:"custom_name,omitempty"`
	RootPane   layout.PaneID                      `json:"root_pane"`
	Focus      layout.PaneID                      `json:"focus"`
	Zoomed     bool                               `json:"zoomed,omitempty"`
	Tree       *layout.SavedNode                  `json:"tree"`
	Panes      map[layout.PaneID]PaneSnapshot     `json:"panes"`
}

// Snapshot is a workspace's durable state.
type Snapshot struct {
	ID             string                `json:"id"`
	CustomName     string                `json:"custom_name,omitempty"`
	IdentityCwd    string                `json:"identity_cwd,omitempty"`
	ActiveTab      int                   `json:"active_tab"`
	PaneNumbers    map[layout.PaneID]int `json:"pane_numbers"`
	NextPaneNumber int                   `json:"next_pane_number"`
	NextTabNumber  int                   `json:"next_tab_number"`
	Tabs           []TabSnapshot         `json:"tabs"`
}

// Snapshot captures the tab's durable state.
func (t *Tab) Snapshot() TabSnapshot {
	panes := make(map[layout.PaneID]PaneSnapshot, len(t.Panes))
	for id, st := range t.Panes {
		panes[id] = PaneSnapshot{CustomName: st.CustomName, Seen: st.Seen}
	}
	return TabSnapshot{
		Number:     t.Number,
		CustomName: t.CustomName,
		RootPane:   t.RootPane,
		Focus:      t.Layout.Focused(),
		Zoomed:     t.Zoomed,
		Tree:       layout.SaveTree(t.Layout.Root()),
		Panes:      panes,
	}
}

// Snapshot captures the workspace's durable state.
func (w *Workspace) Snapshot() Snapshot {
	numbers := maps.Clone(w.PublicPaneNumbers)
	tabs := make([]TabSnapshot, 0, len(w.Tabs))
	for _, t := range w.Tabs {
		tabs = append(tabs, t.Snapshot())
	}
	return Snapshot{
		ID:             w.ID,
		CustomName:     w.CustomName,
		IdentityCwd:    w.IdentityCwd,
		ActiveTab:      w.activeTab,
		PaneNumbers:    numbers,
		NextPaneNumber: w.nextPublicPaneNumber,
		NextTabNumber:  w.nextPublicTabNumber,
		Tabs:           tabs,
	}
}

// Restore rebuilds a workspace from its snapshot, re-spawning each pane's
// terminal through the seam. It validates internal consistency (tab trees vs
// pane maps vs public numbers, focus and root membership, active index) and
// fails loudly on any mismatch — the caller falls back to a fresh session
// rather than driving PTYs from a corrupt model. FocusLast history (layout
// prev) is deliberately not persisted.
func Restore(s PaneSpawner, snap Snapshot) (*Workspace, error) {
	if len(snap.Tabs) == 0 {
		return nil, fmt.Errorf("workspace %s: no tabs", snap.ID)
	}
	if snap.ActiveTab < 0 || snap.ActiveTab >= len(snap.Tabs) {
		return nil, fmt.Errorf("workspace %s: active tab %d out of range", snap.ID, snap.ActiveTab)
	}

	ws := &Workspace{
		ID:                snap.ID,
		CustomName:        snap.CustomName,
		IdentityCwd:       snap.IdentityCwd,
		PublicPaneNumbers: make(map[layout.PaneID]int, len(snap.PaneNumbers)),
		activeTab:         snap.ActiveTab,
		spawner:           s,
	}

	maxPaneNumber, maxTabNumber := 0, 0
	for _, ts := range snap.Tabs {
		tab, err := restoreTab(s, snap, ts)
		if err != nil {
			return nil, fmt.Errorf("workspace %s: %w", snap.ID, err)
		}
		ws.Tabs = append(ws.Tabs, tab)
		maxTabNumber = max(maxTabNumber, ts.Number)
		for id := range ts.Panes {
			n, ok := snap.PaneNumbers[id]
			if !ok {
				return nil, fmt.Errorf("workspace %s: pane %d has no public number", snap.ID, id)
			}
			ws.PublicPaneNumbers[id] = n
			maxPaneNumber = max(maxPaneNumber, n)
		}
	}
	// The persisted counters are authoritative (numbers are never reused), but
	// never let a stale counter fall below what the live numbers imply.
	ws.nextPublicPaneNumber = max(snap.NextPaneNumber, maxPaneNumber+1)
	ws.nextPublicTabNumber = max(snap.NextTabNumber, maxTabNumber+1)
	return ws, nil
}

// restoreTab rebuilds one tab: tree, focus, zoom, pane states, re-spawned
// terminals.
func restoreTab(s PaneSpawner, wsnap Snapshot, snap TabSnapshot) (*Tab, error) {
	root, err := snap.Tree.Tree()
	if err != nil {
		return nil, fmt.Errorf("tab %d: %w", snap.Number, err)
	}
	lay := layout.FromSaved(root, snap.Focus)
	ids := lay.PaneIDs()
	if len(ids) == 0 {
		return nil, fmt.Errorf("tab %d: empty layout", snap.Number)
	}
	inTree := make(map[layout.PaneID]bool, len(ids))
	for _, id := range ids {
		inTree[id] = true
	}
	if len(snap.Panes) != len(ids) {
		return nil, fmt.Errorf("tab %d: %d panes in tree, %d in state", snap.Number, len(ids), len(snap.Panes))
	}
	for id := range snap.Panes {
		if !inTree[id] {
			return nil, fmt.Errorf("tab %d: pane %d has state but is not in the tree", snap.Number, id)
		}
	}
	if !inTree[snap.Focus] {
		return nil, fmt.Errorf("tab %d: focused pane %d not in the tree", snap.Number, snap.Focus)
	}
	if !inTree[snap.RootPane] {
		return nil, fmt.Errorf("tab %d: root pane %d not in the tree", snap.Number, snap.RootPane)
	}

	panes := make(map[layout.PaneID]*PaneState, len(ids))
	for _, id := range ids {
		ps := snap.Panes[id]
		spec := SpawnSpec{
			PaneID: id,
			Cwd:    wsnap.IdentityCwd,
			PublicPaneID: publicPaneIDForNumber(wsnap.ID, wsnap.PaneNumbers[id]),
		}
		terminalID, err := s.Spawn(spec)
		if err != nil {
			return nil, fmt.Errorf("tab %d: respawn pane %d: %w", snap.Number, id, err)
		}
		panes[id] = &PaneState{AttachedTerminalID: terminalID, Seen: ps.Seen, CustomName: ps.CustomName}
	}
	return &Tab{
		CustomName: snap.CustomName,
		Number:     snap.Number,
		RootPane:   snap.RootPane,
		Layout:     lay,
		Panes:      panes,
		Zoomed:     snap.Zoomed,
	}, nil
}
