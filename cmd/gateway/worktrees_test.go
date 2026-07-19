//go:build ghostty

package main

import (
	"path/filepath"
	"testing"
)

// A workspace created at a checkout path is found by workspaceForPath (identity
// cwd match, symlink-canonicalized), and its panes spawn in the checkout via
// paneCwd — the cwd-threading seam worktree.create/open relies on.
func TestWorktreeWorkspaceCwd(t *testing.T) {
	o, err := newOrch("", "/base")
	if err != nil {
		t.Fatalf("newOrch: %v", err)
	}
	dir := t.TempDir()
	id, err := o.session.CreateWorkspaceAt(dir)
	if err != nil {
		t.Fatalf("CreateWorkspaceAt: %v", err)
	}

	if got := o.workspaceForPath(dir); got != id {
		t.Fatalf("workspaceForPath(%s) = %q, want %q", dir, got, id)
	}
	if got := o.workspaceForPath(filepath.Join(dir, "elsewhere")); got != "" {
		t.Fatalf("workspaceForPath on a non-checkout path = %q, want \"\"", got)
	}

	// The new workspace is active; its pane must inherit the checkout cwd.
	pid, ok := o.session.FocusedPane()
	if !ok {
		t.Fatal("no focused pane after CreateWorkspaceAt")
	}
	if got := o.paneCwd(uint32(pid)); canonPath(got) != canonPath(dir) {
		t.Fatalf("paneCwd = %q, want %q", got, dir)
	}
}
