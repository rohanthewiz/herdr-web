package workspace

import (
	"fmt"
	"testing"

	"github.com/rohanthewiz/herdr-web/internal/layout"
)

// fakeSpawner is the test double for the PaneSpawner seam (mirrors Rust's
// test_new/test_split/test_add_tab, which skip runtime creation).
type fakeSpawner struct {
	nextTerm  int
	spawned   []SpawnSpec
	despawned []TerminalID
}

func (f *fakeSpawner) Spawn(spec SpawnSpec) (TerminalID, error) {
	f.nextTerm++
	f.spawned = append(f.spawned, spec)
	return TerminalID(fmt.Sprintf("term_test_%d", f.nextTerm)), nil
}

func (f *fakeSpawner) Despawn(id TerminalID) {
	f.despawned = append(f.despawned, id)
}

// testWorkspace mirrors Workspace::test_new: one tab, one root pane, custom name.
func testWorkspace(t *testing.T, name string) *Workspace {
	t.Helper()
	ws, err := New(&fakeSpawner{}, "/herdr-test/ws", SpawnSpec{Rows: 24, Cols: 80})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ws.CustomName = name
	return ws
}

// mustSplitFocused mirrors Workspace::test_split.
func mustSplitFocused(t *testing.T, ws *Workspace, direction layout.Direction) layout.PaneID {
	t.Helper()
	newPane, err := ws.SplitFocused(direction, SpawnSpec{Rows: 24, Cols: 80})
	if err != nil {
		t.Fatalf("SplitFocused: %v", err)
	}
	return newPane.PaneID
}

// mustAddTab mirrors Workspace::test_add_tab.
func mustAddTab(t *testing.T, ws *Workspace, name string) int {
	t.Helper()
	idx, err := ws.CreateTab("/herdr-test/ws", SpawnSpec{Rows: 24, Cols: 80})
	if err != nil {
		t.Fatalf("CreateTab: %v", err)
	}
	if name != "" {
		ws.Tabs[idx].SetCustomName(name)
	}
	return idx
}

// publicTabNumberForPane mirrors the Rust test-only helper.
func publicTabNumberForPane(ws *Workspace, paneID layout.PaneID) (int, bool) {
	tabIdx, ok := ws.FindTabIndexForPane(paneID)
	if !ok {
		return 0, false
	}
	return ws.PublicTabNumber(tabIdx)
}

func assertPaneNumber(t *testing.T, ws *Workspace, paneID layout.PaneID, want int, wantOK bool) {
	t.Helper()
	got, ok := ws.PublicPaneNumber(paneID)
	if ok != wantOK || (ok && got != want) {
		t.Fatalf("PublicPaneNumber(%d) = (%d, %v), want (%d, %v)", paneID, got, ok, want, wantOK)
	}
}

func assertTabNumberForPane(t *testing.T, ws *Workspace, paneID layout.PaneID, want int) {
	t.Helper()
	got, ok := publicTabNumberForPane(ws, paneID)
	if !ok || got != want {
		t.Fatalf("publicTabNumberForPane(%d) = (%d, %v), want (%d, true)", paneID, got, ok, want)
	}
}

func TestPanePublicNumbersAreStableAndNotReusedAfterClose(t *testing.T) {
	ws := testWorkspace(t, "test")
	root := ws.Tabs[0].RootPane
	second := mustSplitFocused(t, ws, layout.Horizontal)
	third := mustSplitFocused(t, ws, layout.Vertical)

	assertPaneNumber(t, ws, root, 1, true)
	assertPaneNumber(t, ws, second, 2, true)
	assertPaneNumber(t, ws, third, 3, true)

	if ws.ClosePane(second) {
		t.Fatal("closing a pane in a multi-pane tab should not close the workspace")
	}

	assertPaneNumber(t, ws, root, 1, true)
	assertPaneNumber(t, ws, second, 0, false)
	assertPaneNumber(t, ws, third, 3, true)

	fourth := mustSplitFocused(t, ws, layout.Horizontal)
	assertPaneNumber(t, ws, fourth, 4, true)
}

