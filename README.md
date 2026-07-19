# herdr-web

A Go + web presentation tier for [herdr](https://herdr.dev). This is **Phase A** of
an incremental migration of herdr off Rust onto a Go stack (rweb / Element / serr /
logger), presented through the browser.

Phase A attaches to an **unmodified, already-running `herdr server`** as a wire-protocol
client, receives herdr's fully-rendered semantic frames, and streams them to a browser
canvas. No Rust build and no Zig toolchain are involved — the installed `herdr` binary
does all terminal emulation and rendering; this gateway is a thin, language-agnostic
client + web renderer.

## Status

| Piece | State |
|-------|-------|
| bincode v2 `standard` codec (`internal/wire`) | ✅ hand-written, validated against live server |
| Wire messages: Hello / Input / Resize / Detach, Welcome / Frame / Shutdown | ✅ |
| Color + modifier decode (`color.go`) | ✅ named / 256-palette / RGB → CSS |
| herdr connection wrapper (`internal/herdrconn`) | ✅ handshake + typed send/recv |
| rweb gateway: page + `/ws` WebSocket bridge (`cmd/gateway`) | ✅ one herdr client per browser tab |
| Browser canvas renderer + keyboard input (`cmd/gateway/web/index.html`) | ✅ renders frames; key→bytes mapping |
| **Frame diffing** (gateway sends only changed cells) | ✅ full frame ~53 KB → steady-state diffs ~100 B |
| **Mouse input** (SGR 1006) gated on server `MouseCapture` | ✅ MouseCapture decodes; browser sends drag/wheel/click |
| **Clipboard**: herdr→browser copy (OSC 52) | ✅ |
| **Paste**: ⌘V text (`InputEvents::Paste`) / Ctrl+V image (`ClipboardImage` → staged file path) | ✅ verified end-to-end into Claude Code |
| **OSC 8 hyperlinks** (click-to-open when mouse not captured) | ✅ |
| **Window title** + **notify toasts** | ✅ |
| Kitty graphics passthrough | ⏳ deferred |
| Headless end-to-end verification (`cmd/probe` — `wire` + `ws` subcommands) | ✅ handshake, frame, diffs, mouse-capture confirmed |
| Browser→herdr input/mouse/paste exercised against a live session | ⏳ coded; not injected into the real session (gated) |

The installed herdr 0.7.0 server speaks **protocol 14**; `internal/wire.ProtocolVersion`
matches. Proto 14 inserted `ServerMessage::WindowTitle` at index 7, shifting `MouseCapture`
to 9 — handled in `internal/wire`. The server renders per-client at each client's requested
size, so attaching a web client does not resize other clients' views.

## Run

Only one binary needs to be built and run: the **gateway**. There is no separate
frontend build — the browser UI (`web/index.html`) is embedded into the gateway at
compile time via `go:embed`.

1. **Have a `herdr server` running** (the installed Rust herdr). The gateway is a thin
   client — it attaches to an already-running herdr session over a Unix socket; it does
   not start herdr itself. If you already use herdr in your terminal, this is satisfied.

2. **Build and run the gateway:**

   ```bash
   go run ./cmd/gateway --addr :8420
   ```

   Or build a durable binary:

   ```bash
   go build -o herdr-gateway ./cmd/gateway
   ./herdr-gateway --addr :8420
   ```

3. **Open `http://localhost:8420`** in your browser.

`--socket` defaults to `~/.config/herdr/herdr-client.sock` (the default session). The
gateway attaches to whatever session that socket belongs to, so the browser controls that
live session — keystrokes reach its focused pane, tmux-style, alongside any terminal
clients attached to the same session.

> **Note:** because `web/index.html` is compiled into the binary, after editing it you
> must **restart the gateway** (`go run` recompiles and re-embeds) — a browser reload
> alone will keep serving the old page.

### Headless verification (optional)

`cmd/probe` is a diagnostics tool — never needed just to use herdr in the browser:

```bash
# Smoke-test the wire protocol directly against the herdr server (read-only, no gateway):
go run ./cmd/probe wire --frames 2

# Full browser↔gateway↔herdr frame round-trip, no browser needed (read-only):
go run ./cmd/probe ws --msgs 8
# add --send-input to also exercise the keyboard path (reaches the focused pane!)
```

## Layout

```
internal/wire/        bincode codec, wire messages, framing, color decode
internal/herdrconn/   herdr client connection (handshake, send/recv)
cmd/gateway/          rweb web server + WebSocket bridge + embedded canvas UI
cmd/probe/            headless verification: `probe wire` (direct protocol smoke
                      test) and `probe ws` (stdlib WebSocket client, end-to-end)
```

## What's next (migration roadmap)

- **Phase A polish:** mouse/wheel input, OSC 8 hyperlinks, Kitty graphics passthrough,
  clipboard (OSC 52), frame diffing to cut bandwidth, per-tab isolation.
- **Phase B:** move PTY + VT emulation into Go via `go.mitchellh.com/libghostty`
  (go-libghostty), shrinking the Rust surface. Note: go-libghostty links libghostty-vt,
  which must be built with Zig — the same toolchain herdr uses.
- **Phase C:** port herdr's portable logic (app state, BSP layout, agent detection,
  session/workspace) to Go and retire the Rust core.
