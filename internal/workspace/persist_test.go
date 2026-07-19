package workspace

import (
	"encoding/json"
	"testing"

	"github.com/rohanthewiz/herdr-web/internal/layout"
)

// buildWorkspace makes a workspace with two tabs — tab 1 split twice with a
// custom ratio, tab 2 single-pane — plus custom names, zoom off, and a closed
// pane so the numbering counter is ahead of the live numbers.
func buildWorkspace(t *testing.T) *Workspace {
	t.Helper()
	ws, err := New(recordingSpawner(), "/tmp/wsroot", SpawnSpec{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := ws.SplitFocused(layout.Horizontal, SpawnSpec{}); err != nil {
		t.Fatalf("split: %v", err)
	}
	np, err := ws.SplitFocusedWithRatio(layout.Vertical, 0.3, SpawnSpec{})
	if err != nil {
		t.Fatalf("split2: %v", err)
	}
	// Close the newest pane: its public number must never be reused.
	ws.ClosePane(np.PaneID)
	if _, err := ws.CreateTab("/tmp/tab2", SpawnSpec{}); err != nil {
		t.Fatalf("CreateTab: %v", err)
	}
	ws.SetCustomName("myws")
	ws.Tabs[0].SetCustomName("build")
	return ws
}

// recordingSpawner mirrors gateway's modelSpawner: pure, deterministic ids.
func recordingSpawner() PaneSpawner { return modelStubSpawner{} }

type modelStubSpawner struct{}

func (modelStubSpawner) Spawn(spec SpawnSpec) (TerminalID, error) {
	return TerminalID("term_" + EncodePublicNumber(int(spec.PaneID))), nil
}
func (modelStubSpawner) Despawn(TerminalID) {}

func TestWorkspaceSnapshotRoundTrip(t *testing.T) {
	ws := buildWorkspace(t)

	snap := ws.Snapshot()
	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back Snapshot
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	restored, err := Restore(recordingSpawner(), back)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}

	if restored.ID != ws.ID || restored.CustomName != "myws" || restored.IdentityCwd != "/tmp/wsroot" {
		t.Fatalf("identity: got %s/%s/%s", restored.ID, restored.CustomName, restored.IdentityCwd)
	}
	if restored.ActiveTabIndex() != ws.ActiveTabIndex() {
		t.Fatalf("active tab: got %d want %d", restored.ActiveTabIndex(), ws.ActiveTabIndex())
	}
	if len(restored.Tabs) != len(ws.Tabs) {
		t.Fatalf("tabs: got %d want %d", len(restored.Tabs), len(ws.Tabs))
	}
	for i, tab := range ws.Tabs {
		rt := restored.Tabs[i]
		if rt.Number != tab.Number || rt.CustomName != tab.CustomName ||
			rt.RootPane != tab.RootPane || rt.Zoomed != tab.Zoomed {
			t.Fatalf("tab %d: got %+v want %+v", i, rt, tab)
		}
		if rt.Layout.Focused() != tab.Layout.Focused() {
			t.Fatalf("tab %d focus: got %d want %d", i, rt.Layout.Focused(), tab.Layout.Focused())
		}
		gotIDs, wantIDs := rt.Layout.PaneIDs(), tab.Layout.PaneIDs()
		if len(gotIDs) != len(wantIDs) {
			t.Fatalf("tab %d panes: got %v want %v", i, gotIDs, wantIDs)
		}
		for j := range gotIDs {
			if gotIDs[j] != wantIDs[j] {
				t.Fatalf("tab %d panes: got %v want %v", i, gotIDs, wantIDs)
			}
			// Terminals were re-spawned through the seam, not persisted.
			id := gotIDs[j]
			if rt.Panes[id].AttachedTerminalID == "" {
				t.Fatalf("tab %d pane %d: no terminal attached", i, id)
			}
		}
	}

	// Public numbering survives, and the counter is ahead of the closed pane's
	// number: the next split must NOT reuse it.
	for id, n := range ws.PublicPaneNumbers {
		if got, ok := restored.PublicPaneNumber(id); !ok || got != n {
			t.Fatalf("pane %d number: got %d/%v want %d", id, got, ok, n)
		}
	}
	np, err := restored.SplitFocused(layout.Horizontal, SpawnSpec{})
	if err != nil {
		t.Fatalf("split after restore: %v", err)
	}
	if n, _ := restored.PublicPaneNumber(np.PaneID); n != 5 {
		// Panes 1..3 existed, pane 3 closed (its number burned), tab 2's root
		// took 4 — the counter must hand out 5 next, not resurrect 3.
		t.Fatalf("post-restore number: got %d want 5", n)
	}
}

// A snapshot whose pane map disagrees with its tree must fail to restore.
func TestRestoreRejectsInconsistency(t *testing.T) {
	ws := buildWorkspace(t)
	snap := ws.Snapshot()

	// Corrupt: point the first tab's focus at a pane that isn't in the tree.
	snap.Tabs[0].Focus = layout.PaneID(9999)
	if _, err := Restore(recordingSpawner(), snap); err == nil {
		t.Fatal("expected error for out-of-tree focus")
	}

	snap = ws.Snapshot()
	snap.ActiveTab = 7
	if _, err := Restore(recordingSpawner(), snap); err == nil {
		t.Fatal("expected error for out-of-range active tab")
	}

	snap = ws.Snapshot()
	delete(snap.PaneNumbers, ws.Tabs[0].RootPane)
	if _, err := Restore(recordingSpawner(), snap); err == nil {
		t.Fatal("expected error for missing public number")
	}
}
