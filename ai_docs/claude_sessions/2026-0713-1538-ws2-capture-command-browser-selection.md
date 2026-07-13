# WS2 capture (pane_text) command + browser drag-selection UI (read)

**Session id:** `c017d5e8-f786-4291-92be-1d4990e402fb`
**Date:** 2026-0713-1538 · **Branch:** `roh/phase-b` (herdr-web)
**Continues:** `2026-0713-1446-ws2-agent-focus-server-lifecycle.md`, which completed the §7 command
table and left three priorities. This session did **#1 (pane_text round-trip)** and **#3
(browser-side selection UI)**; **#2 (hoist `orch` behind a `PaneBackend`/`Sink`)** still deferred.

> Added the `capture` command (β `RequestText`→`pane_text`) by first generalizing the read
> round-trip machinery into a shared pending-request layer, then built the browser drag-to-select
> UI that originates `read`. Validating the browser's viewport→absolute coordinate math headlessly
> (new `readvp` probe op) surfaced and fixed a real stale-scroll bug. Race-clean, green, live-verified.

---

## Item 1 — `capture` command (pane_text round-trip)

### Generalized the pending machinery (the prereq from #1's note)

`read` was the only daemon round-trip command; its plumbing lived in `gateway.go` as
`pendingReads map[uint32][]*pendingRead` + `resolveRead`/`timeoutRead`/`flushPendingReads`/
`dropPending`/`replyRead`. Generalized to a **shared layer keyed by `reqKey{pane, kind}`**:

- `reqKind` = `reqSelection` (read → `pane_selection`) | `reqText` (capture → `pane_text`), with
  `label()` for error text. `pending` replaces `pendingRead`; `pendingReqs map[reqKey][]*pending`.
- `registerPending` / `resolvePending(key, data any)` / `timeoutPending` / `flushPending` /
  `dropPending(key,i)` / `replyPending(pr, data, errMsg)`. Command-specific starters stay thin:
  `startRead` sends `RequestSelection`, `startCapture` sends `RequestText`; both just
  `registerPending` then send.

**Why keyed by (pane, kind), not just pane:** a pane can have a read *and* a capture in flight at
once, and neither reply carries a command id — the daemon replies one `pane_selection` per read and
one `pane_text` per capture over its single ordered connection. The **message type** picks the
queue; per-(pane,kind) FIFO does the correlation. New test `TestResolvePendingPerKind` locks this.

### The command

- `browserproto/cmd.go`: `CmdCapture = "capture"`, `CaptureParams{Pane, Scope, Lines, Ansi, Unwrap}`
  (β `RequestText`: scope 0=visible viewport, 1=recent last-N of scrollback+active, 0 lines=whole
  buffer), `CaptureResult{Text}`. Capture takes **no coordinates** — whole rows, for "copy
  scrollback" / feeding an agent the terminal contents.
- `commands.go`: `case CmdCapture` — validates id/pane/daemon like `read`, then `startCapture`.
- `daemon.go`: `case MsgPaneText` → `resolvePending(reqKey{id, reqText}, CaptureResult{Text})`;
  `pane_selection` now routes the same way with `ReadResult`. Deleted the stale
  "pane_text: nothing requests it yet" comment. Disconnect flush renamed `flushPending`.

### Files

- `cmd/gateway2/gateway.go` — generic pending layer + `reqKind`/`reqKey`/`pending` (was the
  read-only block); `pendingReqs` field; `reqTimeout` (was `readTimeout`).
- `cmd/gateway2/commands.go`, `cmd/gateway2/daemon.go` — capture dispatch + `pane_text` handler.
- `cmd/gateway2/read_test.go` → **`pending_test.go`** (`git mv`), rewritten for the generic API;
  helpers `newPendingHarness`/`resultText`/`selKey`/`txtKey`; tests cover FIFO, per-pane, **per-kind
  independence**, timeout (names the command in the error), flush (both kinds), id-less.
  `commands_test.go` — `newReadHarness`→`newPendingHarness`.
- `internal/browserproto/cmd.go` — `CmdCapture` + `CaptureParams`/`CaptureResult`.
- `cmd/wsprobe2/main.go` — `capture:PANE[:visible|recent][:LINES][:ansi][:unwrap]` +
  `captureeq`/`capturehas` ops; `lastCapture` field.

## Item 3 — Browser drag-selection UI (`web/index.html`)

On a **non-mouse-capturing** pane, left-drag originates a browser-local selection (§7 `read`);
**Alt** = rectangular block. mousedown starts `p.sel = {anchor, cursor, rect, moved}`; mousemove
extends it (one update per cell) and draws a translucent wash (`drawSelection`: reading-order spans
for linear, bounding box for rect, endpoints inclusive to match the server). mouseup: a real drag →
`read` + copy to clipboard; a plain click → follow a hyperlink under it (moved from mousedown).
Scrolling clears the wash. `read` results route through a new `sendCmdAwait` (id→callback map).

