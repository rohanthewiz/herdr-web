# WS2 §7 deferred pane commands — focus_direction, cycle/swap/zoom/resize_border, last, rename

**Session id:** `f9bd05bb-9037-4106-9c43-c6cb0874ed63`
**Date:** 2026-0713-1258 · **Branch:** `roh/phase-b` (herdr-web)
**Continues:** `2026-0713-0934-ws2-orchestrator-event-loop.md`. That session landed the WS2
orchestrator (pure `internal/app` session + gateway2 event-loop actor, `75b2716`) with the
core §7 table and a list of *deferred* commands. This session works down that list.

> Implemented the entire remaining **pane** command group of §7
> (`ai_docs/phase-c-ws9-protocol.md`) — `pane.focus_direction`, `pane.cycle`, `pane.swap`,
> `pane.zoom`, `pane.resize_border`, `pane.last`, `pane.rename` — each as one `app.Session`
> method + one gateway2 dispatch case over existing layout primitives, with unit tests and
> live end-to-end verification (real termhost + gateway2 + wsprobe2). Three commits, all
> race-clean and green.

---

## Commits (this session)

| Commit | Commands |
|---|---|
| `915fbb8` | `pane.focus_direction` |
| `90d81e4` | `pane.cycle`, `pane.swap`, `pane.zoom`, `pane.resize_border` |
| `cd38e33` | `pane.last`, `pane.rename` |

