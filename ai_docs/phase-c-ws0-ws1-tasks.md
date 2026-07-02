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

## Stage A — Make termhost the unconditional default (no deletes)
- [ ] **A1.** `Cargo.toml [features]` (line 14): add `default = ["termhost"]` (feature is
  `termhost = []`, line 18; there is currently **no** `default` key, so in-process is default).
  Keep the `Actor` arms compiling. → `cargo build` builds with termhost on.
- [ ] **A2. Decide fallback policy.** `connect_backend()` (`src/termhost/mod.rs:89`) returns
  `None` when the daemon is unreachable/unconfigured, and the per-pane selector silently falls
  back to in-process (`src/pane.rs:1804-1824`). Change to a **hard error** (or a temporary
  `HERDR_TERMHOST_INPROCESS=1` escape hatch) so "default" truly lands on termhost. **Decision to
  record.**
- [ ] **A3. Verify.** `cargo build`, `cargo test`; run `tests/termhost_e2e.rs` with a real daemon
  (`HERDR_TERMHOST_BIN`); confirm a normal run drives panes through the Go daemon.

## Stage B — Relocate shared plain-data types out of `src/ghostty/`
These leak onto the app/termhost surface and must move before ghostty can be deleted.
- [ ] **B1.** Create a ghostty-free module (e.g. `src/terminal/types.rs`, or fold into
  `protocol/wire.rs`) and move: `FocusEvent` (`ghostty/mod.rs:107`) + `encode_focus` (`:531`),
  `KittyImageFormat` (`:185`), `KittyImagePlacement` (`:192`), `KittyImageDescriptor` (`:208`),
  `KittyPlacementRenderInfo` (`:219`). Repoint consumers: `kitty_graphics.rs:14,799`;
  `app/api.rs:592,595,631`; `terminal/runtime.rs:342-344,369`; `pane.rs:2759-2761,2800,2809`.
- [ ] **B2. Replace the residual `PaneTerminal` uses on the termhost path.** `PaneRuntime.terminal`
  currently holds an *unfed* `GhosttyPaneTerminal` even for termhost panes, used only for
  input-mode mirroring + key/mouse encoding (`pane/terminal.rs:193,754`, `#[cfg(feature=
  "termhost")]`). Provide a ghostty-free key/mouse encoder + input-mode state so `.terminal` no
  longer needs `GhosttyPaneTerminal`. **Decision to record:** keep a minimal Rust encoder now, or
  move key encoding to Go (WS9) — recommend a small Rust encoder for WS0, revisit in WS9.
- [ ] **B3. Verify.** `cargo build`/`cargo test` green (still linking Zig at this stage).

## Stage C — Remove selection/fallback + the in-process arms
- [ ] **C1.** Drop the `client_if_enabled()` guard (`pane.rs:1804`) so termhost is unconditional;
  make `finish_termhost` (`pane.rs:2305`) the sole `PaneRuntime` constructor.
- [ ] **C2.** Delete `PaneRuntimeIo::Actor` (`pane.rs:864-865`) and every `Actor` arm across its
  methods (`termhost_pane` 878, `shutdown` 885, `is_termhost` 902, `duplicate_handoff_fd` 914,
  `foreground_process_group_id` 929, `begin_handoff` 940, `set_handoff_paused` 952,
  `release_after_commit` 969, `resize` 979, `nudge_child_redraw_after_handoff` 1013,
  `send_bytes` 1031, `try_send_bytes` 1045). Convert `TestChannel` (`869`) into the test double.
- [ ] **C3.** Delete the in-process read path: `on_read`/child-watcher (`pane.rs:1826-1913`) and
  the Rust detection task (`pane.rs:1915-2290`) — Go owns detection (`internal/detect`).
- [ ] **C4.** Collapse the ~15 backend-split accessors to their termhost arm, removing the
  `.terminal.*` fall-through: `scroll_up`(2540) `scroll_down`(2550) `scroll_reset`(2560)
  `set_scroll_offset_from_bottom`(2570) `scroll_metrics`(2583) `cursor_state`(2599)
  `visible_text`(2642) `visible_ansi`(2650) `detection_text`(2658) `recent_*`(2676-2705)
  `extract_selection`(2713) `render`(2725) `collect_dirty_patch`(2734) `visible_hyperlinks`(2746).
- [ ] **C5.** Redefine `from_handoff_fd` (`pane.rs:1625`, in-process fd import) per the
  persistent-daemon handoff model (handoff = new server reconnects to the live daemon). Confirm
  the `is_termhost() → continue` skips in `server/headless.rs:840-847,948,1032` become the only
  path. **Decision to record.**
- [ ] **C6. Test rewrite (largest hidden cost).** Delete in-process-emulator unit tests
  (`ghostty/mod.rs` ~21, `pane/terminal.rs` ~77, `pane.rs` ~55) and rewrite the shared-helper
  tests onto a termhost/synthetic double: `test_with_screen_bytes` / `test_process_pty_bytes` /
  `test_with_scrollback_bytes` (defined in `pane.rs`, re-exported `terminal/runtime.rs:444-483`)
  are used by tests in `app/api.rs`, `app/input/{copy_mode,mouse,navigate,terminal}.rs`,
  `app/runtime.rs`, `ui.rs`, `ui/panes.rs`, `server/headless.rs`, `persist/snapshot.rs`,
  `pane/{input,osc}.rs`. Keep `protocol/wire.rs` (47) and `terminal/state.rs` (64) tests.
- [ ] **C7. Verify.** `cargo build` (still Zig), `cargo test`.

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
