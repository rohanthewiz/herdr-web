# WS1 (Stages 1–3) — layout + workspace ported from Rust to Go

**Session id:** `2c92c617-b9d1-457a-8328-0465082a4d15`
**Date:** 2026-0701-2310 · **Repo:** `~/projs/go/herdr-web` · **Branch:** `roh/phase-b`
**Continues:** `2026-0701-2237-herdr-full-go-port-feasibility-phase-c.md` (plan doc:
`ai_docs/phase-c-ws0-ws1-tasks.md`).

> **Implementation session.** Executed all active stages of WS1 (core data model & layout):
> ported `src/layout.rs` and the `Tab`/`Workspace` bookkeeping from `~/projs/rust/herdr` into
> `internal/layout` + `internal/workspace`, with all Rust tests ported and green. Three commits,
> one per stage. Stage 4 (git/aggregate/multi-workspace) remains deferred by design.

---

## Commits (this session)

- `9bc4759` **feat(layout): port BSP pane-tiling tree (Stage 1)** — `internal/layout/layout.go`
  + `layout_test.go`.
- `9c28cbd` **feat(workspace): port public id/numbering scheme (Stage 2)** —
  `internal/workspace/ids.go` + `ids_test.go`.
- `2d57f5f` **feat(workspace): Tab/Workspace behind PaneSpawner seam (Stage 3)** —
  `internal/workspace/{spawner,tab,workspace}.go` + `workspace_test.go`.

## Stage 1 — `internal/layout` (BSP tree, L1–L7)

- Faithful port of `src/layout.rs` (905 LOC, pure). **Geometry parity is load-bearing** and
  matched exactly: `splitRect` = f32 round-half-away-from-zero saturated into u16
  (`roundF32ToU16`) + saturating remainder; nav tiebreak tuple
  `(edgeDistance, larger-overlap-first, centerDistance, index)` in `FindInDirection`.
- `Node` is a sealed interface (`*PaneNode`/`*SplitNode`); `TileLayout` API: `SplitFocused
  [WithRatio]` (ratio clamp 0.1–0.9, non-finite→0.5), `CloseFocused` (next-focus pick),
  `SwapPanes`, `ResizeFocused/ResizePane` (ratio-snapshot change detection), `Root`/`FromSaved`.
- `PaneID` = typed `uint32`; global atomic `AllocPaneID` (starts at 1) **plus injectable
  allocator** via `NewWithAllocator` for deterministic tests later.
- All **10 Rust tests ported** (4-direction outer-edge resize became a Go table test), incl. the
  crown jewels: ancestor-split fallbacks + overlap tiebreak. Micro-divergence: Go `CloseFocused`
  degrades safely where Rust would `unwrap`-panic on a corrupt-focus state (unreachable).

## Stage 2 — `internal/workspace/ids.go` (N1–N2)

- Bijective base-32 handles, alphabet `123456789ABCDEFGHJKMNPQRSTVWXYZ0` ("0" = digit 32):
  `Encode/DecodePublicNumber` (with Rust's checked-overflow guards), `GenerateWorkspaceID`
  ("w1", "w2", …), `PublicWorkspaceNumber`, `ReserveWorkspaceIDs` (CAS-raise; restored ids never
  reused).
- **Divergences (documented in code):** takes `[]string` ids instead of Rust's `&[Workspace]`;
  Go counter stores *last handed out* vs Rust's *next* (verified equivalent: reserve "wZ" → next
  is "w0" = 32 in both).
- 3 id tests ported; green under `-shuffle -count=3` (shared global counter, relative asserts).

## Stage 3 — `internal/workspace` Tab/Workspace + seam (W1–W5)

- **`spawner.go`:** `PaneSpawner{Spawn(SpawnSpec) (TerminalID, error); Despawn(TerminalID)}`.
  One `SpawnSpec` (rows/cols/cwd/argv/command/extraEnv/publicPaneID) absorbs Rust's
  spawn/shell/argv method fan. **Divergence from plan sketch:** `Spawn` does not return `PaneID`
  — layout allocates it (as in Rust); it rides in via `spec.PaneID`. `NewPane`/`DetachedPane`
  return `(PaneID, TerminalID)`; caller owns despawn-vs-reattach.
- **`tab.go`:** `Tab{CustomName, Number, RootPane, Layout, Panes map[PaneID]*PaneState, Zoomed}`;
  `PaneState{AttachedTerminalID, Seen}`. Split rolls back layout on spawn error; `detachPane`
  computes root promotion *before* closing; zoom is just the bool (W4).
- **`workspace.go`:** explicit `ActiveTab()` (replaces Rust `Deref`); stable numbering
  (`max(next, n+1)` advance; **tab numbers consumed even on failed create** — bug-for-bug);
  `SwitchTab` marks panes seen; `MoveTab` re-finds active tab by root-pane identity;
  `CloseTab/ClosePane/CloseFocused` with exact index fixups + tab collapse. Seams for deferred
  work: `TerminalCwdLookup func(TerminalID) (string, bool)` replaces the terminals-map +
  runtime-registry pair; git-cached fields are plain optionals (`*string`, `*AheadBehind`) for
  the Stage-4 `GitProvider`; `deriveLabelFromCwd` skips the git-repo-root branch until then.
- Remaining **4 of the 7 workspace tests ported** (pane/tab numbers stable + not reused,
  identity-follows-cwd → "pion", move_tab keeps active identity → labels `[foo, 3, 1]`), driven
  through a `fakeSpawner` that exercises the *real* constructors (better than Rust's `test_new`
  bypass).

## Verification / acceptance

- 17 tests green across both packages, `-shuffle -count=3`; whole repo builds + tests green.
- **WS1 acceptance met:** `internal/workspace` imports = `[errors, internal/layout, math, os,
  path/filepath, strconv, strings, sync/atomic]`; `internal/layout` = `[math, slices,
  sync/atomic]` — zero terminal-backend coupling.
- `ai_docs/phase-c-ws0-ws1-tasks.md` checkboxes updated (L1–L7, N1–N2, W1–W5) with notes on
  what moved where.

## Next steps

- **WS0** (Rust, `~/projs/rust/herdr`): flip termhost default + delete in-process VT path.
  **Pre-req:** commit the uncommitted scenario-C live-handoff fix in the Rust working tree
  (it touches `pane.rs`/`terminal/runtime.rs`/handoff, which WS0 also edits). → started next.
- Real `PaneSpawner` implementation backed by `internal/orchestration`/termhost (WS2+).
- WS1 Stage 4 deferrals: `GitProvider`, `aggregate.rs` rollup, multi-workspace collection.