func TestTabPublicNumbersAreStableAndNotReusedAfterClose(t *testing.T) {
	ws := testWorkspace(t, "test")
	firstRoot := ws.Tabs[0].RootPane
	secondTab := mustAddTab(t, ws, "")
	secondRoot := ws.Tabs[secondTab].RootPane
	thirdTab := mustAddTab(t, ws, "")
	thirdRoot := ws.Tabs[thirdTab].RootPane

	assertTabNumberForPane(t, ws, firstRoot, 1)
	assertTabNumberForPane(t, ws, secondRoot, 2)
	assertTabNumberForPane(t, ws, thirdRoot, 3)

	if !ws.CloseTab(secondTab) {
		t.Fatal("closing a non-last tab should succeed")
	}

	assertTabNumberForPane(t, ws, firstRoot, 1)
	assertTabNumberForPane(t, ws, thirdRoot, 3)

	fourthTab := mustAddTab(t, ws, "")
	fourthRoot := ws.Tabs[fourthTab].RootPane
	assertTabNumberForPane(t, ws, fourthRoot, 4)
}

func TestWorkspaceIdentityFollowsFirstTabRootPaneCwd(t *testing.T) {
	ws := testWorkspace(t, "ignored")
	ws.CustomName = ""
	rootPane := ws.Tabs[0].RootPane
	terminalID, ok := ws.Tabs[0].TerminalIDFor(rootPane)
	if !ok {
		t.Fatal("root pane should have a terminal")
	}
	lookup := func(id TerminalID) (string, bool) {
		if id == terminalID {
			return "/herdr-test/pion", true
		}
		return "", false
	}

	if got := ws.DisplayNameFrom(lookup); got != "pion" {
		t.Fatalf("DisplayNameFrom = %q, want %q", got, "pion")
	}
	if got := ws.ResolvedIdentityCwdFrom(lookup); got != "/herdr-test/pion" {
		t.Fatalf("ResolvedIdentityCwdFrom = %q, want %q", got, "/herdr-test/pion")
	}
}

func TestMovingTabKeepsActiveIdentityAndStableTabNumbers(t *testing.T) {
	ws := testWorkspace(t, "test")
	movedRoot := ws.Tabs[0].RootPane
	mustAddTab(t, ws, "foo")
	finalAutoIdx := mustAddTab(t, ws, "")
	activeRoot := ws.Tabs[finalAutoIdx].RootPane
	ws.SwitchTab(finalAutoIdx)

	if !ws.MoveTab(0, len(ws.Tabs)) {
		t.Fatal("moving the first tab to the end should succeed")
	}

	var labels []string
	for _, tab := range ws.Tabs {
		labels = append(labels, tab.DisplayName())
	}
	wantLabels := []string{"foo", "3", "1"}
	if len(labels) != len(wantLabels) {
		t.Fatalf("labels = %v, want %v", labels, wantLabels)
	}
	for i := range wantLabels {
		if labels[i] != wantLabels[i] {
			t.Fatalf("labels = %v, want %v", labels, wantLabels)
		}
	}
	if ws.Tabs[0].CustomName != "foo" {
		t.Fatalf("tabs[0].CustomName = %q, want %q", ws.Tabs[0].CustomName, "foo")
	}
	if !ws.Tabs[1].IsAutoNamed() || !ws.Tabs[2].IsAutoNamed() {
		t.Fatal("tabs[1] and tabs[2] should be auto-named")
	}
	if ws.Tabs[0].Number != 2 || ws.Tabs[1].Number != 3 || ws.Tabs[2].Number != 1 {
		t.Fatalf("tab numbers = [%d %d %d], want [2 3 1]",
			ws.Tabs[0].Number, ws.Tabs[1].Number, ws.Tabs[2].Number)
	}
	if ws.Tabs[2].RootPane != movedRoot {
		t.Fatalf("tabs[2].RootPane = %d, want %d (moved tab)", ws.Tabs[2].RootPane, movedRoot)
	}
	if ws.ActiveTab().RootPane != activeRoot {
		t.Fatalf("active tab root = %d, want %d (identity should follow move)",
			ws.ActiveTab().RootPane, activeRoot)
	}
}
