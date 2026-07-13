# WS2 §7 read — pane selection extraction (first daemon round-trip command)

**Session id:** `a5ffcca5-247b-4411-a0ed-ec6797bfbcd5`
**Date:** 2026-0713-1359 · **Branch:** `roh/phase-b` (herdr-web)
**Continues:** `2026-0713-1258-ws2-deferred-pane-commands.md`. That session finished the entire
§7 **pane** command group and flagged the remaining deferred commands as "a different shape"
— `read` first, because it "needs a request/response round-trip through the daemon seam."

> Implemented `read` (`ai_docs/phase-c-ws9-protocol.md` §7) — selection-text extraction. It is
> the **first §7 command that round-trips through the daemon**: every prior command was a
> fire-and-forget model mutation, but `read` sends a β `RequestSelection`, returns without
> replying, and completes the browser's `cmd_result` only when the daemon's `pane_selection`
> event comes back (or a timeout / disconnect fails it). One commit, race-clean and green,
> with unit tests + live end-to-end verification (linear / rect / multi-line + error path).

---

## The one hard part: an async round-trip on a lock-free actor loop

Every §7 command so far did `session mutation → applyModel/broadcast → reply(true)` synchronously
on the orchestrator loop. `read` can't: the orchestrator holds no emulator (its local one is
unfed for termhost panes), so only the **daemon** can resolve selection coordinates to text. So
the loop must **not block** waiting — it registers the request and moves on; the reply lands later.

**Correlation.** The β reply `pane_selection{pane_id, text}` carries **no command id**. But there
is exactly one daemon connection, the host emits **one `pane_selection` per request, in order**,
and the reply names its pane. So correlation is **per-pane FIFO**: the oldest outstanding read for
pane P matches the next `pane_selection` for P. This is exact for the normal path and needs no id.

**Three ways a read resolves** (all loop-goroutine only, keyed off `pendingReads map[uint32][]*pendingRead`):
- `resolveRead(pane, text)` — a `pane_selection` arrived → pop the pane's FIFO head, send `cmd_result{ok, data:{text}}`.
- `timeoutRead(pane, pr)` — a `time.AfterFunc(readTimeout=5s)` fired → remove `pr` **by identity**
  (a late reply may have shifted the queue), send `cmd_result{ok:false,"read timed out"}`. Safety
  net so a browser cmd never hangs if the host errors (host errors go out on the separate
  `MsgError` channel, which does **not** pop the read queue).
- `flushPendingReads(msg)` — the daemon connection dropped (in `daemon.run`'s redial path) → fail
  every in-flight read; no `pane_selection` will ever come.

`replyRead` skips a read with `id==""` (no result channel). `handleCmd` validates the pane exists
and the daemon is connected before calling `startRead`, so the common failures reply synchronously.

## Files

- `cmd/gateway2/gateway.go` — `pendingRead` type, `orch.pendingReads` field, `readTimeout` const,
  and the round-trip helpers: `startRead` / `resolveRead` / `timeoutRead` / `flushPendingReads` /
  `dropPending` / `replyRead`.
- `cmd/gateway2/commands.go` — the `CmdRead` dispatch case (validate → `o.startRead`; returns
  without replying — the async resolve sends the `cmd_result`).
- `cmd/gateway2/daemon.go` — new `MsgPaneSelection` case → `o.resolveRead`; `flushPendingReads`
  added to the disconnect closure. (`pane_text` is still the only unrequested β event.)
- `cmd/gateway2/read_test.go` (**new**, `//go:build ghostty`) — 5 unit tests driving the resolve
  logic directly against a bare `orch` + fake `client` (no daemon needed): FIFO order, per-pane
  isolation, timeout-then-continue (+ late-reply no-op), flush, id-less no-op.
- `cmd/wsprobe2/main.go` — new `read` / `readeq` ops to drive + assert the round-trip. `read`'s
  row may be `@TEXT` = the viewport row where TEXT first appears; with no scrollback yet that
  equals the screen-buffer (absolute) row the daemon selects on, so a test anchors to content it
  just typed without hardcoding a prompt-dependent row.

**Pre-built machinery reused** (nothing new needed below the seam): `browserproto.ReadParams`/
`ReadResult` wire types; β `RequestSelection`→`PaneSelection` round-trip (host-side
`requestSelection` → `emu.FormatSelection`, already covered by `orchestration.TestHostReportsPaneSelection`).
Wire `Pane` is the raw pane id (like every other command), so it maps straight to the daemon id.

## Verification harness (macOS, unchanged from prior sessions)

- Sockets under `/tmp` (sun_path limit). Build:
  `PKG_CONFIG_PATH=~/projs/rust/herdr/vendor/libghostty-vt/zig-out/share/pkgconfig go build -tags ghostty -o $scratch/... ./cmd/{termhost,gateway2}`; wsprobe2 untagged.
  Run `termhost -socket … -persistent`, then `gateway2 --addr :PORT --socket … --auth none`.
- `go test ./...` green; `go test -tags ghostty ./cmd/gateway2` green; **`-race` clean** on both the
  unit tests and a live race-built gateway2 driving the full script (the read round-trip crosses the
  daemon-pump, loop, `AfterFunc` timer, and writer goroutines — the real race surface). `go vet` +
  `go build` clean for untagged **and** `-tags ghostty`. No stray root binaries (all to `$scratch`).

### Live results (deterministic, real ghostty extraction)

- **Linear** whole-line: `type echo $((123*1000+456))` → `read pane=1 @123456,0 → @123456,119` → `"123456"`.
- **Rect** column-window: `read @123456,1 → @123456,4 rect` → `"2345"` (cols 1–4; end col inclusive in rect mode).
- **Multi-line** linear: `seq $((50*10)) $((50*10+2))` → `read @500,0 → @502,3` → `"500\n501\n502"` (newline-joined, per-line trailing trimmed).
- **Unknown pane**: `read:999:…` → `cmd_result{ok:false,"unknown pane 999"}`, no hang.
- wsprobe2 note: `type:` turns `\n` into Enter and can't send a literal `\n`, so multi-line output
  is produced with `seq` (one command, consecutive lines) — and computed args (`$((50*10))`) keep
  the target tokens out of the command echo so `@TEXT` locks onto the output line, not the echo.

## Notes / leftovers

- **Browser-side selection UI is a separate front-end task.** `read` is proven and driveable now
  (wsprobe2), but the mouse-drag that would *originate* the anchor/cursor coordinates in the browser
  isn't wired — same category as last session's deferred keybindings and draggable border handles.
- **Remaining deferred §7 commands:** `agent.focus {pane}` (focus the agent's pane; needs
  agent-detection wiring) and `server.reload_config` / `server.stop` (daemon-lifecycle). Both are a
  different shape again from `read`.
- **`read` set the round-trip pattern** (`pendingReads` + FIFO correlation + timeout/flush) that any
  future request/response command over the daemon seam can follow (e.g. a `pane_text`-backed command
  — `pane_text` is the last β event still unrequested).
- The actor loop still lives in gateway2 (package main); hoisting `orch` behind a PaneBackend/Sink
  interface into `internal/app` remains the WS4 (CLI/control-API) prerequisite. `read`'s helpers are
  pure loop-goroutine methods that would move cleanly.
