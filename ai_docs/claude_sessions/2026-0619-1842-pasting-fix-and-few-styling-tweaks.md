# pasting-fix-and-few-styling-tweaks

**Date:** 2026-0619-1842
**Session ID:** `abdb76c5-c85a-4c2c-a052-bfb246f46a45` (continuation of the herdr-web-impl session)
**Project:** `~/projs/go/herdr-web` · references `~/projs/rust/herdr` (Rust source)

---

## Scope

Follow-up polish on the Phase A web gateway (`herdr-web`): sidebar focus emphasis,
fixing paste, adding image paste, and rendering dim text. All changes are in the web
client / gateway; herdr's Rust binary is unchanged (only read for reference).

The gateway attaches to the running herdr **0.7.0** server (protocol 14) and is served at
`http://localhost:8420`. Testing was done against the user's live default session
(read-only where possible; the user drove the browser for interactive checks).

---

## Work done (all committed + pushed to github.com/rohanthewiz/herdr-web)

### 1. Emphasize the active sidebar row — `c0c88aa`
- herdr marks the active agent/workspace row with a subtle `surface_dim` background
  (`src/ui/sidebar.rs:888`). Too faint in the browser.
- Web fix in `cmd/gateway/web/index.html` `draw()`: detect the left-anchored,
  partial-width highlight band (anchored to column 0 → sidebar only; width-capped at 55%
  so full-width bars don't trigger), brighten it slightly, and draw a bright accent stripe
  (`#5b9dff`) down its left edge.
- Brighten amount tuned down 0.17 → **0.09** per user feedback (stripe carries the cue).

### 2. Text + image paste — `209eb8c` (the main fix)
- **Root-cause bug:** the browser sent paste as `{t:"paste", text:…}` but the gateway's
  `clientCmd` reads the `data` field (`json:"data"`), so an **empty paste** was forwarded.
  Typing worked because it already used `data`. Fixed by sending paste text in `data`.
  - Diagnosis path: confirmed the wire encoding was correct (InputEvents=7, Paste=2 in
    `v0.7.0`), confirmed typing worked (so `Mode::Terminal` was active and the server's
    shared mode gate at `src/app/mod.rs:1381` wasn't the issue), which isolated it to the
    gateway field-name mismatch.
- **Cmd+V** → `navigator.clipboard.readText()` → structured `InputEvents::Paste` (server
  applies bracketed-paste framing only when the focused app enabled it).
- **Ctrl+V** → `navigator.clipboard.read()` → image blob → base64 → new `{t:"image"}` →
  gateway `EncodeClipboardImage` → `ClientMessage::ClipboardImage`. Server stages the image
  to a temp file (`…/T/herdr-clipboard-images-502/…png`) and pastes the **file path** into
  the focused pane — exactly how Claude Code ingests images. Verified end-to-end (a pasted
  screenshot containing "testing this text" arrived correctly).
- A focused `<canvas>` emits no native `paste` event, so reads go through the async
  Clipboard API on the keydown gesture. Added **toast feedback** for permission/empty/
  unsupported cases (this is how we caught the "pasted 17 chars but prompt empty" mismatch).
- New code: `wire.EncodeClipboardImage`, `herdrconn.SendClipboardImage`, gateway `image`
  case + `Ext` field + `encoding/base64`, browser `pasteText`/`pasteImage`/`sendImage`/
  `base64FromBuffer`, and the Cmd+V/Ctrl+V keydown split.

### 3. Render the DIM modifier — `ac7666a`
- Claude Code's prompt suggestions (SGR 2 faint / ratatui `Modifier::DIM` = bit `0x2`) were
  rendering white because the renderer ignored DIM.
- Added `M_DIM = 0x2` and a `blend(fg, bg, t)` helper; faint cells blend fg **0.5** toward
  bg → muted gray on the dark theme. User confirmed the gray looks right.
- Refactored color math into `parseHex`/`toHex` (shared by `lighten` and `blend`).
- README row updated (`ac7666a` precedes a small README doc commit on prior work).

---

## Key facts / gotchas (for future sessions)

- Paste/typing both flow through the server's `route_client_events`; paste only reaches the
  pane in `Mode::Terminal`. If a paste "succeeds" in the browser but nothing appears, check
  (a) the gateway field name, then (b) whether herdr is in terminal mode.
- herdr image paste = `ClipboardImage` → temp file → path pasted (not inline image bytes).
- The async Clipboard API needs a secure context — `localhost` qualifies; a LAN IP does not.
  `readText`/`read` are unsupported in Firefox web content (would need a hidden-textarea
  fallback). User is on a Chromium/WebKit browser on macOS, so it works.
- Renderer now handles bold/dim/italic/underline/reverse/hidden + named/256/RGB color +
  OSC8 hyperlinks + cursor + frame diffing + SGR mouse + the sidebar focus accent.

---

## How to run

```bash
cd ~/projs/go/herdr-web
go build ./...
go run ./cmd/gateway --addr :8420   # open http://localhost:8420
# stop: pkill -f herdr-gateway
```

Gateway left running on `:8420` at end of session.

---

## Commits this session (on origin/main)

- `c0c88aa` feat: emphasize the active sidebar row in the web client
- `209eb8c` feat: text and image paste in the web client
- `ad011c3` docs: mark text and image paste verified in README
- `ac7666a` feat: render the DIM modifier as faint text in the web client

## Next step

Phase B spike: `brew install zig` (0.16) + `go.mitchellh.com/libghostty` to move PTY + VT
emulation into Go — also the first real test of Zig vs macOS SDK 26.5.
