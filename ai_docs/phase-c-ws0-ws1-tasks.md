# Phase C — WS0 & WS1 actionable task lists

**Date:** 2026-07-01. Companion to `ai_docs/fbl_go_port_feasibility_analysis.md` (Part 2).
Anchors are `file:symbol`/`file:line` in the two repos:
- **WS0** = Rust-side, `~/projs/rust/herdr`.
- **WS1** = Go-side, `~/projs/go/herdr-web`.

WS0 and WS1 have **no dependency on each other** and can proceed in parallel. Line numbers are
from the checkout mapped 2026-07-01 (local Rust = v0.6.10) — re-confirm before editing.

**Pre-req (housekeeping):** commit the uncommitted scenario-C live-handoff fix sitting in the
Rust working tree before starting WS0 (it touches `pane.rs`/`terminal/runtime.rs`/handoff, which
WS0 also edits).

---

# WS0 — Flip termhost to default & delete the in-process PTY/VT/ghostty path

**Goal:** make the Go termhost daemon the unconditional terminal backend and remove Rust's
in-process emulator so `cargo build` no longer needs a Zig toolchain / never links libghostty-vt.

### Scope corrections (do NOT delete these)
- **`src/terminal/`** is the *shared* terminal layer (`TerminalRuntime` is a newtype over
  `pane::PaneRuntime`; `state.rs`, `metadata.rs`, `runtime_registry.rs`, `id.rs` have **zero**
  ghostty refs). It survives. The in-process VT actually lives in **`src/pane/terminal.rs`
  (`GhosttyPaneTerminal`)**, **`src/ghostty/`**, and **`src/pty/`**.
- **`src/protocol/wire.rs`** (no ghostty dep) defines the termhost render types (`CellData`,
  `FrameData`, `CursorState`, `u32_to_color`, `u16_to_modifier`). Keep it. Its
  `#[cfg_attr(not(any(test, feature="termhost")), allow(dead_code))]` on `wire.rs:678,718`
  becomes unconditionally live once termhost is default.
- **The Zig build (`build.rs`) is gated independently of the `termhost` feature** — it runs on
  every build regardless. Dropping Zig is a `build.rs` edit, separate from the feature flip.

## Stage A — Make termhost the unconditional default (no deletes) — DONE (herdr `2f267ef`)
- [x] **A1.** `Cargo.toml [features]`: `default = ["termhost"]` added. `--no-default-features`
  still builds the legacy in-process-only binary until stages C/D.
- [x] **A2. Fallback policy — DECIDED & RECORDED:** unreachable/unconfigured daemon is a **hard
  error** at pane creation (`termhost::required_backend()` → `io::Error` with guidance);
  `HERDR_TERMHOST_INPROCESS=1` is the transitional escape hatch (deleted with the in-process
  path in stage C). **Daemon discovery** when no env var names it: `herdr-termhost` (or dev
  `termhost`) sibling of the herdr executable, then `herdr-termhost` on PATH; env vars override.
  Unit tests keep pre-flip in-process behavior via `cfg(test)` until the C6 rewrite.
- [x] **A3. Verified.** `cargo build` green (needs `ZIG=<herdr-web>/.tools/zig-wrapped` on macOS
  26.5); 1921 unit tests pass (full-suite flakes pre-exist — green in isolation and on the
  pre-change tree); `termhost_e2e` **12/12 with a real daemon, no skips** (both
  `HERDR_TERMHOST_SOCKET` hand-launched and `HERDR_TERMHOST_BIN` managed modes — beware: those
  tests silently SKIP-pass when the env vars are unset); headless-server smokes confirmed
  discovery→spawn→connect (daemon survives herdr death), the hard error at startup-workspace
  creation, and the escape hatch spawning in-process.

