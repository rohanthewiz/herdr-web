# herdr-web (Go) — Phase B: selection passthrough on the termhost seam

**Date:** 2026-0624-1408
**Session ID:** loaded from `2026-0622-0008-termhost-osc-detection.md` + `2026-0621-0855-e2e-herdr-termhost-test.md`
**Repo:** `~/projs/go/herdr-web` (Go terminal backend) · paired with `~/projs/rust/herdr` (Rust orchestrator)
**Branch (Go):** `roh/phase-b` · **Branch (Rust):** `roh/phase-b-termhost-client`

> Continues the Go↔Rust seam. The immediately prior work shipped OSC 7/52/9/0/2
> passthrough, agent detection (Stages A–C.2), OSC 8 hyperlinks, and scrollback
> (`2026-0622-0008-termhost-osc-detection.md`). This session implements the
> **selection** request/response that was flagged as the next step there.

---

## Goal

Termhost panes keep an **unfed local emulator** (slice-1 from the e2e session):
display/IO/cursor come from the Go backend, but the ~40 `PaneRuntime` methods that
read the local emulator return empty. Selection extraction was one of those —
drag-select copy on a termhost pane produced nothing. Close that gap with the
decided model: **Go request/response**. Rust owns selection state + mouse/key
handling; Go owns the fed emulator that can resolve coordinates to text.

## What shipped

### Go side — commit `67f84f1` (selection passthrough)
- **`protocol.go`**: `RequestSelection { pane_id, anchor, cursor, rectangle }`
  command + `SelectionPoint { row, col }`, and `PaneSelection { pane_id, text }`
  reply event. `SelectionPoint` is **screen-buffer (absolute) coordinates** (row
  from the top of scrollback, stable across scroll) — mirrors herdr's `Selection`
  endpoints. New message types `MsgRequestSelection` / `MsgPaneSelection`.
- **`terminal.go`**: `Emulator.FormatSelection(anchor, cursor, rectangle)` + the
  pure `SelectionEndpoint { Row, Col }` type.
- **`ghostty.go`**: the impl **mirrors herdr's `read_text_screen`** exactly: order
  the two endpoints top-left → bottom-right (same rule as Rust `Selection::ordered`),
  resolve each via a `PointTagScreen` `GridRef`, build a `libghostty.Selection`,
  then `SelectionFormatString(WithSelectionFormat(Plain), WithSelectionUnwrap(true),
  WithSelectionTrim(true))`. The two grid refs are **borrowed views** of terminal
  internals, so they're built and consumed back-to-back with no intervening
  mutation — the Host holds `emuMu` across the whole call.
- **`host.go`**: dispatch `request_selection` → `requestSelection` extracts under
  `emuMu` and **always** replies with `pane_selection` (definite response; `""` =
  no selectable content).
- Tests: codec round-trip for both messages + `-tags ghostty` Host integration
  `TestHostReportsPaneSelection` (child prints `HELLO WORLD`, request screen cols
  0..4 inclusive → `pane_selection` text `"HELLO"`).

