//go:build ghostty

package main

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/rohanthewiz/herdr-web/internal/browserproto"
	"github.com/rohanthewiz/herdr-web/internal/config"
)

// config.set merges the live-appliable sections onto the current config,
// persists YAML that reloads to the same state, and leaves unnamed keys at
// their defaults. config.get then reflects the saved state.
func TestConfigSetPersists(t *testing.T) {
	o, c := newPendingHarness()
	path := filepath.Join(t.TempDir(), "config.yaml")
	o.cfg = config.Default()
	o.cfgPath = path

	o.handleCmd(c, cmd(t, "c1", browserproto.CmdConfigSet, browserproto.ConfigSetParams{
		Theme:    &browserproto.ConfigTheme{Colors: map[string]string{"bg": "#000000"}, Font: "monospace"},
		CopyMode: map[string][]string{"yank": {"y", "c"}},
	}))
	if r, ok := recvDown(t, c).(*browserproto.CmdResult); !ok || !r.Ok {
		t.Fatalf("config.set should ack ok, got %#v", r)
	}

	got, _, err := config.Load(path)
	if err != nil {
		t.Fatalf("reload saved config: %v", err)
	}
	if got.Theme.Colors["bg"] != "#000000" || got.Theme.Font != "monospace" {
		t.Fatalf("theme not persisted: %+v", got.Theme)
	}
	if got.Theme.Colors["fg"] != config.Default().Theme.Colors["fg"] {
		t.Fatalf("unnamed color should keep its default, got %q", got.Theme.Colors["fg"])
	}
	if !reflect.DeepEqual(got.Keybindings.CopyMode["yank"], []string{"y", "c"}) {
		t.Fatalf("copy-mode rebind not persisted: %v", got.Keybindings.CopyMode["yank"])
	}

	// config.get reflects the applied state.
	o.handleCmd(c, cmd(t, "c2", browserproto.CmdConfigGet, nil))
	r, ok := recvDown(t, c).(*browserproto.CmdResult)
	if !ok || !r.Ok {
		t.Fatalf("config.get should ack ok, got %#v", r)
	}
}

// An invalid config.set (unknown copy-mode action) is rejected by the shared
// Validate path and writes nothing.
func TestConfigSetRejectsInvalid(t *testing.T) {
	o, c := newPendingHarness()
	path := filepath.Join(t.TempDir(), "config.yaml")
	o.cfg = config.Default()
	o.cfgPath = path

	o.handleCmd(c, cmd(t, "c1", browserproto.CmdConfigSet, browserproto.ConfigSetParams{
		CopyMode: map[string][]string{"teleport": {"t"}},
	}))
	if r, ok := recvDown(t, c).(*browserproto.CmdResult); !ok || r.Ok {
		t.Fatalf("invalid config.set should fail, got %#v", r)
	}
	if _, _, err := config.Load(path); err == nil {
		t.Fatal("a rejected config.set must not write the file")
	}
	if _, ok := o.cfg.Keybindings.CopyMode["teleport"]; ok {
		t.Fatal("a rejected config.set must not mutate the live config")
	}
}
