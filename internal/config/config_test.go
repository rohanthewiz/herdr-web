package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// The default config is self-consistent: it validates, its TTL parses, and it
// carries the full theme + copy-mode tables.
func TestDefaultValid(t *testing.T) {
	d := Default()
	if err := d.Validate(); err != nil {
		t.Fatalf("Default should validate: %v", err)
	}
	if ttl, err := d.Server.TTL(); err != nil || ttl != 24*time.Hour {
		t.Fatalf("default TTL = %v, %v; want 24h", ttl, err)
	}
	if len(d.Theme.Colors) != len(defaultColors) {
		t.Fatalf("default theme has %d colors, want %d", len(d.Theme.Colors), len(defaultColors))
	}
	if len(d.Keybindings.CopyMode) != len(defaultCopyMode) {
		t.Fatalf("default copy_mode has %d actions, want %d", len(d.Keybindings.CopyMode), len(defaultCopyMode))
	}
	// Default returns independent maps — mutating one result must not affect
	// another (guards the shared package-global globals).
	d.Theme.Colors["bg"] = "#000000"
	if Default().Theme.Colors["bg"] == "#000000" {
		t.Fatal("Default must return fresh maps, not shared globals")
	}
}

// An empty file yields exactly the defaults.
func TestParseEmpty(t *testing.T) {
	got, err := parse([]byte(""))
	if err != nil {
		t.Fatalf("parse empty: %v", err)
	}
	if !reflect.DeepEqual(got, Default()) {
		t.Fatalf("empty config should equal Default()")
	}
}

// A partial config overrides only what it names: one scalar, one color, one
// keybinding — everything else keeps its default. tls.enabled:false (a zero
// value) must survive, i.e. absent keys keep defaults but present keys win even
// when zero.
func TestParsePartialMerge(t *testing.T) {
	yaml := `
server:
  addr: "127.0.0.1:9000"
  auth: none
theme:
  colors:
    bg: "#000000"
keybindings:
  copy_mode:
    yank: ["y", "c"]
`
	got, err := parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.Server.Addr != "127.0.0.1:9000" || got.Server.Auth != "none" {
		t.Fatalf("server overrides not applied: %+v", got.Server)
	}
	// Untouched scalars keep defaults.
	if got.Server.TermhostSocket != Default().Server.TermhostSocket {
		t.Fatalf("termhost socket should keep default, got %q", got.Server.TermhostSocket)
	}
	// Overridden color wins; sibling colors keep defaults.
	if got.Theme.Colors["bg"] != "#000000" {
		t.Fatalf("bg override not applied: %q", got.Theme.Colors["bg"])
	}
	if got.Theme.Colors["fg"] != defaultColors["fg"] {
		t.Fatalf("fg should keep default, got %q", got.Theme.Colors["fg"])
	}
	if len(got.Theme.Colors) != len(defaultColors) {
		t.Fatalf("partial theme should keep all default colors, got %d", len(got.Theme.Colors))
	}
	// Rebound action wins; sibling actions keep defaults.
	if !reflect.DeepEqual(got.Keybindings.CopyMode["yank"], []string{"y", "c"}) {
		t.Fatalf("yank rebind not applied: %v", got.Keybindings.CopyMode["yank"])
	}
	if !reflect.DeepEqual(got.Keybindings.CopyMode["exit"], defaultCopyMode["exit"]) {
		t.Fatalf("exit should keep default, got %v", got.Keybindings.CopyMode["exit"])
	}
}

// Validation rejects bad enum/duration/keybinding values.
func TestValidateRejects(t *testing.T) {
	cases := map[string]string{
		"bad auth":       "server:\n  auth: maybe\n",
		"bad ttl":        "server:\n  session_ttl: \"neverish\"\n",
		"unknown action": "keybindings:\n  copy_mode:\n    teleport: [\"t\"]\n",
		"empty key list": "keybindings:\n  copy_mode:\n    yank: []\n",
		"lone tls cert":  "server:\n  tls:\n    cert: /x.pem\n",
	}
	for name, yaml := range cases {
		if _, err := parse([]byte(yaml)); err == nil {
			t.Errorf("%s: expected parse error", name)
		}
	}
}

// Load reads an explicit path and merges it; a missing explicit path is an
// error; a syntactically empty file is defaults.
func TestLoad(t *testing.T) {
	dir := t.TempDir()

	// Present, valid file → merged.
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("server:\n  addr: \":7000\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, gotPath, err := Load(path)
	if err != nil {
		t.Fatalf("Load present: %v", err)
	}
	if got.Server.Addr != ":7000" || gotPath != path {
		t.Fatalf("Load present: addr=%q path=%q", got.Server.Addr, gotPath)
	}

	// Missing explicit path → error (the operator named a file that isn't there).
	if _, _, err := Load(filepath.Join(dir, "nope.yaml")); err == nil {
		t.Fatal("Load of a missing explicit path should error")
	}

	// HERDR_CONFIG env resolves the path when no override is given.
	t.Setenv(EnvVar, path)
	got, _, err = Load("")
	if err != nil || got.Server.Addr != ":7000" {
		t.Fatalf("Load via env: addr=%q err=%v", got.Server.Addr, err)
	}
}

// The persistence block: absent keys keep the on-by-default behaviour; present
// keys override; a negative history_lines is rejected.
func TestParsePersistence(t *testing.T) {
	got, err := parse([]byte("persistence:\n  state_dir: /tmp/state\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !got.Persistence.Enabled || got.Persistence.StateDir != "/tmp/state" || got.Persistence.HistoryLines != 2000 {
		t.Fatalf("got %+v", got.Persistence)
	}

	got, err = parse([]byte("persistence:\n  enabled: false\n  history_lines: 500\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.Persistence.Enabled || got.Persistence.HistoryLines != 500 {
		t.Fatalf("got %+v", got.Persistence)
	}

	if _, err := parse([]byte("persistence:\n  history_lines: -1\n")); err == nil {
		t.Fatal("negative history_lines should be rejected")
	}
}
