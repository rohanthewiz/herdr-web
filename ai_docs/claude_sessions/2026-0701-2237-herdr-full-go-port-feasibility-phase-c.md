# herdr-web (Go) — full Go-port feasibility + Phase C (web-only) work-breakdown

**Session id:** `b10b8d6d-dc4a-48cd-b23b-7781452b2db8`
**Date:** 2026-0701-2237 · **Repo:** `~/projs/go/herdr-web` (Go) · paired with `~/projs/rust/herdr` (Rust orchestrator).
**Branch (Go):** `roh/phase-b`

> **Analysis / planning session — no product code changed.** The user's goal: move *all* of
> herdr off Rust onto Go. Assessed feasibility of the full port, decided **web-front-end only**,
> and scoped the first two workstreams (WS0, WS1) into file:symbol-anchored task lists.
> Deliverables are three docs (below) + a project memory. Method: two rounds of parallel
> Explore subagents (Rust core inventory + session-history synthesis; then WS0 gating map +
> WS1 layout/workspace inventory).

---

## Artifacts produced this session

- **`ai_docs/fbl_go_port_feasibility_analysis.md`** — Part 1 feasibility analysis (phase status,
  Rust module LOC inventory, dependency→Go mapping, the "pure-Go" asterisk, the two hard problems);
  Part 2 the Phase C **web-front-end-only** work-breakdown WS0–WS11 (target architecture, the
  ~12k-LOC "delete not port" table, sizes, dependency graph, effort, open decisions).
- **`ai_docs/phase-c-ws0-ws1-tasks.md`** — actionable, staged, file:symbol-anchored task lists for
  WS0 (Rust) and WS1 (Go).
- **This session note.**
- Project memory `goal-full-go-migration.md` (in the Claude memory dir, outside the repo) —
  records the goal, the web-only decision, and pointers to both docs.

## Key feasibility findings

- **Verdict:** feasible, and further along than raw LOC implies. Migration is a 4-phase plan
  (A→D): **Phase A done, Phase B ~90% done** (only "gap #4" left); "everything to Go" = Phase C+D,
  not started **except agent detection, already ported** (`internal/detect`).
- **Rust core size:** 136,809 LOC total but **~53% is inline tests** → **~64.5k production LOC**.
  Already handled: VT emulation (`src/ghostty` ~5.4k) via go-libghostty; PTY (`src/pty`) via
  creack/pty. The ~100k test LOC is a porting *asset* (spec for Go table tests).
- **The "pure-Go" asterisk:** the VT engine is Zig `libghostty-vt`, statically linked via CGO in
  go-libghostty. A fully-migrated herdr is "a Go app with one isolated CGO dep," not zero-native.
  Don't reimplement a VT emulator in Go. Cost is toolchain fragility (Zig 0.15.2 pin + the
  macOS-26.5 `arm64-macos` SDK-slice workaround, already scripted under `.tools/`); CI must cache
  the prebuilt `.a`.
- **Concurrency is favorable:** herdr is event-loop-over-channels (357 mpsc refs, only 112
  `.await`), maps ~1:1 onto goroutines+select — little async coloring to unwind.
- **The two hard problems, both defused by web-only:** (1) ratatui UI (~6.2k, no Go equivalent) —
  **deleted** by rendering chrome as HTML; (2) SCM_RIGHTS live handoff — **likely obsolete** given
  the persistent Go daemon already keeps shells across a restart (delete, don't port).

## Decision recorded: web-front-end only

The browser is the **only** front-end. The native ratatui TUI (`src/ui` ~6.2k), native attach
client (`src/client` ~1.8k), and SSH thin-client (`src/remote` ~1.7k) are **retired, not ported**
(~12k LOC deleted). Chrome renders as HTML/Element; each pane is a canvas fed by the per-pane cell
grids `internal/orchestration` already produces. Keep the termhost daemon split (free
crash-survival from Phase B). **Net-to-port drops from ~55–58k to ~44k prod LOC** + net-new web UI.

**WS8 correction (from the user):** the Phase-A browser UI already works — canvas cell renderer,
keyboard/SGR-mouse input, text+image paste, OSC 52 clipboard, OSC 8 hyperlinks, title, toasts. So
WS8 is **partly built**; net-new work is (a) split the single composited grid into per-pane
canvases positioned by WS1 layout rects, (b) render chrome as HTML, (c) wire UI gestures to WS9.

## WS0 & WS1 scoping — the two corrections that reshaped them

**WS0 (Rust — flip termhost default, delete in-process path):**
- **Don't delete `src/terminal/` or `src/protocol/wire.rs`** — they're shared/ghostty-free. The
  in-process VT is `src/pane/terminal.rs` (`GhosttyPaneTerminal`) + `src/ghostty/` + `src/pty/`.
- **`build.rs` builds Zig unconditionally**, independent of the `termhost` feature — dropping Zig
  is a separate `build.rs` edit. Cargo has `termhost = []` and **no `default` key**, so in-process
  is today's default; `connect_backend` silently falls back to in-process when the daemon is
  unreachable.
- Staged A→D: (A) `default=["termhost"]` + make unreachable-daemon a hard error; (B) relocate
  shared plain-data types (`FocusEvent`/`encode_focus`/`KittyImage*`) out of `src/ghostty` +
  replace the residual unfed `PaneTerminal` used for input-mode mirroring; (C) delete
  `PaneRuntimeIo::Actor` + arms, `on_read`/detection task, collapse ~15 accessors — **the test
  rewrite (~150+ emulator-bound tests) is the biggest cost**; (D) delete `pty/ghostty/
  pane/terminal.rs`, strip `build.rs` Zig, drop `portable-pty`. Acceptance: `cargo build` with
  `zig` off PATH, no `ghostty-vt` link step, termhost e2e green.

**WS1 (Go — core data model & layout):**
- The BSP tree in `src/layout.rs` (905 LOC) is **essentially pure** (only imports ratatui
  `Rect`/`Direction`, no `crate::` deps, stores pane *identity* only) → port to `internal/layout`
  **first**, with its 10 tests (crown jewels: the 4-direction resize test and the
  `find_in_direction` overlap-tiebreak test — match `split_rect` u16 round/saturating-sub and the
  `(edgeDist,-overlap,centerDist,index)` tiebreak exactly).
- `Tab`/`Workspace` (`src/workspace.rs` + `workspace/tab.rs`) port to `internal/workspace` behind a
  **`PaneSpawner` seam** (fake spawner for tests, mirroring Rust `test_new`/`test_split`). Port the
  base32 id + stable public pane/tab-number helpers (7 tests).
- **Deferred (not WS1):** git (`workspace/git/*` ~2383 LOC) behind a `GitProvider`; `aggregate.rs`;
  the multi-workspace collection (app-state).

## Next steps

- **Pre-req:** commit the uncommitted scenario-C live-handoff fix in `~/projs/rust/herdr` (WS0
  edits the same files).
- WS0 and WS1 are independent → parallelizable. Recommended first move: **execute WS1 Stage 1**
  (`internal/layout` + its 10 tests) — risk-free foundation, no external deps.
- Open decisions still to lock (in the plan doc): daemon-split vs in-process; Windows support;
  in-app manifest updates; WS10 auth model; WS0 fallback policy + handoff redefinition.
</content>
