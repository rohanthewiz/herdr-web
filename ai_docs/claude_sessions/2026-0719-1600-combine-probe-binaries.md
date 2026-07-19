# combine-probe-binaries

**Date:** 2026-0719-1600
**Session ID:** `dfd33925-c229-4466-a189-23af329d98b9`
**Project:** `~/projs/go/herdr-web`

---

## Scope

Consolidate the repo's three binaries down to two by merging the two headless
test tools into a single `probe` binary, then rewrite the README's Run section
around the fact that only the gateway needs building/running. Loaded the prior
3 session docs for context at session start.

---

## Work done

### 1. Merged `cmd/smoke` + `cmd/wsprobe` → `cmd/probe`

The gateway (production server) stays its own binary; the two diagnostics were
the natural pair to combine. New layout:

```
cmd/probe/main.go   subcommand dispatch (`wire` | `ws`), usage text,
                    defaultSocket() mirroring the gateway's default
cmd/probe/wire.go   formerly cmd/smoke — dials herdr-client.sock directly,
                    Hello handshake, frame summaries, clean Detach (read-only)
cmd/probe/ws.go     formerly cmd/wsprobe — stdlib RFC6455 client driving the
                    gateway's /ws end-to-end without a browser
```

Design details:

- Each subcommand owns its own `flag.NewFlagSet`, so the original flags carry
  over unchanged (`wire`: --socket/--cols/--rows/--frames; `ws`:
  --url/--cols/--rows/--msgs/--send-input) with no renames or merging.
- Only code rename: wsprobe's `readFrame` → `readWSFrame` (avoids clashing with
  the wire-protocol reader concept in the shared package).
- Improvement: `probe wire --socket` now **defaults** to
  `~/.config/herdr/herdr-client.sock` (matching the gateway) instead of being a
  required flag.
- Old `cmd/smoke/` and `cmd/wsprobe/` dirs deleted (content lives on in
  `cmd/probe` + git history).

### 2. Verified live

- `go build ./...`, `go vet ./...`, `gofmt -l` all clean.
- `probe wire --frames 1` → Welcome (proto 14) + real frame decoded from the
  live herdr session.
- The gateway from the prior session was no longer running; rebuilt and
  relaunched it (`/tmp/herdr-gateway` on `:8420`, left running).
- `probe ws --msgs 3` → 101 upgrade, mouse-capture flag, full 120x32 frame
  (~94 KB JSON) through the gateway.

### 3. README updates

- All `cmd/smoke` / `cmd/wsprobe` references updated to `cmd/probe wire` /
  `cmd/probe ws` (status table, Run, Layout).
- Fixed a stale example while at it: README showed `wsprobe --frames 1`, but
  that flag never existed on wsprobe — it's `--msgs`.
- Rewrote the **Run** section to lead with "only one binary needs to be built
  and run: the gateway" + no separate frontend build (`go:embed`), then the
  3 steps (herdr server running → `go run ./cmd/gateway --addr :8420` or a
  built binary → open `http://localhost:8420`). Probe moved under
  "Headless verification (optional)", explicitly marked as diagnostics never
  needed just to use herdr in the browser.

---

## Key facts (for future sessions)

- Binaries are now: `cmd/gateway` (the app) and `cmd/probe` (diagnostics with
  `wire`/`ws` subcommands).
- To run herdr-web locally: have a `herdr server` running, then
  `go run ./cmd/gateway --addr :8420`, open `http://localhost:8420`. Restart
  the gateway after editing `web/index.html` (it's embedded at compile time).
- Two `rangeint` style suggestions ("modernize with range over int") exist in
  `cmd/probe/ws.go` — carried over verbatim from wsprobe, harmless.

## State at end of session

- Gateway running on `:8420` as `/tmp/herdr-gateway`.
- All changes committed and pushed with this session doc (see /sess-wrap).

## Next step (unchanged from prior sessions)

Phase B spike: `brew install zig` (0.16) + `go.mitchellh.com/libghostty` to
move PTY + VT emulation into Go — first real test of Zig vs macOS SDK 26.5.