## Stage B — Relocate shared plain-data types out of `src/ghostty/` — DONE (herdr `c789343`)
- [x] **B1.** `src/terminal/types.rs` now owns `FocusEvent` + `encode_focus` (**pure** CSI I/O
  pair — ghostty's FFI `ghostty_focus_encode` wrapper deleted outright) and the `KittyImage*`/
  `KittyPlacementRenderInfo` structs. All consumers repointed; ghostty imports them back.
- [x] **B2. DECIDED & DONE: small Rust encoder now (revisit in WS9).** `PaneTerminal` is an enum:
  `Ghostty(emulator)` in-process / `Mirror(InputMirror)` termhost — termhost panes construct **no
  Zig terminal at all**. New `src/pane/input_mirror.rs` mirrors the Go-reported modes
  (`PaneSignal::Modes`) + pure `crate::input` encoders + pure `KittyKeyboardTracker`. Parity
  pinned by a **differential test** (mirror vs ghostty-backed mirror, 45 combos × key/mouse
  matrix); pure-encoder fixes it forced: Esc→`CSI 27 u` under kitty, Shift+Tab→`CSI 9;2u`,
  DECCKM SS3 only under legacy protocol, no wheel in X10. *Known divergence:* kitty
  report-event-types/report-all-keys (bits 2/8) degrade to legacy-compatible output until WS9.
- [x] **B3. Verified.** Build + clippy clean; 1923 unit tests; `termhost_e2e` 12/12 vs live
  daemon (keys now encode through the mirror). **Stage-A gap found & fixed:** the non-termhost
  integration suites (20 spawn sites) now pin `HERDR_TERMHOST_INPROCESS=1` — post-flip they'd
  hard-error without a discoverable daemon (C6 rewires them properly). Pre-existing environmental
  failures on this machine (fail identically at `417b4b1` in isolation): `api_ping::events_
  subscribe_streams_output_and_agent_status_events` + 2 `cross_area` agent-survival tests.

## Stage C — Remove selection/fallback + the in-process arms — DONE
## (herdr `c7c89ef` + `4875ed8` + `70b6e29`, herdr-web `21f65ce`)
- [x] **C1.** `termhost::required_client()` replaces `client_if_enabled()`+`BackendChoice`;
  `HERDR_TERMHOST_INPROCESS` and the `cfg(test)` in-process default are gone. `finish_termhost`
  is the sole real constructor. Under `cfg(test)`, `spawn*` returns a channel-backed fake
  runtime (seeds cwd + restore history, echoes input into content, executes explicit argv/shell
  commands as plain subprocesses for marker-file/PaneDied semantics; never records the child pid
  — it shares the test runner's session).
- [x] **C2.** `PaneRuntimeIo::Actor` + all arms deleted; `TestChannel` is the test double.
  `src/pty/` is dead code behind a transitional `#![allow]` (deleted in D).
- [x] **C3.** In-process read path, child watcher, Rust detection task, and
  `pane/agent_detection.rs` deleted. `begin_graceful_release`/`reset_agent_detection`/
  `set_full_lifecycle_authority_active` are documented no-ops until agent lifecycle moves
  Go-side. Dead detect/process-probe helpers carry "delete with the detect-port workstream"
  allows.
- [x] **C4.** `PaneTerminal` is now `Mirror(InputMirror)` | `#[cfg(test)] Fake(FakePaneTerminal)`
  — `GhosttyPaneTerminal` and the emulator trackers (`pane/{osc,cursor,input,xtgettcap}.rs`,
  ~3.7k LOC) are deleted. Prod accessors answer from the Go backend (termhost arm) with the
  Mirror returning empty defaults; the render/dirty-patch wire conversion is shared
  (`render_wire_frame`/`wire_dirty_patch`) so tests exercise the prod path.
- [x] **C5. DECIDED: `from_handoff_fd` is deleted, not redefined.** Handoff = the replacement
  server reconnecting to the persistent daemon and adopting live shells (welcome.panes);
  `persist/restore.rs` treats any fd-passed pane from an older binary as a failed import and
  respawns it seeded with snapshot history (fd closed, not leaked). The `is_termhost()` skips in
  headless.rs remain the only path (the fd-dup/manifest machinery degenerates to empty sets;
  full removal deferred to D/later). Fixes shipped with this: failed-handoff **rollback
  reattaches** to the daemon (`TermhostClient::reattach`; detach-before-attempt used to leave
  panes dark), an aborted import **preserves** adopted panes instead of closing them
  (`preserve_runtimes_for_failed_handoff` + Drop honoring preserve for the daemon pane),
  respawn-after-exit drops the old runtime before spawning (daemon pane-id namespace race), and
  surviving-pane adoption is claim-once (`claim_surviving_pane`). Launch-argv respawn semantics
  now follow `adopted_live_shell()` (the successor to the fd-import marker).
- [x] **C6.** Unit tests: the six `test_with_*` helper signatures are unchanged, reimplemented on
  `pane/fake_terminal.rs` — a deliberately tiny VT interpreter (text + line discipline, DEC
  private-mode whitelist, basic SGR, cursor addressing, DECSCUSR, OSC 8, kitty CSI-u, XTMODKEYS)
  over a wire-shaped grid; unknown sequences panic. Mode state + encoders delegate to the real
  `InputMirror`. All ~119 helper call sites run unmodified; emulator-bound tests
  (`pane/terminal.rs` 77, handoff/detection tests in `pane.rs`, the ghostty differential test)
  deleted; `ghostty/mod.rs` tests survive until D deletes the module. Integration tests: the 20
  `HERDR_TERMHOST_INPROCESS` pins are now `HERDR_TERMHOST_BIN` via
  `support::termhost_daemon_bin()` (hard error with build instructions when the daemon binary is
  missing — no silent skips). `live_server_holds_one_pty_master_fd_per_pane` inverted to
  `live_server_holds_no_pty_master_fds`. **Go daemon (herdr-web `21f65ce`)** now scans the raw
  stream for XTMODKEYS and reports `modify_other_keys` in `pane_modes`, closing the stage-B
  divergence (modified Enter survives handoffs as CSI 27;mod;13~; the mirror also encodes it).
- [x] **C7. Verified.** `cargo build`/`clippy` clean; unit suite 1736/1736; `termhost_e2e` 12/12
  vs live daemon (both socket + managed modes, no skips); live_handoff 16/16, client_mode 16/16,
  server_headless 15/15, detach_reattach 11/11, cross_area 7/9, api_ping 10/11, multi_client
  10/11 — the 4 failures are the pre-existing machine-environmental set (multi_client broadcast
  verified failing identically at the stage-B commit). `auto_detect`/`cli_wrapper` are
  Linux-gated. Requires the herdr-web daemon at `21f65ce`+ next to the herdr binary.

## Stage D — Delete dead modules + drop the Zig/ghostty build
- [ ] **D1.** Delete `src/pty/`, `src/ghostty/`, `src/pane/terminal.rs`; remove `mod pty;`/
  `mod ghostty;` (`main.rs:56,69`) and `mod terminal;`/`use self::terminal::…` (`pane.rs:32,42-43`).
- [ ] **D2.** `Cargo.toml`: drop `portable-pty` (line 28); collapse/remove the `termhost` feature
  if you made it unconditional.
- [ ] **D3.** `build.rs`: delete the Zig invocation (lines ~65-83) + link directives (85-96) +
  `vendor/libghostty-vt` `rerun-if-changed`. Reduce to build-info stamping, or remove `build.rs`
  and the `build = "build.rs"` Cargo line.
- [ ] **D4. ACCEPTANCE.** On a no-Zig environment (`env -u ZIG`, PATH without `zig`):
  `cargo build --release` succeeds with **no** `libghostty-vt` link step; `cargo test` passes;
  `tests/termhost_e2e.rs` passes against a live daemon; `cargo tree` shows no `portable-pty`;
  link logs / `nm` show no `ghostty-vt`.

**WS0 risk:** the test rewrite (C6) is the biggest effort, not the deletes. Sequence A→B→C→D so
every stage compiles and tests independently.

---

# WS1 — Core data model & layout → Go (`internal/layout`, `internal/workspace`)

**Goal:** port the pure BSP pane tree + workspace/tab bookkeeping to Go with **no terminal-backend
coupling**, gated behind a `PaneSpawner` seam, validated by the ported Rust tests.

### Scope facts
- The BSP tree lives in **`src/layout.rs` (905 LOC), essentially pure** — its only non-data import
  is `ratatui::layout::{Direction, Rect}` (`:5`); **no `crate::` imports**. Stores pane *identity*
  (`PaneId`) only, never content; rects are computed on demand from a passed-in screen `Rect`.
- `Tab`/`Workspace` **don't hold content** but *create* it (spawn PTYs) → need a spawner seam.
- **Defer:** `workspace/git/*` (~2383 LOC subprocess I/O), `workspace/aggregate.rs` (agent rollup,
  coupled to detect/terminal), and the multi-workspace collection + create/close/switch (lives in
  `src/app`/`src/session.rs`, not WS1).

## Stage 1 — `internal/layout` (pure BSP core; port FIRST)
- [x] **L1. Value types** (replace ratatui with own structs): `Rect`, `Direction{Horizontal,
  Vertical}`, `NavDirection{Left,Right,Up,Down}` (`layout.rs:60`), `PaneID` (typed `uint32` +
  **injectable allocator** for deterministic tests, cf. atomic `NEXT_PANE_ID` `:11`), `Node`
  (sealed iface `PaneNode`/`SplitNode`, or tagged struct — `layout.rs:68`), `TileLayout{root,
  focus}` (`:79`), `PaneInfo{id, rect, innerRect, scrollbarRect, isFocused}` (`:31`),
  `SplitBorder{pos, direction, ratio, area, path []bool}` (`:45`).
- [x] **L2. Geometry core** `splitRect(area, dir, ratio)` (`layout.rs:588`): `first =
  round(len*ratio)`, `second = len - first`, **saturating subtraction**. Match `u16` rounding/
  saturation exactly — correctness-critical.
- [x] **L3. Tree recursion** (all pure): `countPanes`(377) `collectPanes`(384) `collectSplits`(409)
  `collectIDs`(438) `splitRatios`(448) `swapPaneIDs`(474) `splitAt`(490) `removePane`(527, collapse
  = promote surviving sibling) `setRatioAt`(550) `getRatioAt`(568).
- [x] **L4. `TileLayout` public API:** `New`(87) `Panes(area)`(107) `Splits(area)`(114)
  `SplitFocused[WithRatio]`(121/126) `CloseFocused`(136, incl. next-focus pick) `FocusPane`(159)
  `SwapPanes`(167) `SetRatioAt`(180) `ResizeFocused`(186) `ResizePane`(212) `PaneIDs`(230)
  `PaneCount`(102) `Root`(237) `FromSaved`(243). Clamp ratios **0.1–0.9** (`valid_split_ratio` :519).
- [x] **L5. `FindInDirection`** (`layout.rs:251`) — directional focus nav; tuple tiebreak
  `(edgeDistance, -overlap, centerDistance, index)`; helpers `rangesOverlap`(308)
  `rangeOverlapAmount`(363) `rangeCenterDistance`(369). **Subtle — match tiebreak order exactly.**
- [x] **L6. Resize helpers:** `splitOnRequestedEdge`(312) `splitAreaOverlapsFocusedPane`(316)
  `nearestResizeSplit`(327) `oppositeDirection`(341) `splitEdgeDistance`(350).
- [x] **L7. Port the 10 `layout.rs` tests** (`:609` mod) as Go table tests, incl. fixtures
  `sample_layout`(617) + `split_snapshot`(654). Crown jewels: `resize_outer_edges_shrink_
  focused_pane`(751, all 4 dirs), `resize_outer_edge_falls_back_to_{horizontal,vertical}_ancestor_
  split`(786/815), `find_in_direction_tiebreaks_by_larger_overlap_before_layout_order`(875).
  **Acceptance:** all 10 pass.

## Stage 2 — public numbering helpers
- [x] **N1.** Port base32 id + public-number logic (`workspace.rs`): `generate_workspace_id`(75)
  `encode_public_number`(80) `decode_public_number`(94) `public_workspace_number`(107)
  `reserve_workspace_ids`(115) → `internal/workspace/ids.go`. Alphabet at `:73`.
  *(`register_new_pane_with_number`(851) `unregister_pane`(856) `public_pane_number`(713)
  `public_tab_number`(717) are `Workspace`-struct methods → land with Stage 3/W3.)*
- [x] **N2.** Port the id/number tests (`workspace.rs:954-1039`): base32 handling, encode/decode
  round-trip, `reserve_workspace_ids` (3 tests, pass under `-shuffle`). *(Pane & tab numbers
  stable + not reused after close need the spawner seam → covered by Stage 3/W5.)*

## Stage 3 — `internal/workspace` (Tab/Workspace pure bookkeeping + spawner seam)
- [x] **W1. `PaneSpawner` interface** — `Spawn(spec) (PaneID, TerminalID, error)` / `Despawn(PaneID)`
  (replaces `TerminalRuntime::spawn*`). Provide a **fake/no-op spawner** for tests (mirrors Rust
  `Workspace::test_new`(871)/`test_split`(915)/`test_add_tab`(924)).
- [x] **W2. `Tab`** (`tab.rs:32`): fields `customName, number, rootPane, layout TileLayout,
  panes map[PaneID]PaneState, zoomed`; `PaneState{TerminalID, Seen}` (`pane/state.rs:6`). Pure
  methods: `SplitFocused[WithRatio]`(196/221 → `layout.SplitFocused` + `panes` insert + `zoomed=
  false`, spawn via seam), `CloseFocused/ClosePane/RemovePane`→`detachPane`(391-404:
  `layout.CloseFocused` + `panes` remove + `promotedRootIfNeeded` :429 + `zoomed=false`; returns
  `(PaneID, TerminalID)`).
- [x] **W3. `Workspace`** (`workspace.rs:140`): fields `id, customName, identityCwd, tabs []Tab,
  activeTab int, publicPaneNumbers, next*Number, git-cached fields as plain optionals`. Replace
  Rust `Deref`→active-tab with an explicit `ActiveTab()` accessor. Methods: `SwitchTab`(316, flips
  `seen`) `CreateTab`(327/347) `CloseTab`(408, fix `activeTab` index) `MoveTab`(424, keep active
  identity via `rootPane`) + split/close orchestration (453-819: tab-index math +
  `findTabIndexForPane` :801 + numbering; spawn via seam).
- [x] **W4.** Model zoom as just the `zoomed bool` (no algorithm here; the toggle is app-level at
  `app/input/navigate.rs:824`, rendering honors it — out of WS1).
- [x] **W5. Port the 7 workspace tests** (`workspace.rs:950`) using the fake spawner — pane/tab
  numbers stable & not reused, `move_tab` keeps active identity, identity-follows-cwd.
  **Acceptance:** pass.

## Stage 4 — deferred (flagged, NOT WS1)
- Git behind a `GitProvider` interface (`workspace/git/*`); the cached-git `Workspace` fields become
  plain optionals fed by it.
- `aggregate.rs` (agent-state rollup) — port with the detect/terminal work.
- Multi-workspace collection + workspace create/close/switch — app-state (`src/app`, `src/session.rs`).

**WS1 acceptance:** `internal/layout` + `internal/workspace` compile; ported tests (10 + numbering +
7) pass; **zero** import of any terminal backend (only the `PaneSpawner` interface).

**Suggested order:** L1→L7 → N1→N2 → W1→W5. Do `internal/layout` end-to-end (with tests green)
before touching workspace — it's the risk-free foundation and its tests pin the exact geometry/nav
semantics everything else assumes.
</content>
