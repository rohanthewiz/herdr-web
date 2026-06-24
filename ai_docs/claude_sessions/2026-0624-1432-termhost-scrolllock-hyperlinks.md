# herdr-web (Go) — Phase B: scroll-lock + native-TUI hyperlink resolver

**Date:** 2026-0624-1432
**Repo:** `~/projs/go/herdr-web` (Go terminal backend) · paired with `~/projs/rust/herdr` (Rust orchestrator)
**Branch (Go):** `roh/phase-b` · **Branch (Rust):** `roh/phase-b-termhost-client`

> Continues termhost degradation cleanup. Same session-day as selection
> passthrough (`2026-0624-1408-termhost-selection-passthrough.md`). Knocks out two
> follow-ups that were flagged there: **scroll-lock/pinning** and the
> **native-TUI hyperlink click resolver**.

---

## 1. Scroll-lock / pinning — Go commit `b93aeac` (Go-only)

**Problem:** the emulator snapped the viewport to the live bottom on *every* write
while scrolled up (`ghostty.go Write` called `ScrollViewportBottom()` when its
tracked `scrollOffset != 0`). So any streaming output yanked a user out of
scrollback history.

**Fix:** drop the snap and let **libghostty pin natively**. On the active area new
output follows the bottom; scrolled into history the viewport stays pinned to that
content and its offset-from-bottom grows as output accumulates below. This is
exactly what the Rust in-process path does — its write path (`write_pty_bytes_*`)
does nothing special; libghostty handles follow-vs-pin via the viewport pin.

**Also:** removed the self-tracked `scrollOffset`. The prior session believed
"libghostty exposes no current-offset query" and tracked it by hand — but the
binding *does* expose `Terminal.Scrollbar()` (the same call herdr's Rust
`scroll_metrics` uses). Now:
- `ScrollMetrics()` reads `Scrollbar{Total, Offset, Len}` →
  `offset_from_bottom = Total-(Offset+Len)`, `max = Total-Len`, `viewport = Len`
  (mirrors herdr exactly; `min()`-guarded to avoid uint underflow).
- `Scroll(delta)` just delegates to `ScrollViewportDelta(delta)` (negative = up;
  libghostty clamps, so a large positive delta is still a reliable scroll-to-bottom).
- `Write(p)` is now a plain `term.Write(p)`.

**No Rust change needed.** The Rust scroll path (`PaneRuntime::scroll_up/down/reset/
set_offset/scroll_metrics`) only *forwards* `scroll_viewport` over the seam — it
can't write to the Go emulator, so pinning is entirely Go-side. The Rust side just
keeps receiving frames (Go marks dirty on each read) that now render the pinned
viewport with a growing offset, and the scrollbar tracks it.

**Test:** `internal/terminal/scrollback_test.go TestScrollback` rewritten — after
scrolling to the top (L1) and emitting a line, asserts the viewport stayed pinned
(L1 still at row 0, offset grew by exactly the one pushed line) instead of the old
snap-to-bottom; then scroll-to-bottom still snaps to 0.

## 2. Native-TUI hyperlink click resolver — Rust commit herdr `1e9c7e5` (Rust-only)

**Problem:** `PaneRuntime::visible_hyperlinks` read the unfed local emulator for
termhost panes, so the native-TUI click-to-open resolver (`url_at_pane_cell`) never
saw OSC 8 links — even though the Go `pane_frame` already carries the per-cell link
index + the URI table (shipped in the OSC 8 passthrough work). Browser-side
clickable links already worked (frame-data render path); this was the *native TUI
mouse resolver* gap noted as a follow-up.

**Fix (mirrors the other termhost branches):**
- **`client.rs`**: `PaneGrid::visible_hyperlinks(origin_x, origin_y, width, height)`
  walks the accumulated grid window and emits `((screen_x, screen_y), cell_symbol,
  uri)` per linked cell — the same tuple shape the in-process
  `ghostty_visible_hyperlinks` produces. `TermhostPane` delegates under the grid
  lock. Coords are primitives (keeps ratatui out of the client). Empty URI table →
  empty result.
- **`pane.rs`**: `PaneRuntime::visible_hyperlinks` branches to the termhost backend.
- The `TerminalRuntime` wrapper already forwards `visible_hyperlinks`, so
  `url_at_pane_cell` and the web full-render link collector pick it up with no
  call-site change.
- **Tests:** two `PaneGrid` unit tests — cell→screen/URI mapping (links offset by the
  pane origin, deduped URI shared) and window-clipping / empty-table.

## Key facts for future me

- **`libghostty.Terminal.Scrollbar()` exists** (`{Total, Offset, Len}`) — use it for
  live scroll position; no need to self-track offset. `ScrollViewportDelta` clamps.
- **Scroll-lock is libghostty-native** once you stop snapping on write. Follow at the
  bottom, pin in history — matches the Rust in-process path.
- **Hyperlink resolution for termhost reads the frame grid, not the emulator.** The
  `pane_frame` carries `hyperlinks` (URI table) + per-cell `hyperlink` index; the
  resolver just maps grid (col,row)+origin → screen coords and looks up the table.
- Build/run env unchanged (Go ghostty `PKG_CONFIG_PATH=…libghostty-vt…/pkgconfig`;
  Rust `ZIG=…/.tools/zig-wrapped`).

## Verification (all green)

- Go: default `go build ./...` + `-tags ghostty` build; `go test -tags ghostty
  ./internal/...` (incl. the rewritten `TestScrollback` + `TestHostScrollbackReportsMetrics`);
  gofmt/vet clean.
- Rust: `cargo build` (feature off) clean; `--features termhost` build + clippy clean
  (in changed files). `cargo test --bin herdr` = **1892 passed**; `--features
  termhost` = **1911 passed** (+ the 2 new `PaneGrid` hyperlink tests on top of the
  selection suite).

## Commits

```
Go   (roh/phase-b):                 b93aeac feat: scroll-lock for termhost panes — pin the viewport during output
Rust (roh/phase-b-termhost-client): 1e9c7e5 feat: native-TUI hyperlink click resolver for termhost panes
```

## Remaining termhost degradations

- **Kitty graphics** — still not carried by the seam (`Frame.graphics` is reserved/empty).
- **Daemon lifecycle** — have the Rust server spawn/supervise `cmd/termhost` instead
  of the manual `HERDR_TERMHOST_SOCKET` env + hand launch.
- Eventually: make termhost the default and retire the Rust in-process PTY/detect path.
- A live browser/TUI e2e pass (scroll-lock during streaming output; click an OSC 8
  link in a termhost pane) is worth a manual run — each side is unit/integration tested.
</content>
