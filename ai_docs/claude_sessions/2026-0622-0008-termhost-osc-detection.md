# herdr-web (Go) — Phase B: OSC passthrough + agent detection in Go

**Date:** 2026-0622-0008
**Session ID:** `231ee2d1-0169-4dd7-b77b-7e2bbdf885c2`
**Repo:** `~/projs/go/herdr-web` (Go terminal backend) · paired with `~/projs/rust/herdr` (Rust orchestrator, remote `origin` = `rohanthewiz/herdr-go`)
**Branch:** `roh/phase-b` · pushed to `origin` (`rohanthewiz/herdr-web`)

> This is the **Go-side** record. The Rust-side companion lives in the herdr repo
> at `ai_docs/claude_sessions/2026-0622-0008-termhost-osc-detection.md`.
> Earlier context: `2026-0619-2154-go-rust-orchestration-seam.md` (the seam),
> `2026-0621-0855-e2e-herdr-termhost-test.md` (Rust pane-runtime wiring + e2e).

---

## Strategic context

Decision this session: **diverge from upstream herdr** (`ogulcancelik/herdr`). The
forks (`rohanthewiz/herdr-go` for Rust, `rohanthewiz/herdr-web` for Go) are now
canonical; no PRs upstream. The endgame is **Go as the single terminal backend**
(PTY + VT emulation + detection), eventually retiring the Rust in-process
PTY/ghostty/detect path. That reframing drives the work: build the Go→Rust signal
channel so the Rust emulator can eventually be dropped.

## What shipped (Go side), in order

### 1. OSC 7 cwd passthrough — commit `1a8220f`
- **Finding:** the pinned go-libghostty does **not** surface OSC 7 — `Terminal.Pwd()`
  stays empty for every OSC 7 form, while OSC 0/2 `Title()` works. (Verified by a
  throwaway probe.) So polling the emulator for cwd is a dead end.
- **Fix:** `internal/orchestration/osc.go` — a pure-Go `oscScanner` state machine that
  extracts OSC 7 from the **raw PTY byte stream** (mirrors how Rust's in-process path
  scans bytes), tolerant of sequences split across reads, length-capped. Handles
  `file://host/path`, `file:///path`, bare `/path`; BEL or ST terminated.
- `host.go` `readPump` runs the scanner per pane; emits a new `pane_cwd` event on change.
- `protocol.go`: `MsgPaneCwd` + `PaneCwd` + `NewPaneCwd`.
- Tests: pure scanner tests (split-across-reads, overlong, percent-decode) + a
  `-tags ghostty` `TestHostReportsPaneCwd` (child `printf` emits OSC 7).
- **This is the template for OSC 52 clipboard** (also not surfaced by go-libghostty).

### 2. Agent detection in Go — Stage A: process identity — commit `156be75`
- **`internal/detect`** (new pkg). Pure `IdentifyAgent(name)` ports herdr's
  `identify_agent` table → canonical agent label ("claude", "codex", "agy", …).
- **`procscan`**: foreground process-group inspection. macOS via **cgo**
  (`tcgetpgrp` + `proc_listpids(PROC_PGRP_ONLY)` + per-pid comm via `proc_name`,
  exec path via `proc_pidpath`, and **argv via `KERN_PROCARGS2`** — needed because
  agents like Claude run under `node`, so argv[0]/[1] carry the real name). Linux via
  `/proc`; other platforms stub to "". Plain cgo (system libproc) — **builds under
  default `go build`**, no ghostty toolchain.
- `host.go`: a per-pane `detectPump` (400ms ticker) calls `detect.ForegroundAgent(ptmx.Fd())`,
  emits `pane_agent` on change. Stage A state was coarse (idle if agent foreground).
- `protocol.go`: `MsgPaneAgent` + `PaneAgent{agent,state,visible_blocker,visible_working}`.
- Tests: pure identify + a real-PTY test using **`exec -a claude sleep`** to fake an
  agent name (validates tcgetpgrp + enumeration + argv id without a real agent).

### 3. Agent detection — Stage B: manifest-driven state — commit `5b7e723`
- **`internal/detect/manifest.go`** — pure port of herdr's manifest rule engine.
  - **Manifests as embedded JSON** (`internal/detect/manifests/*.json`), converted from
    herdr's TOML via `python3 -m tomllib`. **The manifest `id` field IS the agent label**
    (e.g. `github-copilot.json` has `id="copilot"`), so lookup is keyed by `id`. All 17 load.
    Chose JSON + stdlib `encoding/json` over adding a TOML dep (low-dep repo).
  - Rule compilation, the **8 region extractors actually used** (`whole_recent`,
    `osc_title`/`osc_progress`, `bottom_(non_empty_)lines(N)`, `after_last_prompt_marker`,
    `after_last_horizontal_rule`, `prompt_box_body`), gate matching
    (`contains`/`regex`/`line_regex` + `all`/`any`/`not`, priority, known-agent idle fallback).
  - **Go RE2 == Rust `regex` except two rewrites** in `translatePattern`: `\uXXXX → \x{XXXX}`
    and `\p{Alphabetic} → \p{L}`. (Found by a diag test compiling every manifest regex.)
- `terminal`: added `Title()` to the `Emulator` interface + ghostty impl (libghostty
  surfaces OSC 0/2 title fine).
- `host.go` `detectPump` now snapshots screen (rows joined by `\n`, trailing blanks
  trimmed) + title, runs `detect.Detect(label, Input)` → real idle/working/blocked +
  visible flags; honors `skip_state_update` (transcript viewer / model picker hold last state).
- Tests: manifest unit tests (claude working/idle/blocked, pi, fallback, all-compile) +
  Host integration `TestHostReportsAgentWorkingState` (`exec -a pi sh -c 'printf Working...'`).