Pattern held throughout: the pure `Session` already sits above the daemon seam, and the wire
params (`browserproto/cmd.go`) + layout geometry helpers were mostly pre-built, so each command
really was "one method + one dispatch case" as the prior session predicted. Geometry-dependent
commands take an `area layout.Rect` param (consistent with layout's `ResizeFocused`/`ResizePane`),
keeping `Session` pure.

## `915fbb8` — pane.focus_direction

- `Session.FocusPaneDirection(nav, area) (moved bool, err)`: resolves the nearest neighbour via
  `layout.FindInDirection` over `tab.Layout.Panes(area)`, moves focus. `false,nil` = no neighbour
  that way (no-op, not error). Stays within the active tab → focus-only, dispatch rebroadcasts
  `viewportLayout()` only when moved (like `pane.focus`).
- wsprobe2: `focusdir:left|right|up|down`. Unit test: move + edge no-op both ways.
- Live: split L/R; `focusdir left`→focused resolves to left pane, `focusdir right`→right. ✓

## `90d81e4` — pane.cycle / swap / zoom / resize_border

All four remaining pane-*layout* commands. Key facts discovered: `browserproto.BuildLayout`
**already** rendered `tab.Zoomed` (full-area focused pane, no borders) and `BorderID`/`BorderPath`
(opaque split-path handle `"r01"`), so zoom + resize_border wire/layout machinery pre-existed. A
pane resize emits a **full** frame (`FrameTranslator` → `translateFull` on `f.Full`), the same
proven path as window-resize, so swap/zoom/resize_border repaint correctly.

- `CyclePane(next) bool` — focus next/prev in `Layout.PaneIDs()` order, wrapping. Focus-only.
- `SwapPaneDirection(nav, area) (bool, err)` — `FindInDirection` + `Layout.SwapPanes`; the focused
  pane travels to the neighbour's slot and **keeps focus**. `applyModel` (slots/sizes).
- `ToggleZoom(target?) (bool, err)` — flips `tab.Zoomed`; `VisiblePaneIDs` and gateway2
  `desiredGrids` became **zoom-aware** (zoomed pane = sole viewport pane sized to full area;
  hidden siblings stay live PTYs at split size so `syncDaemon` won't close them). `applyModel`.
- `ResizeBorder(path, ratio)` — `BorderPath` decode → `Layout.SetRatioAt`. `applyModel`.
- Front-end: ⤢ zoom button on pane chrome (indicator was already rendered). wsprobe2:
  `cycle`/`swap`/`zoom`/`resizeborder` ops + a **`rect:PANE:x|y|w|h:eq|lt|gt:N`** assertion
  (polls a pane rect field from the last layout — needed for swap/resize geometry checks).
- Live (one probe): cycle moves focus; swap sends focused x 60→0; zoom→panes=1, unzoom→panes=2;
  resize_border shrinks focused width 60→36. Race-clean. ✓

## `cd38e33` — pane.last + pane.rename

- **last** (LastPane): `TileLayout` gained a `prev PaneID`, maintained by a new `setFocus()`
  through which `FocusPane`/`SplitFocused`/`CloseFocused` route; `ResizePane`'s transient focus
  swap **deliberately bypasses** it (a resize must not change last-focus). `FocusLast()` toggles
  current↔prev, skipping a `prev` that has since closed. `Session.FocusLastPane`; focus-only dispatch.
- **rename** (RenamePane): `PaneState` gained a durable `CustomName` (survives a daemon restart,
  unlike the cached terminal title). `Session.RenamePane`/`PaneCustomName`. The orch resolves an
  `effectiveTitle(pid)` = custom name else `rt.title`, used in **three** spots — the daemon
  `PaneTitle` handler, `broadcastPaneChrome`, and the rename dispatch — so a terminal title event
  never clobbers a custom name; clearing reverts to the terminal title. Front-end: ✎ rename button
  (browser-local `prompt`). wsprobe2: `last`/`rename:PANE:NAME` ops + `title:PANE:TEXT` (exact) assert.
- Unit: layout `FocusLast` (incl. ResizePane not polluting prev); Session `FocusLastPane` (incl.
  refusing a closed target) + `RenamePane` (set/clear/unknown).
- Live: `last` ping-pongs focus both ways; rename shows the custom name and survives a tab
  round-trip. **Decisive title test** — a shell OSC title set via `printf` with **octal `\073`**
  for `;` (dodging wsprobe2's naive `;` script split and the `type` op's ESC-less escaping) sets
  `TERMTTL`; the pane title **stays `MYPANE`** (no clobber); clearing then **reveals `TERMTTL`**
  (proving it was cached + suppressed). Race-clean. ✓

## Verification harness (unchanged from prior session, macOS)

- Sockets under `/tmp` (sun_path limit). Build: `PKG_CONFIG_PATH=~/projs/rust/herdr/vendor/
  libghostty-vt/zig-out/share/pkgconfig go build -tags ghostty -o $scratch/... ./cmd/{termhost,gateway2}`;
  wsprobe2 untagged. Run `termhost -socket … -persistent` then `gateway2 --auth none` on a spare port.
- Every commit: `go test ./...` green; `-race` on touched packages / live gateway2 clean; `go build`
  + `go vet` for untagged **and** `-tags ghostty`. No stray root binaries (built to `$scratch`).

## Notes / leftovers

- **§7 pane group is now complete.** Remaining deferred §7 commands are a **different shape**
  (not pure session mutations):
  - `read {pane,anchor,cursor,rect?}` → `cmd_result.data.text` — selection extraction over β
    `RequestSelection`; needs a request/response **round-trip through the daemon seam** (browserproto
    `ReadParams`/`ReadResult` wire types already exist; `daemon.go` notes "pane_selection/pane_text:
    nothing requests them yet").
  - `agent.focus {pane}` — focus the pane running an agent; agent-detection wiring.
  - `server.reload_config` / `server.stop` — daemon-lifecycle.
- wsprobe2 grew useful general assertions this session: `rect` (pane geometry) and `title` (exact
  pane title) — reusable for future layout/chrome checks.
- The actor loop still lives in gateway2 (package main); hoisting `orch` into `internal/app` behind
  a PaneBackend/Sink interface remains the WS4 (CLI/control-API) prerequisite. Pure `Session` is
  already positioned for it.
- Front-end still lacks keybindings for focus_direction/cycle/swap/last and draggable border handles
  for resize_border — those server commands are proven via wsprobe2 and driveable now; the browser
  triggers are separate UI tasks. Zoom (⤢) and rename (✎) got chrome buttons.
