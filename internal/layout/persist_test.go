package layout

import (
	"encoding/json"
	"testing"
)

// SaveTree → JSON → Tree() must reproduce the exact tree: shape, directions,
// ratios, pane ids, and the geometry the tree computes.
func TestSaveTreeRoundTrip(t *testing.T) {
	l := sampleLayout()

	saved := SaveTree(l.Root())
	data, err := json.Marshal(saved)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back SavedNode
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	root, err := back.Tree()
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	restored := FromSaved(root, l.Focused())

	if got, want := restored.PaneIDs(), l.PaneIDs(); len(got) != len(want) {
		t.Fatalf("pane ids: got %v want %v", got, want)
	} else {
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("pane ids: got %v want %v", got, want)
			}
		}
	}
	if restored.Focused() != l.Focused() {
		t.Fatalf("focus: got %d want %d", restored.Focused(), l.Focused())
	}
	origRects, restRects := paneRects(l), paneRects(restored)
	for i := range origRects {
		if origRects[i] != restRects[i] {
			t.Fatalf("rect %d: got %+v want %+v", i, restRects[i], origRects[i])
		}
	}
}

// A single-pane tree (no splits) round-trips too.
func TestSaveTreeSinglePane(t *testing.T) {
	saved := SaveTree(&PaneNode{ID: pane(7)})
	root, err := saved.Tree()
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	p, ok := root.(*PaneNode)
	if !ok || p.ID != pane(7) {
		t.Fatalf("got %#v, want pane 7", root)
	}
}

// Corrupt saved nodes fail loudly rather than building a broken tree.
func TestSavedNodeValidation(t *testing.T) {
	id := pane(1)
	cases := map[string]*SavedNode{
		"nil":         nil,
		"empty":       {},
		"both":        {Pane: &id, Split: &SavedSplit{First: &SavedNode{Pane: &id}, Second: &SavedNode{Pane: &id}}},
		"missing kid": {Split: &SavedSplit{First: &SavedNode{Pane: &id}}},
		"bad dir":     {Split: &SavedSplit{Dir: 9, First: &SavedNode{Pane: &id}, Second: &SavedNode{Pane: &id}}},
	}
	for name, saved := range cases {
		if _, err := saved.Tree(); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

// An out-of-range persisted ratio is clamped back to a valid split, matching
// what the live tree enforces on every split/resize.
func TestSavedNodeRatioClamped(t *testing.T) {
	a, b := pane(1), pane(2)
	saved := &SavedNode{Split: &SavedSplit{Ratio: 3.5,
		First: &SavedNode{Pane: &a}, Second: &SavedNode{Pane: &b}}}
	root, err := saved.Tree()
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	if r := root.(*SplitNode).Ratio; r != 0.9 {
		t.Fatalf("ratio: got %v want 0.9", r)
	}
}

// ReservePaneIDs must push the allocator past every restored id, and never
// lower it.
func TestReservePaneIDs(t *testing.T) {
	base := AllocPaneID()
	ReservePaneIDs([]PaneID{base + 10, base + 3})
	if next := AllocPaneID(); next != base+11 {
		t.Fatalf("after reserve: got %d want %d", next, base+11)
	}
	ReservePaneIDs([]PaneID{base}) // lower than current — must not rewind
	if next := AllocPaneID(); next != base+12 {
		t.Fatalf("after low reserve: got %d want %d", next, base+12)
	}
}
