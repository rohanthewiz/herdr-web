//go:build ghostty

package main

import (
	"maps"
	"slices"
	"strings"

	"github.com/rohanthewiz/herdr-web/internal/persist"
)

// Agent-session resume on cold restore — the port of herdr's src/agent_resume.rs
// plan table plus the restore-pass dedupe from src/persist/restore.rs. A pane
// whose hook-reported session ref survived in session.json is re-spawned with
// the agent's native resume command instead of a shell, so the conversation
// picks up where it left off.
//
// One deliberate adaptation: herdr spawns a shell, waits up to 750 ms for the
// host terminal theme, then types the shell-quoted resume command into it. The
// gateway instead execs the plan argv directly via create_pane.command/args —
// the seam termhost already honors — so the id stays argv data (never shell
// text) and no timing gate is needed. The visible difference: when the resumed
// agent exits, the pane exits with it rather than dropping to a shell.

// resumeArgv is herdr's agent_resume::plan: the exact per-agent resume command
// line for an official (source, agent) pair, nil for anything unresumable. The
// ref is re-validated with the same rules as ingest (session_ref_from_snapshot):
// id ≤512 chars, path pi-only and absolute ≤4096 chars, control chars rejected —
// a corrupted state file yields no resume, never a malformed exec.
func resumeArgv(source, agent, kind, value string) []string {
	if officialAgentSources[source] != agent {
		return nil
	}
	switch kind {
	case "id":
		if value == "" || len(value) > 512 || hasControl(value) {
			return nil
		}
	case "path":
		// Only pi records a path-form ref (its session file); everyone else
		// resumes by id.
		if agent != "pi" || value == "" || len(value) > 4096 ||
			!strings.HasPrefix(value, "/") || hasControl(value) {
			return nil
		}
	default:
		return nil
	}
	switch agent {
	case "claude":
		return []string{"claude", "--resume", value}
	case "codex":
		return []string{"codex", "resume", value}
	case "copilot":
		return []string{"copilot", "--resume=" + value}
	case "droid":
		return []string{"droid", "--resume", value}
	case "kimi":
		return []string{"kimi", "--session", value}
	case "pi":
		return []string{"pi", "--session", value}
	case "hermes":
		return []string{"hermes", "--resume", value}
	case "opencode":
		return []string{"opencode", "--session", value}
	case "qodercli":
		return []string{"qodercli", "--resume", value}
	case "kilo":
		return []string{"kilo", "--session", value}
	case "cursor":
		return []string{"cursor-agent", "--resume", value} // binary ≠ agent label
	}
	return nil
}

// resumeDedupeKey identifies one agent conversation across panes (herdr's
// AgentResumePlan.dedupe_key): NUL-joined so no field content can collide.
func resumeDedupeKey(s persist.AgentSession) string {
	return s.Source + "\x00" + s.Agent + "\x00" + s.Kind + "\x00" + s.Value
}

// planResume validates the loaded per-pane agent sessions and, when resume is
// enabled, builds each pane's resume argv (herdr's pane_restore_startup):
//
//   - kept: the refs to rehydrate — invalid entries are dropped, and with
//     resume on, so is every duplicate of an already-planned conversation
//     (herdr: a duplicate pane gets no plan and no rehydrated ref).
//   - plans: pane → resume argv, first pane wins a shared conversation. Herdr
//     dedupes in restore traversal order; the gateway uses ascending pane id —
//     deterministic, though not necessarily the same winner.
//   - suppressHist: panes whose saved scrollback must not be replayed — every
//     pane with a planned or duplicate-suppressed resume (the resumed agent
//     owns the scrollback; a duplicate's stale transcript would masquerade as
//     a live one).
//
// With resume disabled, every valid ref is kept (metadata preserved, exactly
// as herdr rehydrates refs it will not act on) and no plans are built.
func planResume(saved map[uint32]persist.AgentSession, resume bool) (
	kept map[uint32]persist.AgentSession, plans map[uint32][]string, suppressHist map[uint32]bool) {
	kept = make(map[uint32]persist.AgentSession)
	plans = make(map[uint32][]string)
	suppressHist = make(map[uint32]bool)
	seen := make(map[string]bool)
	for _, pid := range slices.Sorted(maps.Keys(saved)) {
		s := saved[pid]
		argv := resumeArgv(s.Source, s.Agent, s.Kind, s.Value)
		if argv == nil {
			continue // invalid or unresumable — not rehydrated
		}
		if !resume {
			kept[pid] = s
			continue
		}
		suppressHist[pid] = true
		if seen[resumeDedupeKey(s)] {
			continue // duplicate conversation — first pane already resumes it
		}
		seen[resumeDedupeKey(s)] = true
		kept[pid] = s
		plans[pid] = argv
	}
	return kept, plans, suppressHist
}
