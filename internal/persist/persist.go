// Package persist is gateway's on-disk session state (WS3): the model
// snapshot that survives a gateway restart (session.json) and the captured
// scrollback seeds that survive a termhost daemon loss (history.json). Two
// files because they have different rhythms — the model is small and written
// on every mutation (debounced); history is large and written occasionally
// (periodic capture + clean shutdown).
//
// Both are versioned JSON, written atomically (temp file + rename) with
// owner-only permissions — history contains raw terminal scrollback, which is
// as sensitive as anything the user typed.
package persist

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/rohanthewiz/herdr-web/internal/app"
)

// Version is the state-file schema version. A mismatch refuses the file (the
// caller starts fresh) rather than guessing at a shape we no longer write.
const Version = 1

// sessionFile is the session.json envelope. PaneCwds rides alongside the model
// snapshot: each pane's last daemon-reported working directory (OSC 7), so a
// cold restore re-spawns shells where they were — runtime chrome the domain
// model deliberately doesn't own. PaneAgents is the same idea for resumable
// agent sessions (herdr's PaneAgentSessionSnapshot): the hook-reported session
// identity per pane, so a cold restore can relaunch the agent's native
// conversation (`claude --resume <id>`). Additive fields — a version bump is
// not needed, an old file simply has neither map.
type sessionFile struct {
	Version    int                     `json:"version"`
	Session    app.Snapshot            `json:"session"`
	PaneCwds   map[uint32]string       `json:"pane_cwds,omitempty"`
	PaneAgents map[uint32]AgentSession `json:"pane_agent_sessions,omitempty"`
}

// AgentSession is one pane's persisted resumable agent-session identity
// (herdr's PaneAgentSessionSnapshot): the reporting source ("herdr:claude"),
// the agent label it must match, and the session ref — Kind "id" for every
// agent except pi, which may use an absolute "path". No timestamps, no TTL:
// staleness is the agent's own problem at resume time, exactly as in herdr.
type AgentSession struct {
	Source string `json:"source"`
	Agent  string `json:"agent"`
	Kind   string `json:"kind"` // "id" | "path"
	Value  string `json:"value"`
}

// historyFile is the history.json envelope: pane id → VT-encoded scrollback,
// captured via the daemon's request_text(ansi) and replayed through
// create_pane.initial_history on a cold start.
type historyFile struct {
	Version int               `json:"version"`
	Panes   map[uint32]string `json:"panes"`
}

// DefaultDir is the state directory when the config names none:
// $XDG_STATE_HOME/herdr, falling back to ~/.local/state/herdr (state, not
// config — this is machine-local runtime data, following the same XDG-with-
// fallback convention as the config file). "" if no home dir is resolvable.
func DefaultDir() string {
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(dir, "herdr")
}

// SessionPath / HistoryPath are the file locations within a state dir.
func SessionPath(dir string) string { return filepath.Join(dir, "session.json") }
func HistoryPath(dir string) string { return filepath.Join(dir, "history.json") }

// SaveSession writes the model snapshot (plus per-pane cwds and agent
// sessions) atomically.
func SaveSession(path string, snap app.Snapshot, paneCwds map[uint32]string, paneAgents map[uint32]AgentSession) error {
	return writeJSON(path, sessionFile{Version: Version, Session: snap, PaneCwds: paneCwds, PaneAgents: paneAgents})
}

// LoadSession reads a model snapshot. A missing file returns fs.ErrNotExist
// (start fresh, silently); anything else — unreadable, unparseable, wrong
// version — is an error the caller should log before starting fresh.
func LoadSession(path string) (app.Snapshot, map[uint32]string, map[uint32]AgentSession, error) {
	var f sessionFile
	if err := readJSON(path, &f); err != nil {
		return app.Snapshot{}, nil, nil, err
	}
	if f.Version != Version {
		return app.Snapshot{}, nil, nil, fmt.Errorf("%s: version %d, want %d", path, f.Version, Version)
	}
	if f.PaneCwds == nil {
		f.PaneCwds = map[uint32]string{}
	}
	if f.PaneAgents == nil {
		f.PaneAgents = map[uint32]AgentSession{}
	}
	return f.Session, f.PaneCwds, f.PaneAgents, nil
}

// SaveHistory writes the scrollback seeds atomically.
func SaveHistory(path string, panes map[uint32]string) error {
	return writeJSON(path, historyFile{Version: Version, Panes: panes})
}

// LoadHistory reads the scrollback seeds; error semantics match LoadSession.
func LoadHistory(path string) (map[uint32]string, error) {
	var f historyFile
	if err := readJSON(path, &f); err != nil {
		return nil, err
	}
	if f.Version != Version {
		return nil, fmt.Errorf("%s: version %d, want %d", path, f.Version, Version)
	}
	if f.Panes == nil {
		f.Panes = map[uint32]string{}
	}
	return f.Panes, nil
}

// writeJSON marshals v and writes it atomically with owner-only permissions:
// temp file in the target dir (same filesystem, so rename is atomic), fsync'd
// before the swap so a crash leaves either the old file or the new one, never
// a torn write.
func writeJSON(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}
