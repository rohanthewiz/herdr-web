# WS9 Stages 1–2 — browser protocol spec + `internal/browserproto`

**Session id:** `0b0f1ccc-8494-4e48-a6a2-7ce27e92d267`
**Date:** 2026-0711-0136 (session started 2026-07-03) · **Repo:** this repo only
(branch `roh/phase-b`); herdr (Rust) untouched.
**Continues:** `2026-0703-0019-ws0-stage-d-zig-free-build.md`.

> **Planning + implementation session.** Verified the WS0/WS1 baseline, planned WS9
> from the feasibility analysis Part 2 (`ai_docs/phase-c-ws9-tasks.md`), then executed
> **Stage 1** (protocol spec) and **Stage 2** (Go message layer). Stages 1–2 are checked
> off in the task doc.

---

## Commits

- herdr-web `e4f4613` **feat(browserproto): WS9 browser protocol — spec + Go message
  layer (stages 1-2)** — 12 files, +2,289 lines (spec doc, task doc, package + tests).

## Pre-work: baseline verification + standard-herdr isolation

- User runs **standard herdr** (Homebrew `/opt/homebrew/bin/herdr`, `~/.config/herdr/`)
  — confirmed never touched; dev/test uses the `herdr-dev` namespace under temp dirs.
- Full WS0 re-verification green: Go build+tests; fresh ghostty-tagged daemon; zig-free
  Rust build (0 ghostty symbols, 0 prod portable-pty); **1701/1701** unit; **70/70**
  integration (termhost_e2e 12, live_handoff 16, client_mode 16, server_headless 15,
  detach_reattach 11).
- Cleaned two rounds of leaked test daemons (pkill scoped to test-namespace sockets).
  Root cause of overnight persistence: live_handoff leaks attached `herdr server`
  clients, defeating the daemon's 10-min idle reaper. **Flagged harness fix, not done.**

## WS9 plan (task doc) + decisions

`ai_docs/phase-c-ws9-tasks.md`: 4 stages. Decisions locked provisionally 2026-07-03
(AskUserQuestion timed out; user's "continue with stage 2" implicitly accepted them):
- **D1** sparse-index diffs `{i, cell}` to the browser (β's skip-flag stays β-internal).
- **D2** packed-u32 colors (`0x02_RR_GG_BB`) on the wire; JS resolves to CSS; γ's
  `wire.ColorToCSS` is NOT ported.
- **D3** computed rects (`PaneInfo` + `SplitBorder`) — the BSP tree never crosses the wire.
- **D4** structured input with server-side VT encoding (retires browser JS key table,
  kitty bits-2/8 degradation, XTMODKEYS Enter special-case — once WS2 wires it).

## Stage 1 — `ai_docs/phase-c-ws9-protocol.md` (spec v1)

§0 server owns all state, chrome is data, panes addressed by uint32, dialogs
chrome-local · §1 JSON text WS frames `{"t":…}`, PV=1, unknown `t` ignored, cmd `id` is
a **string** · §2 `init`→`welcome`→initial full state; reconnect = fresh session · §3
`layout` (full replacement; rects + opaque border ids), `agents` rollup (global),
per-pane chrome events · §4 `pane_frame`/`pane_diff` (f/b omitted when == def) · §5
clipboard/notify/title/error/shutdown/update_ready · §6 structured `key` (W3C
code/key/mods/kind), `mouse` (cell coords), paste/image/resize, deprecated `raw` · §7
`cmd` envelope reusing the control-API vocabulary (one WS2 command table for browser +
CLI) · §8 frames only for the connection's active workspace+tab; chrome unfiltered ·
§9 α/β/γ disposition + seam-doc answers · §10 deferred (binary cells, kitty graphics,
layout diffs, auth→WS10, Node tree→WS3). Command set cross-checked against the Rust
control API `Method` enum (src/api/schema.rs:22) and `NavigateAction` set
(src/app/input/navigate.rs:480).

## Stage 2 — `internal/browserproto`

- **`proto.go`** ProtocolVersion=1 (independent of β's), `Type` consts, `Marshal`,
  `DecodeUp`/`DecodeDown` (unknown `t` → `ErrUnknownType`, callers drop per spec).
- **`down.go`** all 17 down messages. **`up.go`** all 8 up messages + mods/kind consts.
- **`cmd.go`** 23 command names + typed params structs; `SplitDirection`("h"/"v") and
  `NavDirection`("left"…) mappers onto `internal/layout` types.
- **`layout.go`** `BuildLayout(workspaces, active, area)` — zoomed tab = focused pane
  full-area, no borders. **Border ids are a stateless path encoding** `"r"+('0'|'1')*`
  (`BorderID`/`BorderPath`) — no server-side table; browser treats them opaquely
  (spec §3 updated to say so).
- **`frame.go`** `FrameTranslator` — stateful **per pane per connection**: emits full on
  β-full / first frame / `Reset()` (pane newly visible, §8) / diff touching **>3/5** of
  cells (free to decide: β diffs carry the whole resolved grid, skip-flagged).
  `def_fg`/`def_bg` = **dominant (most frequent) colors** of the full frame — β resolves
  all cells, terminal defaults are unknown here — held fixed across diffs so omitted
  colors resolve correctly. Links → 1-based `h` into the frame table. `ModesFrom`
  reduces β's 10 mode fields to `{mouse, alt_screen}`.
- **One change outside the package:** `workspace.PublicPaneID(paneID)` accessor
  (`workspace.go:311`) renders `"w1:p3"` — accessor only.

### Tests (73 runs, all green; ghostty-tagged build verified too)

- `TestRoundTrip` — every message Marshal→Decode→DeepEqual. `TestWireShapes` — exact
  JSON strings pinned against the spec (omission rules can't drift). Direction
  separation (up type unknown to down decoder), cmd params round-trip, cell omission.
- Layout: `BuildLayout` (2 workspaces, split, border pos/ratio), second tab not active,
  zoomed, edge cases, scrollbar passthrough; `TestBorderIDDrivesResize` (border id from
  the layout msg → `BorderPath` → `SetRatioAt` works).
- Frame: full/diff/diff-before-full/threshold-boundary (6/10 stays diff, 7/10 full)/
  Reset/links+scroll/ModesFrom.
- **`TestReplayReconstruction` (task 2.3 property test):** 60-step seeded replay through
  the REAL `orchestration.FrameFromSnapshot`; asserts browser-side reconstruction ==
  β-side fold every step, plus diff-index ordering/bounds and no-links-in-diffs.

### Finding: β link-removal non-propagation

The property test's first run failed and surfaced a real β quirk: `resolveCell`'s
skip comparison **ignores `Link`**, so a hyperlink removed with no other cell change is
never propagated by a β diff — the fold retains it stale. In practice links on screen
force full frames (`HasHyperlinks`), so it only bites synthetically; the test models
the fold faithfully and the generator honors the realistic invariant (link changes
accompany content changes). β left untouched per the out-of-scope rule.

## Next

- **Stage 3** (structured input, D4): spike first — does go-libghostty expose the
  key-event VT encoder? If not, port the pure Rust encoders (pinned by the Stage-B
  differential test). Can run before/parallel to Stage 4.
- **Stage 4** proof harness: gateway spike speaking the new protocol, panes directly
  from the termhost daemon, two-pane split, minimal JS.
- Flagged, not done: live_handoff harness fix (leaked attached `herdr server` clients).