### 4. Agent detection — Stage C: driver-parity debounce — commit `2207526`
- **`internal/orchestration/detectstate.go`** (new, **pure / no ghostty tag** → unit-testable
  without the emulator toolchain) — port of herdr's `src/pane/agent_detection.rs` flicker-smoothing
  state machine. The old `detectPump` emitted on every per-tick change; this smooths it:
  - **`pendingIdle`** debounce: a `Working→plain-Idle` drop (Idle with no visible-idle marker)
    is held until 3 confirmations OR a 700ms cap — bypassed by visible-idle, agent change, or
    process exit. (`shouldHoldWorkingToIdle`, mirrors `PendingIdleConfirmation`.)
  - **content-change skip** (`shouldSkipIdleScreenScan`): while Idle + agent present + no transition
    + unchanged PTY content-seq → skip the screen snapshot entirely.
  - **stable-signal refresh** (`stableVisibleSignalRefreshDue` + `shouldPublishDetectionUpdate`):
    a steady visible blocker is re-emitted every 800ms; the composed decision is
    `decideDetectionTransition`.
  - Tuning matched to Rust: base **300ms** (was 400ms), pending recheck **100ms**, cap **700ms**,
    3 confirmations, stable refresh **800ms**, startup grace **3s**.
- **`host.go` `detectPump` rewrite**: variable cadence (300ms base / 100ms while confirming a
  pending-idle); a **newly-acquired agent publishes Idle immediately and is pinned to Idle for a
  3s startup grace** before its first screen scan (so startup paint isn't misread as Working);
  internal tracking of `visibleIdle/Blocker/Working` + last-refresh time. Wire protocol unchanged
  (`pane_agent` still carries only `visible_blocker`/`visible_working`).
- **`pane.detectSeq atomic.Uint64`**: bumped on each non-empty PTY read in `readPump`
  (mirrors Rust's `observe_detection_content_change`), feeding the content-skip.
- Tests: `detectstate_test.go` (8 pure cases — hold/cap/bypass, skip-scan, publish, refresh,
  composed transition). `pi` working-state fixture bumped to `sleep 8` so it outlives the grace;
  `TestHostReportsAgentWorkingState` now takes ~3.6s (direct evidence the grace gates the scan).
- **Partial parity:** the heavy process-probe throttle (5s/30s rechecks, acquisition windows,
  agent-presence consecutive-miss handling) is NOT ported — we still probe the foreground agent
  every base tick. Only the pending-idle 100ms recheck cadence was ported. Follow-up if idle-pane
  syscall cost matters.

---

## Key facts for future me

- **go-libghostty gaps:** OSC 7 (pwd) and OSC 52 (clipboard) are NOT exposed; scan raw
  bytes (`oscScanner`). OSC 0/2 title IS exposed (`Title()`). OSC 9 progress: not checked
  yet — pass "" for now (some Claude rules want it).
- **detect pkg is plain cgo (libproc), not ghostty-tagged** → compiles in default builds.
  The orchestration Host that uses it is `//go:build ghostty`.
- **Agent label == manifest `id`.** Detection: process → identity (label); manifest →
  state (given the label). Both needed.
- **Detection publishing is debounced (Stage C), not raw.** `detectstate.go` is the pure,
  unit-testable parity port; `detectPump` drives it. Startup grace (3s) means a fresh agent
  reads Idle for ~3s before its first real scan — tests/fixtures must outlive that window.
- **Build/run env:**
  - Rust: `export ZIG="~/projs/go/herdr-web/.tools/zig-wrapped"`
  - Go (ghostty): `export PKG_CONFIG_PATH=~/projs/rust/herdr/vendor/libghostty-vt/zig-out/share/pkgconfig`
  - Daemon: `go build -tags ghostty -o /tmp/td ./cmd/termhost && /tmp/td --socket /tmp/x.sock`
- **Seam events now (Go→Rust):** `welcome`, `pane_frame`, `pane_cwd`, `pane_agent`, `pane_exited`, `error`.

## Verification (all green)

- Default `go build ./...` (cgo on); `-tags ghostty` build.
- `go test ./internal/detect/` (pure, no toolchain): identify, manifest engine, all-compile.
- `go test ./internal/orchestration/` (pure, default build): `detectstate` debounce machine (8 cases).
- `go test -tags ghostty ./internal/...`: Host cwd/agent/agent-working + terminal + orchestration.
- gofmt/vet clean in both default and `-tags ghostty` modes.

## Commits on `roh/phase-b` (this session)

```
2207526 feat: Stage C — driver-parity detection debounce (Go side)
5b7e723 feat: manifest-driven agent state detection in Go (Stage B)
156be75 feat: agent detection in Go — process identity (Stage A)
1a8220f feat: OSC 7 cwd passthrough on the termhost seam (Go side)
```
(pushed to `origin/roh/phase-b`)

## Next steps

- **Stage C — process-probe throttle (the un-ported remainder):** port herdr's foreground
  process-group recheck cadences (5s identified / 30s missing-group), acquisition windows, and
  agent-presence consecutive-miss handling. The debounce/grace/content-skip half shipped in
  `2207526`; this half cuts idle-pane syscall churn (we currently probe every 300ms tick).
- **OSC 52 clipboard:** extend `oscScanner` to OSC 52 (+ base64) → `pane_clipboard` event.
- **OSC 9 progress:** raw-scan for richer Claude idle/working rules.
- **Daemon lifecycle:** have the Rust server spawn/supervise `cmd/termhost` instead of the
  manual `HERDR_TERMHOST_SOCKET` env + hand launch.
- Eventually: make termhost the default and **retire the Rust in-process detector/PTY path**.
