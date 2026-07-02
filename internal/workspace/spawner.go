package workspace

import "github.com/rohanthewiz/herdr-web/internal/layout"

// TerminalID identifies a terminal (PTY + VT state) owned by the spawner,
// e.g. "term_18f3a2c41" (cf. terminal/id.rs). Workspace/Tab only carry it;
// they never touch the terminal itself.
type TerminalID string

// SpawnSpec describes the terminal to create for a new pane. It replaces the
// parameter fan of Rust's TerminalRuntime::spawn/spawn_shell_command/
// spawn_argv_command — the backend decides what the extra knobs (scrollback,
// theme, shell config) look like.
type SpawnSpec struct {
	// PaneID is filled in by Tab/Workspace before Spawn is called (the
	// layout allocates it); any caller-provided value is overwritten.
	PaneID     layout.PaneID
	Rows, Cols uint16
	// Cwd is the working directory; "" lets the tab fall back to the
	// process cwd (cf. split_focused_with_runtime's env::current_dir).
	Cwd string
	// Argv launches an explicit command vector instead of a shell.
	Argv []string
	// Command runs a one-off shell command ("" = none). Mutually exclusive
	// with Argv.
	Command string
	// ExtraEnv is appended to the child environment for Command launches.
	ExtraEnv [][2]string
	// PublicPaneID is the stable public handle, e.g. "w1:p3". Filled in by
	// Workspace; any caller-provided value is overwritten.
	PublicPaneID string
}

// PaneSpawner is the seam between the pure workspace/tab bookkeeping and the
// terminal backend (Rust: TerminalRuntime::spawn*). Spawn creates the
// terminal for spec.PaneID and returns its TerminalID; Despawn terminates a
// terminal after its pane was detached (callers get the TerminalID back from
// CloseFocused/ClosePane and decide whether to despawn or re-attach).
type PaneSpawner interface {
	Spawn(spec SpawnSpec) (TerminalID, error)
	Despawn(id TerminalID)
}

// NewPane reports a successful split: the layout pane and the terminal the
// spawner attached to it (Rust NewPane carries the TerminalState/runtime;
// here the spawner owns those).
type NewPane struct {
	PaneID     layout.PaneID
	TerminalID TerminalID
}

// DetachedPane identifies a pane removed from a tab together with the
// terminal that was attached to it (cf. tab.rs DetachedPane).
type DetachedPane struct {
	PaneID     layout.PaneID
	TerminalID TerminalID
}
