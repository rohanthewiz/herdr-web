// Package worktree is the pure logic behind gateway's git-worktree feature
// (port of the Rust src/worktree.rs): branch/path slug derivation, the
// `git worktree` command builders, the `worktree list --porcelain` parser, and
// the dirty-remove error detection the two-step force confirmation keys on.
// Everything except the runners is deterministic and I/O-free; the runners are
// thin exec wrappers whose errors carry git's stderr (the text the dialogs
// surface). The orchestrator must call the runners off its loop goroutine.
package worktree

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// defaultPrefix namespaces generated branches and is the path-slug fallback.
const defaultPrefix = "worktree"

// The Rust word lists — kept identical so generated branch names look the same
// across the two implementations.
var (
	slugAdjectives = []string{"brave", "calm", "clear", "green", "lucky", "quiet", "rapid", "silver"}
	slugNouns      = []string{"river", "cloud", "field", "forest", "harbor", "meadow", "stone", "valley"}
)

// GeneratedBranchSlug derives a "worktree/{adj}-{noun}-{%04x}" branch name from
// a seed (callers pass e.g. time.Now().UnixMicro()). Deterministic per seed.
func GeneratedBranchSlug(seed int64) string {
	u := uint64(seed)
	adj := slugAdjectives[u%uint64(len(slugAdjectives))]
	noun := slugNouns[(u/uint64(len(slugAdjectives)))%uint64(len(slugNouns))]
	return fmt.Sprintf("%s/%s-%s-%04x", defaultPrefix, adj, noun, u&0xffff)
}

// BranchToPathSlug turns a branch name into a filesystem-safe folder name:
// lowercase, non-alphanumeric runs collapsed to a single "-", edges trimmed,
// "worktree" when nothing survives.
func BranchToPathSlug(branch string) string {
	var b strings.Builder
	lastDash := false
	for _, ch := range branch {
		switch {
		case ch >= 'a' && ch <= 'z', ch >= '0' && ch <= '9':
			b.WriteRune(ch)
			lastDash = false
		case ch >= 'A' && ch <= 'Z':
			b.WriteRune(ch + ('a' - 'A'))
			lastDash = false
		case !lastDash:
			b.WriteByte('-')
			lastDash = true
		}
	}
	slug := strings.Trim(b.String(), "-")
	if slug == "" {
		return defaultPrefix
	}
	return slug
}

// DefaultCheckoutPath is where a new checkout lands under the configured
// worktree root: <root>/<repoName>/<branch-slug>.
func DefaultCheckoutPath(root, repoName, branch string) string {
	return filepath.Join(root, repoName, BranchToPathSlug(branch))
}

// ExpandTilde resolves a leading "~" or "~/" against the user's home directory
// (the worktrees.directory default is "~/.herdr/worktrees"). A path with no
// tilde, or no resolvable home, is returned unchanged.
func ExpandTilde(path string) string {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if path == "~" {
		return home
	}
	return filepath.Join(home, path[2:])
}

// Entry is one checkout from `git worktree list --porcelain`.
type Entry struct {
	Path       string
	Branch     string // "" when detached/bare
	IsBare     bool
	IsDetached bool
	IsPrunable bool
}

// ParseWorktreeListPorcelain parses `git worktree list --porcelain` output.
// Entries are blank-line separated blocks; the first is always the main
// worktree. Branch names are stripped of their refs/heads/ prefix.
func ParseWorktreeListPorcelain(out []byte) []Entry {
	var entries []Entry
	var cur Entry
	have := false
	finish := func() {
		if have {
			entries = append(entries, cur)
		}
		cur = Entry{}
		have = false
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			finish()
			continue
		}
		switch {
		case strings.HasPrefix(line, "worktree "):
			finish() // a block missing its blank separator still terminates
			cur.Path = strings.TrimPrefix(line, "worktree ")
			have = true
		case strings.HasPrefix(line, "branch "):
			ref := strings.TrimPrefix(line, "branch ")
			cur.Branch = strings.TrimPrefix(ref, "refs/heads/")
		case line == "bare":
			cur.IsBare = true
		case line == "detached":
			cur.IsDetached = true
		case strings.HasPrefix(line, "prunable"):
			cur.IsPrunable = true
		}
	}
	finish()
	return entries
}

// IsDirtyRemoveError detects git's dirty-checkout refusal on `worktree remove`
// (both substrings, so a locked-worktree hint does not match). This is what
// escalates the front-end confirm to "delete anyway".
func IsDirtyRemoveError(msg string) bool {
	lower := strings.ToLower(msg)
	return strings.Contains(lower, "contains modified or untracked files") &&
		strings.Contains(lower, "use --force to delete it")
}

// Command is one git invocation (program + args), separable from the runner so
// the builders stay pure and unit-testable.
type Command struct {
	Program string
	Args    []string
}

// AddCommand builds `git -C <sourceCheckout> worktree add -b <branch> <path> <base>`
// — a new branch off base, checked out at path.
func AddCommand(sourceCheckout, path, branch, base string) Command {
	return Command{Program: "git", Args: []string{
		"-C", sourceCheckout, "worktree", "add", "-b", branch, path, base,
	}}
}

// RemoveCommand builds `git -C <repoRoot> worktree remove [--force] <path>`.
// It never deletes the branch — only the checkout folder.
func RemoveCommand(repoRoot, path string, force bool) Command {
	args := []string{"-C", repoRoot, "worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, path)
	return Command{Program: "git", Args: args}
}

// ListCommand builds `git -C <repoRoot> worktree list --porcelain`.
func ListCommand(repoRoot string) Command {
	return Command{Program: "git", Args: []string{"-C", repoRoot, "worktree", "list", "--porcelain"}}
}

// Run executes a command, discarding stdout. On failure the error message is
// git's trimmed stderr (falling back to stdout, then the exec error) — the text
// the dialogs show.
func Run(c Command) error {
	_, err := Output(c)
	return err
}

// Output executes a command and returns its stdout, with the same error shaping
// as Run.
func Output(c Command) ([]byte, error) {
	cmd := exec.Command(c.Program, c.Args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		if msg == "" {
			msg = fmt.Sprintf("%s failed: %v", c.Program, err)
		}
		return nil, errors.New(msg)
	}
	return stdout.Bytes(), nil
}

// RepoRoot resolves the toplevel of the checkout containing dir
// (`git -C <dir> rev-parse --show-toplevel`). For a linked worktree this is the
// worktree's own root, not the main repo's.
func RepoRoot(dir string) (string, error) {
	out, err := Output(Command{Program: "git", Args: []string{"-C", dir, "rev-parse", "--show-toplevel"}})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// List runs ListCommand against a checkout and parses the porcelain output.
func List(repoRoot string) ([]Entry, error) {
	out, err := Output(ListCommand(repoRoot))
	if err != nil {
		return nil, err
	}
	return ParseWorktreeListPorcelain(out), nil
}
