package app

import (
	"encoding/json"
	"testing"

	"github.com/rohanthewiz/herdr-web/internal/layout"
	"github.com/rohanthewiz/herdr-web/internal/workspace"
)

// buildSession makes a two-workspace session: w1 with a split tab plus a second
// tab, w2 single-pane, with custom names and a non-default focus/active state.
func buildSession(t *testing.T) *Session {
	t.Helper()
	s, err := NewSession(stubSpawner{}, "/tmp/sess")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if _, err := s.SplitPane(nil, layout.Horizontal); err != nil {
		t.Fatalf("split: %v", err)
	}
	if _, err := s.CreateTab(); err != nil {
		t.Fatalf("tab: %v", err)
	}
	if _, err := s.CreateWorkspace(); err != nil {
		t.Fatalf("workspace: %v", err)
	}
	if err := s.RenameWorkspace(s.ActiveWorkspace().ID, "scratch"); err != nil {
		t.Fatalf("rename ws: %v", err)
	}
	// Land back on w1's first (split) tab with a custom pane name.
	if err := s.FocusWorkspace(s.workspaces[0].ID); err != nil {
		t.Fatalf("focus ws: %v", err)
	}
	if err := s.FocusTab(1); err != nil {
		t.Fatalf("focus tab: %v", err)
	}
	ids := s.ActiveWorkspace().ActiveTab().Layout.PaneIDs()
	if err := s.RenamePane(ids[0], "builder"); err != nil {
		t.Fatalf("rename pane: %v", err)
	}
	s.ActiveWorkspace().ActiveTab().Layout.FocusPane(ids[0])
	return s
}

type stubSpawner struct{}

func (stubSpawner) Spawn(spec workspace.SpawnSpec) (workspace.TerminalID, error) {
	return workspace.TerminalID("t"), nil
}
func (stubSpawner) Despawn(workspace.TerminalID) {}

func TestSessionSnapshotRoundTrip(t *testing.T) {
	s := buildSession(t)

	snap := s.Snapshot()
	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back Snapshot
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	r, err := RestoreSession(stubSpawner{}, back)
	if err != nil {
		t.Fatalf("RestoreSession: %v", err)
	}

	if r.Cwd() != s.Cwd() || r.ActiveIndex() != s.ActiveIndex() {
		t.Fatalf("session: got %s/%d want %s/%d", r.Cwd(), r.ActiveIndex(), s.Cwd(), s.ActiveIndex())
	}
	gotPanes, wantPanes := r.AllPaneIDs(), s.AllPaneIDs()
	if len(gotPanes) != len(wantPanes) {
		t.Fatalf("panes: got %v want %v", gotPanes, wantPanes)
	}
	for i := range gotPanes {
		if gotPanes[i] != wantPanes[i] {
			t.Fatalf("panes: got %v want %v", gotPanes, wantPanes)
		}
	}
	gotF, _ := r.FocusedPane()
	wantF, _ := s.FocusedPane()
	if gotF != wantF {
		t.Fatalf("focus: got %d want %d", gotF, wantF)
	}
	for _, id := range wantPanes {
		gotPub, _ := r.PublicPaneID(id)
		wantPub, _ := s.PublicPaneID(id)
		if gotPub != wantPub {
			t.Fatalf("pane %d handle: got %s want %s", id, gotPub, wantPub)
		}
	}
	if name, ok := r.PaneCustomName(wantF); !ok || name != "builder" {
		t.Fatalf("custom name: got %q/%v", name, ok)
	}
	if r.workspaces[1].CustomName != "scratch" {
		t.Fatalf("workspace name: got %q", r.workspaces[1].CustomName)
	}

	// New ids after restore must not collide with restored ones.
	np, err := r.SplitPane(nil, layout.Vertical)
	if err != nil {
		t.Fatalf("split after restore: %v", err)
	}
	for _, id := range wantPanes {
		if np == id {
			t.Fatalf("new pane %d reused a restored id", np)
		}
	}
	ws, err := r.CreateWorkspace()
	if err != nil {
		t.Fatalf("workspace after restore: %v", err)
	}
	for _, w := range s.workspaces {
		if ws == w.ID {
			t.Fatalf("new workspace %s reused a restored id", ws)
		}
	}
}

func TestRestoreSessionRejectsCorruption(t *testing.T) {
	s := buildSession(t)

	snap := s.Snapshot()
	snap.Active = 5
	if _, err := RestoreSession(stubSpawner{}, snap); err == nil {
		t.Fatal("expected error for out-of-range active workspace")
	}

	snap = s.Snapshot()
	snap.Workspaces = nil
	if _, err := RestoreSession(stubSpawner{}, snap); err == nil {
		t.Fatal("expected error for no workspaces")
	}

	// Duplicate a pane id across workspaces: the daemon keys PTYs by pane id,
	// so a model with duplicates must be refused.
	snap = s.Snapshot()
	dup := snap.Workspaces[0].Tabs[0]
	snap.Workspaces[1].Tabs[0] = dup
	snap.Workspaces[1].PaneNumbers = snap.Workspaces[0].PaneNumbers
	if _, err := RestoreSession(stubSpawner{}, snap); err == nil {
		t.Fatal("expected error for duplicate pane ids")
	}
}
