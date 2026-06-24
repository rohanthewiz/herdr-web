# herdr-web (Go) — Phase B: text/scrollback extraction over the seam (retire-Rust #2)

**Date:** 2026-0624-1640
**Repo:** `~/projs/go/herdr-web` (Go terminal backend) · paired with `~/projs/rust/herdr` (Rust orchestrator)
**Branch (Go):** `roh/phase-b` · **Branch (Rust):** `roh/phase-b-termhost-client`

> Retire-the-Rust-backend gap #2 (after input modes, `…1616…`). Termhost panes'
> text reads (`pane.read`, session save, agent resume, search) read the *unfed*
> local emulator → empty. Now they round-trip to the Go backend, which owns the
> real buffer.

---

## What shipped

Request/response, same shape as selection — the consumers (`visible_text`,
`visible_ansi`, `recent_text`, `recent_ansi`, `recent_unwrapped_text`,
`recent_unwrapped_ansi`, `detection_text`, and `snapshot_history` =
`recent_unwrapped_ansi(MAX)`) all reduce to "format a screen-coordinate range,
plain or VT, optionally unwrapped, trailing-trimmed" — exactly the `FormatSelection`
machinery from the selection feature.

### Go — commit `7a79667`
- **`terminal`**: `Emulator.ExtractText(scope, lines, ansi, unwrap)` + `TextScope`
  (`TextVisible`/`TextRecent`). Refactored `FormatSelection`'s core into shared
  `formatScreenRange`/`screenRangeRefs`. **visible** = the viewport, derived from the
  live `Scrollbar()` (`Offset`..`Offset+Len`); **recent** = the last N rows of the
  buffer via `TotalRows()` (`lines==0` ⇒ whole buffer — the snapshot_history case).
  Mirrors herdr's `read_text_screen`/`read_ansi_screen`.
- **`protocol`**: `RequestText { pane_id, scope, lines, ansi, unwrap }` command +
  `PaneText { pane_id, text }` reply.
- **`host`**: `request_text` → `ExtractText` under `emuMu` → `pane_text` (always replies).
- Tests: codec round-trip + Host integration (3-row pane prints 12 lines into
  scrollback; whole-buffer request returns row1..row12 — proves it reads history,
  not just the viewport).

### Rust — commit herdr `aa9ddf7`
- **`proto`**: `Command::RequestText` + `TEXT_SCOPE_*` + `Event::PaneText`.
- **`client`**: `pending_text` one-shot + `TermhostPane::extract_text_blocking`
  (blocking round-trip, 1s timeout); reader routes `pane_text` to the waiter. The
  reply-timeout const generalized `SELECTION_REPLY_TIMEOUT` → `SEAM_REPLY_TIMEOUT`.
- **`pane.rs`**: the 7 text methods branch to the Go backend for termhost panes via a
  `termhost_text` helper. `usize::MAX` (snapshot_history) saturates to `u32::MAX`,
  which lands above the buffer size and the Go side reads as "whole buffer".
- Tests: proto + a client blocking round-trip over a real socket.

## Notable: the e2e degradation assertion flipped

The existing render e2e *proved* a pane was termhost-backed by asserting `pane.read`
returned **empty** (local emulator unfed). That's now **inverted** — `pane.read`
returns the program's output via the seam. Updated the assertion to
`read_text.contains(marker)`: a full `program output → Go buffer → request_text →
pane.read` proof. All four e2e tests pass against the real daemon (~6.8s).

## Key facts for future me

- **Text reads for termhost are a blocking seam round-trip** (`extract_text_blocking`),
  like selection. Same single-pending-slot/FIFO reasoning; separate `pending_text`
  slot from `pending_selection`.
- **`lines==0` / `u32::MAX` both mean "whole buffer"** on the Go side
  (`lines>0 && lines<total` gate). snapshot_history passes `usize::MAX`.
- **`visible` = viewport from the live scrollbar**, `recent` = bottom-N of the full
  buffer (`TotalRows`). One `ExtractText` covers all 6 read variants + detection_text
  (mapped to visible for termhost, since Go owns detection).
- **`snapshot_history` now works for termhost** → session save + agent resume capture
  history. But *restoring* it into a termhost pane is gap #3 (persistence) — the Go
  daemon doesn't yet seed history, and it dies with herdr.

## Verification (all green)

- Go: default + `-tags ghostty` build; `go test -tags ghostty ./internal/...` (incl.
  the new Host text test); vet/gofmt clean.
- Rust: feature-off + feature-on + clippy clean. `cargo test --bin herdr` = **1892**;
  `--features termhost` = **1916** (+3: request_text serialize, pane_text decode,
  extract_text round-trip). Full e2e: **4/4** against the real daemon.

## Commits

```
Go   (roh/phase-b):                 7a79667 feat: text/scrollback extraction on the termhost seam (Go side)
Rust (roh/phase-b-termhost-client): aa9ddf7 feat: serve termhost text/scrollback reads from the Go backend
```

## Retire-the-Rust-backend roadmap

1. ~~Input-mode parity~~ ✅ (`…1616…`)
2. ~~Text/scrollback extraction parity~~ ✅ this session
3. **Session persistence + detach/reattach + handoff** — NEXT. Termhost panes don't
   survive server restart (daemon dies with herdr via `--exit-on-disconnect`) or
   handoff (fd ops are no-ops). `snapshot_history` now *captures* history, but
   restoring it needs the Go daemon to seed history on create_pane, and likely a
   **persistent daemon that outlives herdr restarts**. Biggest single piece.
4. **Flip default + delete the in-process path** — config + daemon discovery, then
   remove the unfed local `PaneTerminal` + `Actor` PTY path + `src/pane/terminal.rs`.
   After #1/#2 the local emulator only serves modes (mirrored) and is otherwise dead
   weight; herdr then no longer links ghostty.
5. Build/dist (bundle daemon, CI cache) + smaller items (platform parity,
   `termhost_dirty_patch` changed-rows, restart-on-crash).

Kitty graphics remains back-burnered (experimental/off-by-default).
</content>
