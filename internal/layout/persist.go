// Session persistence support (WS3): a JSON-friendly mirror of the BSP tree
// plus the pane-id reservation that keeps restored ids from colliding with
// newly allocated ones (cf. ReserveWorkspaceIDs in the workspace package).
package layout

import (
	"errors"
	"fmt"
)

// SavedNode is the serializable mirror of a layout tree Node: exactly one of
// Pane or Split is set. The interface tree itself can't round-trip through
// JSON, so snapshots carry this shape and rebuild the tree via Tree().
type SavedNode struct {
	Pane  *PaneID     `json:"pane,omitempty"`
	Split *SavedSplit `json:"split,omitempty"`
}

// SavedSplit mirrors a SplitNode: direction, first-child ratio, two children.
type SavedSplit struct {
	Dir    uint8      `json:"dir"`
	Ratio  float32    `json:"ratio"`
	First  *SavedNode `json:"first"`
	Second *SavedNode `json:"second"`
}

// SaveTree converts a live tree into its serializable mirror (nil for nil).
func SaveTree(n Node) *SavedNode {
	switch n := n.(type) {
	case *PaneNode:
		id := n.ID
		return &SavedNode{Pane: &id}
	case *SplitNode:
		return &SavedNode{Split: &SavedSplit{
			Dir:    uint8(n.Direction),
			Ratio:  n.Ratio,
			First:  SaveTree(n.First),
			Second: SaveTree(n.Second),
		}}
	}
	return nil
}

// Tree rebuilds a live tree from the saved mirror, validating shape as it goes
// (exactly one variant per node, both split children present, sane ratios) so a
// corrupt snapshot fails loudly instead of building a broken layout.
func (s *SavedNode) Tree() (Node, error) {
	switch {
	case s == nil:
		return nil, errors.New("layout: nil saved node")
	case s.Pane != nil && s.Split != nil:
		return nil, errors.New("layout: saved node has both pane and split")
	case s.Pane != nil:
		return &PaneNode{ID: *s.Pane}, nil
	case s.Split != nil:
		if s.Split.Dir > uint8(Vertical) {
			return nil, fmt.Errorf("layout: bad split direction %d", s.Split.Dir)
		}
		first, err := s.Split.First.Tree()
		if err != nil {
			return nil, err
		}
		second, err := s.Split.Second.Tree()
		if err != nil {
			return nil, err
		}
		return &SplitNode{
			Direction: Direction(s.Split.Dir),
			Ratio:     validSplitRatio(s.Split.Ratio),
			First:     first,
			Second:    second,
		}, nil
	}
	return nil, errors.New("layout: empty saved node")
}

// ReservePaneIDs raises the global pane-id counter past every id in the list,
// so panes in a restored session never collide with newly allocated ids.
func ReservePaneIDs(ids []PaneID) {
	var maxSeen uint32
	for _, id := range ids {
		maxSeen = max(maxSeen, uint32(id))
	}
	for {
		current := nextPaneID.Load()
		if current >= maxSeen {
			return
		}
		if nextPaneID.CompareAndSwap(current, maxSeen) {
			return
		}
	}
}