### The coordinate crux (and a real bug it surfaced)

`read` wants **absolute screen-buffer rows** (from top of scrollback); the browser has viewport
cells. Derived `absRow = max − off + y` from the frame's `scroll` (β `ScrollInfo`:
`off`=lines up from live bottom, `max`=history above viewport when pinned = `Total − ViewportRows`,
so viewport-top abs row = `max − off`). Confirmed the daemon is absolute, not viewport-relative:
reading viewport row 9 (showing "260") after scrolling returned "28", i.e. it read absolute row 9.

To validate the formula **headlessly**, added a `readvp:PANE:VROW,VCOL:VROW2,VCOL2[:rect]` probe op
that maps viewport→absolute exactly as the browser does (`parseViewportPoint`), plus a shared
`issueRead` helper. It caught a genuine defect: **`internal/orchestration/protocol.go:552`
populates `f.Scroll` only when `max>0 || off>0`** — so when `clear` wipes the scrollback the frame
*omits* scroll, and a client caching the last value keeps a **stale non-zero `max`**, computing an
out-of-range row (`ghostty: invalid value`, read times out). Every frame otherwise carries current
scroll, so "absent" reliably means zero. **Fix:** reset scroll to zero on any frame/diff that omits
it (browser `pane_diff` handler + wsprobe2 diff handler), matching the full-frame branch.

### Files

- `cmd/gateway2/web/index.html` — `p.scroll`/`p.sel` pane fields; `SEL_FILL`; store `scroll` on
  frame/diff (**reset when absent**); `drawSelection`; `absPoint`/`finishSelection`; drag handling in
  `attachMouse`; `sendCmdAwait` + `cmd_result` callback routing; wheel clears the wash.
- `cmd/wsprobe2/main.go` — `readvp` op, `parseViewportPoint`, shared `issueRead`; `paneGrid` scroll
  fields (`HasScroll`/`Off`/`Max`, reset on absent).

## Verification (macOS, harness unchanged from prior sessions)

- Build: `PKG_CONFIG_PATH=~/projs/rust/herdr/vendor/libghostty-vt/zig-out/share/pkgconfig go build
  -tags ghostty -o $scratch/{termhost,gateway2} ...`; wsprobe2 untagged. Run `termhost -socket …
  -persistent`, then `gateway2 --addr :PORT --socket … --auth none`. Sockets under `/tmp`.
- `go test ./...` green; `go test -tags ghostty ./cmd/gateway2` green; **`-race` clean** on unit
  tests and a live race-built gateway2 (capture + readvp cross daemon-pump → loop → `AfterFunc` →
  writer). `go vet` + build clean, untagged **and** `-tags ghostty`. All binaries to `$scratch`; no
  stray root binaries. Browser JS `node --check` clean (mouse wiring/overlay/clipboard themselves
  need a real browser — the inherently manual part of #3).

### Live results (deterministic, real ghostty)

- **capture visible**: whole viewport buffer incl. the typed command. **recent:N**: bottom N screen
  rows (incl. the live prompt line — faithful, not a bug; `recent:20` over a top-anchored screen is
  blank after trailing-trim). **whole buffer** (lines≥total): full content. **unknown pane** →
  `cmd_result{ok:false,"unknown pane 999"}`, no hang.
- **read regression** (post-refactor): linear `@123456` → "123456"; rect cols 1–4 → "2345".
- **readvp scroll lifecycle** (one connection): `seq 300`, scroll up 20, `readvp @265` → "265";
  scroll to bottom, `clear`, `echo 507`, `readvp @507` → "507" (**proves the stale-scroll fix** —
  timed out before); rect `readvp @507` cols 1–2 → "07".

## Notes / leftovers (updated priorities)

- **Hoist `orch` behind a `PaneBackend`/`Sink` into `internal/app`** — still the top remaining WS2
  item (WS4 CLI/control-API prereq; same command table serves both protocols). `RevealPane` and the
  now-generalized pending helpers move cleanly; the actor loop stays in `main`.
- **Browser selection polish** (front-end, non-headless): the wash currently persists until the next
  mousedown/scroll — could clear on new output; no visual affordance during drag beyond the wash; no
  copy-mode keyboard motions. `capture` has no browser UI yet (a "copy scrollback" chrome button
  would be a natural home).
- **Wire-level `capture`/`readvp` acceptance** could graduate from wsprobe2 into a tagged
  end-to-end test if a daemon fixture is stood up.
