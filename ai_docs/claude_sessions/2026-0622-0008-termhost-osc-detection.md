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
- **Partial parity (now closed — see Stage C.2):** the heavy process-probe throttle was deferred
  here; it shipped in `5c6da0d`.

### 5. Agent detection — Stage C.2: process-probe throttle — commit `5c6da0d`
- **`detect.ForegroundPGID(fd) int`** (new, all 3 procscan platforms): the cheap probe — a single
  `tcgetpgrp` (`-1` == no group). Gates the expensive `ForegroundAgent` enumeration so an idle pane
  costs ~one syscall per tick instead of a full per-pid comm/path/argv sweep.
- **`internal/orchestration/detectthrottle.go`** (new, **pure / no ghostty tag**) — port of herdr's
  `src/pane.rs` throttle (`should_probe_foreground_job` + `AgentDetectionPresence`):
  - **`agentPresence`**: identity debounce. Adopts a non-empty probe immediately; an identified
    agent survives transient misses and only clears after **6 consecutive** empty probes
    (`AGENT_MISS_CONFIRMATION_ATTEMPTS`). A hit resets the miss counter (misses must be consecutive).
  - **`shouldProbeForegroundJob`**: full enumeration runs only on process-group change, the **5s**
    identified recheck, the **30s** missing-foreground-group recheck, or inside a post-group-change
    **acquisition window** (500ms for the first 1.5s, then 2s, up to an 8s cap) to catch a
    still-starting agent (argv settles a beat after the group appears — agents run under `node`).
  - **`foregroundGroupChanged`**: `noPGID (-1)` treated as a real absent value (appear/vanish count).
- **`host.go` `detectPump`**: cheap pgid each tick → throttle decision → enumerate only when due →
  fold through `agentPresence`. Acquisition window opens on an unidentified group change
  (`hadProcessProbe && groupChanged`), clears on identify or after the 8s cap.
- **Scope (documented in `detectthrottle.go`):** no-suppression / no-lifecycle-authority **subset**.
  Omitted `sync_content_change_acquisition` (the content-driven acquisition path); the realistic
  agent-launch case still opens an acquisition window because launching an agent makes a new process
  group while the shell was already being probed. Suppressed-agent (remote release), foreground-shell
  exit reporting, and full-lifecycle-authority inputs are not ported (those subsystems don't exist
  on the Go backend yet).
- Tests: `detectthrottle_test.go` (8 pure cases — presence adopt/switch, miss-tolerance,
  consecutive-only misses, clear, group-changed, identified/unidentified recheck, acquisition
  fast/slow/expired). Existing Host integration tests still pass through the throttled pump
  (`exec -a codex` identified on first probe; `pi` working after the 3s grace).