### Rust side — herdr `ef489d3` (consumer) then `e3ae3d2` (synchronous rework)
**Proto/wire (`ef489d3`, kept):** `Command::RequestSelection {…}` +
`SelectionPoint { row, col }` (`rectangle` `skip_serializing_if` when false,
matching Go's `omitempty`) and `Event::PaneSelection { pane_id, text }`. 5 proto
tests.

**Design (final, `e3ae3d2`): one synchronous mechanism for every selection path.**
The first cut (`ef489d3`) made drag-copy async — the reply rode the OSC 52 clipboard
signal path (`PaneSignal::Selection` → `AppEvent::ClipboardWrite`), no UI-thread
block. But double-click word copy (`copy_word_at_pane_cell`) and URL-at-cell
(`url_at_pane_cell`) read a row's text **inline** to compute word/URL bounds — they
need the value back, not fire-and-forget. So drag-copy's async path was reworked
into a single **blocking** request/response that serves all paths through the
existing `extract_selection` call sites (no per-site branching):
- **`client.rs`**: `PaneState.pending_selection` one-shot slot (`mpsc::Sender`); the
  reader thread routes a `pane_selection` reply to the waiter (take-on-deliver, so a
  late/duplicate reply is dropped). `TermhostPane::extract_selection_blocking(...)`
  registers the waiter, sends the command, and blocks on `recv_timeout` (**1s** cap
  guards a wedged daemon). `PaneSignal::Selection` removed.
- **`pane.rs`**: `PaneRuntime::extract_selection` branches — for a termhost pane it
  calls `extract_selection_blocking(sel.ordered_cells())`; in-process panes read the
  local emulator as before. The `PaneSignal::Selection` sink arm is gone.
- **`terminal/runtime.rs`**: no new delegate needed (the `TerminalRuntime` wrapper
  already forwards `extract_selection`).
- **`app/actions.rs`**: `copy_selection` is the plain synchronous extract+copy —
  unchanged shape; it now just works for termhost panes via the branch above.
- Test: `client.rs` unit test drives the full round-trip over a **real Unix socket**
  with a fake daemon thread (Hello→Welcome, CreatePane, RequestSelection→PaneSelection
  `"HELLO"`), asserting `extract_selection_blocking` returns `Some("HELLO")`.

## Key facts for future me

- **Selection coordinates are screen-buffer/absolute** (row `u32`, col `u16`) on
  both sides. Go resolves them via `PointTagScreen` `GridRef`. The Go side orders
  the endpoints, so the Rust side may send anchor/cursor in any order.
- **`rectangle` is always false today** — herdr's `Selection` has no rectangle
  field. The seam carries the flag for future block selection.
- **Selection extraction is request/response, SYNCHRONOUS (blocking) on the Rust
  side.** `extract_selection` for a termhost pane sends `request_selection` and
  blocks (≤1s) on the reader thread handing back the `pane_selection` reply via a
  per-pane one-shot. Replies are FIFO and requests are issued one at a time from the
  UI thread (which then blocks), so a single pending slot suffices. (The first cut
  was async-only and served just drag-copy; superseded by `e3ae3d2`.)
- **All selection paths now work for termhost panes:** drag-select copy
  (`copy_selection`), double-click word copy (`copy_word_at_pane_cell`), and
  URL-at-cell (`url_at_pane_cell`) — all flow through `extract_selection`, so the
  one branch covers them. The blocking round-trip is sub-millisecond over the local
  socket (the daemon formats under its per-pane `emuMu`).
- **Build/run env unchanged:** Go ghostty `PKG_CONFIG_PATH=~/projs/rust/herdr/vendor/libghostty-vt/zig-out/share/pkgconfig`;
  Rust `ZIG=~/projs/go/herdr-web/.tools/zig-wrapped`.
- **Seam now — commands (Rust→Go):** `hello`, `create_pane`, `input`, `resize`,
  `close_pane`, `scroll_viewport`, **`request_selection`**.
- **Seam now — events (Go→Rust):** `welcome`, `pane_frame`, `pane_cwd`,
  `pane_agent`, `pane_clipboard`, `pane_title`, **`pane_selection`**, `pane_exited`,
  `error`.

## Verification (all green)

- Go: default `go build ./...` + `-tags ghostty` build; `go test ./internal/...`
  (pure) + `-tags ghostty ./internal/...` (incl. `TestHostReportsPaneSelection`);
  gofmt/vet clean both modes.
- Rust: `cargo build` (feature off) clean (the lone `TerminalTitleReported`
  never-constructed warning is pre-existing — the variant is only built in the
  termhost sink); `--features termhost` build + clippy clean. `cargo test --bin
  herdr` = **1892 passed**; `--features termhost` = **1909 passed** (+17: termhost
  proto tests + the client blocking-round-trip test).

## Commits

```
Go   (roh/phase-b):                 67f84f1 feat: selection passthrough on the termhost seam (Go side)
                                    41e923b docs: session notes (this file)
Rust (roh/phase-b-termhost-client): ef489d3 feat: consume selection passthrough on the termhost seam (Rust side)
                                    e3ae3d2 feat: synchronous termhost selection — drag, double-click, URL
```

## Next steps

- **Selection is fully wired** (drag + double-click + URL, all synchronous). A live
  browser e2e (mouse-drag / double-click → clipboard) is still worth a manual run;
  each side is unit/integration tested independently.
- Remaining termhost degradations: kitty graphics; scroll-lock/pinning (output
  snaps to bottom); native-TUI hyperlink click resolver.
- Daemon lifecycle: have the Rust server spawn/supervise `cmd/termhost`.
- Eventually: make termhost the default and retire the Rust in-process PTY/detect path.
</content>
</invoke>
