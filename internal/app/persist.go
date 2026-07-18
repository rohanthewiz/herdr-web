// Session persistence support (WS3): the whole-session snapshot and its
// restore constructor. Snapshot/RestoreSession are pure model conversions —
// where the file lives, when it is written, and how PTYs are re-adopted or
// re-seeded is the orchestrator runtime's business (cmd/gateway2), not the
// domain layer's.
package app

import (
	"errors"
	"fmt"

	"github.com/rohanthewiz/herdr-web/internal/layout"
	"github.com/rohanthewiz/herdr-web/internal/workspace"
)

// Snapshot is the session's durable state: everything a gateway2 restart needs
// to rebuild the workspace/tab/pane tree exactly — split ratios, focus, zoom,
// custom names, public numbering — plus the active-workspace viewport.
type Snapshot struct {
	Cwd        string               `json:"cwd,omitempty"`
	Active     int                  `json:"active"`
	Workspaces []workspace.Snapshot `json:"workspaces"`
}

// Snapshot captures the session's durable state.
func (s *Session) Snapshot() Snapshot {
	wss := make([]workspace.Snapshot, 0, len(s.workspaces))
	for _, ws := range s.workspaces {
		wss = append(wss, ws.Snapshot())
	}
	return Snapshot{Cwd: s.cwd, Active: s.active, Workspaces: wss}
}

// RestoreSession rebuilds a session from its snapshot, re-spawning every
// pane's terminal through the seam, and reserves the global workspace/pane id
// counters past every restored id so newly created ones never collide. Any
// internal inconsistency (out-of-range indices, duplicate pane ids, corrupt
// trees) is an error — the caller starts fresh rather than driving PTYs from a
// broken model.
func RestoreSession(spawner workspace.PaneSpawner, snap Snapshot) (*Session, error) {
	if len(snap.Workspaces) == 0 {
		return nil, errors.New("restore: no workspaces")
	}
	if snap.Active < 0 || snap.Active >= len(snap.Workspaces) {
		return nil, fmt.Errorf("restore: active workspace %d out of range", snap.Active)
	}

	seen := make(map[layout.PaneID]bool)
	var allPanes []layout.PaneID
	var wsIDs []string
	workspaces := make([]*workspace.Workspace, 0, len(snap.Workspaces))
	for _, wsnap := range snap.Workspaces {
		ws, err := workspace.Restore(spawner, wsnap)
		if err != nil {
			return nil, fmt.Errorf("restore: %w", err)
		}
		for _, tab := range ws.Tabs {
			for _, id := range tab.Layout.PaneIDs() {
				if seen[id] {
					return nil, fmt.Errorf("restore: pane %d appears twice", id)
				}
				seen[id] = true
				allPanes = append(allPanes, id)
			}
		}
		wsIDs = append(wsIDs, ws.ID)
		workspaces = append(workspaces, ws)
	}

	workspace.ReserveWorkspaceIDs(wsIDs)
	layout.ReservePaneIDs(allPanes)
	return &Session{spawner: spawner, cwd: snap.Cwd, workspaces: workspaces, active: snap.Active}, nil
}