### 6. OSC 52 clipboard passthrough — commit `c2cb5f8`
- **`internal/orchestration/osc52.go`** (new, **pure / no ghostty tag**) — a **separate**
  `osc52Scanner` (the OSC 7 template from item 1), mirroring herdr's separate
  `Osc52Forwarder` vs `CwdOscTracker`. Kept distinct from `oscScanner` because OSC 52 payloads
  need a **256 KiB** cap (vs OSC 7's 4 KiB) and so the working OSC 7 path + its tests stay untouched.
  - Same ESC/`]`/BEL/ST state machine, split-across-reads tolerant. **ESC mid-payload is pushed
    back as literal bytes** (per Rust) so it just fails base64 rather than aborting the body.
  - `parseOSC52Clipboard`: accepts `52;c;<b64>` and `52;;<b64>` (default selection); rejects other
    selections (`p/q/s/0-7`), queries (`?` — no reply path here), and non-standard-base64. `52;c;`
    (empty payload) decodes to an empty slice = **clipboard-clear**. 256 KiB cap, bounded per byte.
- **`protocol.go`**: `MsgPaneClipboard` + `PaneClipboard{pane_id, data}` + `NewPaneClipboard`.
  `Data []byte` → base64 on the JSON wire (like `Input.Data`); empty Data == clear.
- **`host.go`**: `pane.osc52 osc52Scanner`; `readPump` feeds each read and emits **one
  `pane_clipboard` per write** (no dedup — every write forwarded, matching Rust's drain-all).
- Tests: `osc52_test.go` (12 pure cases ported from herdr's forwarder suite) + Host integration
  `TestHostReportsPaneClipboard` (child `printf`s OSC 52 → `pane_clipboard` data="hello").

### 7. OSC 9 progress wired into detection — commit `5c18aa5`
- **`internal/orchestration/osc9.go`** (new, **pure / no ghostty tag**) — `osc9Scanner` raw-scans
  OSC 9 from the PTY stream (libghostty surfaces OSC 0/2 title via `Title()` but **not** OSC 9),
  mirroring the OSC 7 scanner + the OSC 9 half of herdr's `AgentOscStateTracker`.
  - `parseOSC9Progress` returns the payload after `9;` (e.g. `4;3;`), sanitized via
    `sanitizeOSCString` (strip control chars, cap 256 runes — mirrors `AGENT_OSC_MAX_CHARS`).
  - `scan` returns the most-recent progress in the chunk (latest-retained), 4 KiB cap + recovery.
- **`host.go`**: `pane.osc9` (readPump-owned) + **`pane.progress atomic.Pointer[string]`** (shared
  latest; readPump writes, detectPump reads, `nil`=none). `detectPump` now passes
  `detect.Input.OscProgress` (was hardcoded `""`) and **clears progress on agent change** (mirrors
  Rust's `clear_retained`) so a new agent can't inherit the previous process's progress.
- Tests: `osc9_test.go` (9 pure cases — BEL/ST/empty, ignore-other, split, latest-wins, control
  strip, length cap, overlong-abandon+recover).
- **No distinctive e2e observable today:** claude is the only manifest with an `osc_progress` rule
  (`^4;0`→idle, no visible flag, prio 250) — same result as its idle fallback. So this is **parity +
  future-proofing**; the testable surface is the scanner (covered). No host integration test added
  (nothing the idle fallback doesn't already produce).

### 8. OSC 0/2 window-title passthrough — commit `6807bbb` (Rust consumer: herdr `7f32edf`)
- **`internal/orchestration/osctitle.go`** (new, **pure / no ghostty tag**) — `oscTitleScanner`
  raw-scans OSC 0/2 (libghostty surfaces title via `Title()` for detection, but the seam carried
  none, so a termhost pane's border couldn't show the program's title). Same scanner template as
  osc7/52/9; `parseOSCTitle` accepts OSC 0 (icon+title) / OSC 2 (title), `sanitizeOSCString`,
  empty payload = clear; latest-in-chunk wins.
- **`protocol.go`**: `MsgPaneTitle` + `PaneTitle{pane_id, title}` + `NewPaneTitle`.
- **`host.go`**: `pane.oscTitle` + `lastTitle`; `readPump` emits `pane_title` on change.
- Tests: `osctitle_test.go` (8 pure — OSC0/2, ignore-other incl. OSC 1 icon-only, split, latest-wins,
  control-strip, cap, overlong-recover) + Host integration `TestHostReportsPaneTitle`.
- **Rust consumer** (`7f32edf`): `Event::PaneTitle` → `PaneSignal::Title` → `AppEvent::TerminalTitleReported`
  → `TerminalState.terminal_title`. **`border_label` precedence chosen: hook title > OSC title >
  manual label > agent label** (OSC title shadows a manual label — confirmed via the option preview;
  one-line swap if that's unwanted). Chrome only, not session-persisted.

### 9. OSC 8 hyperlink passthrough — commit `96bec8b` (Rust carry: herdr `5a4aa23`)
- **Different shape from the OSC scanners:** OSC 8 is *inline per-cell* metadata (a link wraps grid
  cells), so it rides the **frame/grid path**, not a `pane_*` event. **libghostty exposes it only via
  `GridRef.HyperlinkURI`** (NOT the render-cell path used by the snapshot).
- **`internal/terminal`**: `Cell.Link` (URI) + `Snapshot.HasHyperlinks`. `ghostty.go` Snapshot
  gates per row via `RenderStateRowIterator.Raw().Hyperlink()` (cheap; may false-positive), then —
  **after** the render iteration completes — resolves URIs with `Terminal.GridRef(viewport
  point).HyperlinkURI()` per cell in flagged rows. GridRef (a borrowed view of terminal internals)
  never interleaves with the render-state iterators.
- **`protocol.go`**: `Frame.Hyperlinks []string` URI table. `FrameFromSnapshot` sends the frame
  **full whenever any cell has a link** and builds a per-frame dedup table, each linked cell indexing
  in. Force-full avoids cross-frame index drift (a skipped cell would keep a stale index); the cost
  is lost diff savings while a link is on screen (links are uncommon/transient).
- **Rust carry** (`5a4aa23`): herdr's `render_ansi` **already** emits OSC 8 from `FrameData`
  (`cell.hyperlink` → `hyperlinks[index]`) and cells already deserialize their index — the only gap
  was the URI table. `proto::Frame.hyperlinks` (`#[serde(default)]`) → `into_frame_data`;
  `client.rs PaneGrid.hyperlinks` folded through `apply`/`snapshot`.
- Tests: pure `FrameFromSnapshot` hyperlink + no-link + ghostty `TestHostReportsHyperlinkFrame`
  (real OSC 8 via `printf`); Rust proto `frame_with_hyperlinks_carries_table_and_indices`.
- **Limitation:** this feeds the **frame-data render path** (web/remote ANSI stream — browser sees
  clickable links). The native-TUI mouse resolver `visible_hyperlinks` still reads the *unfed* local
  emulator for termhost panes → TUI click-to-open won't see them yet (separate consumer; follow-up).

---

## Key facts for future me

- **go-libghostty gaps:** OSC 7 (pwd), OSC 52 (clipboard), and OSC 9 (progress) are NOT exposed;
  scan raw bytes (`oscScanner` / `osc52Scanner` / `osc9Scanner`). OSC 0/2 title IS exposed
  (`Title()`). OSC 9 progress is now scanned and fed to `detect.Input.OscProgress`.
- **detect pkg is plain cgo (libproc), not ghostty-tagged** → compiles in default builds.
  The orchestration Host that uses it is `//go:build ghostty`.
- **Agent label == manifest `id`.** Detection: process → identity (label); manifest →
  state (given the label). Both needed.
- **Detection publishing is debounced (Stage C), not raw.** `detectstate.go` is the pure,
  unit-testable parity port; `detectPump` drives it. Startup grace (3s) means a fresh agent
  reads Idle for ~3s before its first real scan — tests/fixtures must outlive that window.
- **Identity is throttled (Stage C.2), not per-tick.** `detectthrottle.go` gates the expensive
  `ForegroundAgent` enumeration behind a cheap `ForegroundPGID` (tcgetpgrp). An idle pane probes
  identity rarely (5s recheck) but the loop still ticks at 300ms for screen scans. `agentPresence`
  needs 6 consecutive misses to drop an agent — so a one-off probe miss never flaps identity.
- **Build/run env:**
  - Rust: `export ZIG="~/projs/go/herdr-web/.tools/zig-wrapped"`
  - Go (ghostty): `export PKG_CONFIG_PATH=~/projs/rust/herdr/vendor/libghostty-vt/zig-out/share/pkgconfig`
  - Daemon: `go build -tags ghostty -o /tmp/td ./cmd/termhost && /tmp/td --socket /tmp/x.sock`
- **Seam events now (Go→Rust):** `welcome`, `pane_frame`, `pane_cwd`, `pane_agent`, `pane_clipboard`, `pane_title`, `pane_exited`, `error`. (OSC 8 hyperlinks ride `pane_frame` — `Frame.Hyperlinks` table + per-cell index — not a new event.)

## Verification (all green)

- Default `go build ./...` (cgo on); `-tags ghostty` build.
- `go test ./internal/detect/` (pure, no toolchain): identify, manifest engine, all-compile.
- `go test ./internal/orchestration/` (pure, default build): `detectstate` debounce (8) +
  `detectthrottle` (8) + `osc` (OSC 7) + `osc52` (OSC 52, 12) + `osc9` (OSC 9, 9) +
  `osctitle` (OSC 0/2, 8) scanners + `FrameFromSnapshot` hyperlink table/no-link.
- `go test -tags ghostty ./internal/...`: Host cwd/agent/agent-working/title/clipboard/hyperlink-frame
  + terminal + orchestration.
- gofmt/vet clean in both default and `-tags ghostty` modes.

## Commits on `roh/phase-b` (this session)

```
96bec8b feat: OSC 8 hyperlink passthrough on the termhost seam (Go side)
6807bbb feat: OSC 0/2 window title passthrough on the termhost seam (Go side)
5c18aa5 feat: OSC 9 progress wired into detection (Go side)
c2cb5f8 feat: OSC 52 clipboard passthrough on the termhost seam (Go side)
5c6da0d feat: Stage C.2 — process-probe throttle (Go side)
2207526 feat: Stage C — driver-parity detection debounce (Go side)
5b7e723 feat: manifest-driven agent state detection in Go (Stage B)
156be75 feat: agent detection in Go — process identity (Stage A)
1a8220f feat: OSC 7 cwd passthrough on the termhost seam (Go side)
```
(pushed to `origin/roh/phase-b`)

## Next steps

- **`pane_clipboard` (OSC 52)** ✅ herdr `5ce148a`; **`pane_title` (OSC 0/2)** ✅ herdr `7f32edf`.
  (OSC 9 progress is consumed Go-side inside detection — no seam event.)
- **Remaining termhost degradations:** scrollback, selection, kitty graphics — each needs a seam
  carry + Rust consumer. (OSC 8 hyperlinks ✅ item 9, for the frame-data/web render path; the
  native-TUI click resolver is a separate follow-up.)
- **Daemon lifecycle:** have the Rust server spawn/supervise `cmd/termhost` instead of the
  manual `HERDR_TERMHOST_SOCKET` env + hand launch.
- Eventually: make termhost the default and **retire the Rust in-process detector/PTY path**.
