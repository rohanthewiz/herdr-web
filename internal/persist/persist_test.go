package persist

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/rohanthewiz/herdr-web/internal/app"
	"github.com/rohanthewiz/herdr-web/internal/layout"
	"github.com/rohanthewiz/herdr-web/internal/workspace"
)

type nopSpawner struct{}

func (nopSpawner) Spawn(workspace.SpawnSpec) (workspace.TerminalID, error) { return "t", nil }
func (nopSpawner) Despawn(workspace.TerminalID)                            {}

func sampleSnapshot(t *testing.T) app.Snapshot {
	t.Helper()
	s, err := app.NewSession(nopSpawner{}, "/tmp/x")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if _, err := s.SplitPane(nil, layout.Horizontal); err != nil {
		t.Fatalf("split: %v", err)
	}
	return s.Snapshot()
}

func TestSessionSaveLoad(t *testing.T) {
	dir := t.TempDir()
	path := SessionPath(dir)
	snap := sampleSnapshot(t)

	if err := SaveSession(path, snap, map[uint32]string{2: "/tmp/deep"}); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("perms: got %o want 600", fi.Mode().Perm())
	}

	got, cwds, err := LoadSession(path)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if _, err := app.RestoreSession(nopSpawner{}, got); err != nil {
		t.Fatalf("restore of loaded snapshot: %v", err)
	}
	if len(got.Workspaces) != len(snap.Workspaces) || got.Cwd != snap.Cwd {
		t.Fatalf("loaded snapshot differs: %+v vs %+v", got, snap)
	}
	if cwds[2] != "/tmp/deep" {
		t.Fatalf("pane cwds: %+v", cwds)
	}
}

func TestLoadSessionMissing(t *testing.T) {
	_, _, err := LoadSession(SessionPath(t.TempDir()))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("want fs.ErrNotExist, got %v", err)
	}
}

func TestLoadSessionCorrupt(t *testing.T) {
	dir := t.TempDir()
	path := SessionPath(dir)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadSession(path); err == nil || errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("want parse error, got %v", err)
	}
}

func TestLoadSessionWrongVersion(t *testing.T) {
	dir := t.TempDir()
	path := SessionPath(dir)
	if err := os.WriteFile(path, []byte(`{"version":99,"session":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadSession(path); err == nil {
		t.Fatal("want version error")
	}
}

func TestHistorySaveLoad(t *testing.T) {
	dir := t.TempDir()
	path := HistoryPath(dir)
	seeds := map[uint32]string{
		3: "line one\r\nline \x1b[31mtwo\x1b[0m\r\n",
		9: "plain\r\n",
	}
	if err := SaveHistory(path, seeds); err != nil {
		t.Fatalf("SaveHistory: %v", err)
	}
	got, err := LoadHistory(path)
	if err != nil {
		t.Fatalf("LoadHistory: %v", err)
	}
	if len(got) != 2 || got[3] != seeds[3] || got[9] != seeds[9] {
		t.Fatalf("seeds differ: %+v", got)
	}
}

// Save must create the state dir if it doesn't exist yet (first run).
func TestSaveCreatesDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "state")
	if err := SaveHistory(HistoryPath(dir), map[uint32]string{1: "x"}); err != nil {
		t.Fatalf("SaveHistory: %v", err)
	}
	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() {
		t.Fatalf("state dir not created: %v", err)
	}
}
