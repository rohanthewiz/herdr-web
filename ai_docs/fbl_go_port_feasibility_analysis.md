# herdr → Go: full-port feasibility analysis + Phase C (web-only) work-breakdown

**Date:** 2026-07-01 · **Author:** analysis session · **Repos:** `~/projs/go/herdr-web` (Go), `~/projs/rust/herdr` (Rust orchestrator).

**Goal:** migrate *all* of herdr off Rust onto Go. This doc records (Part 1) the feasibility
assessment and (Part 2) a concrete Phase C work-breakdown under the decision to make the
**browser the only front-end** (retire the native ratatui TUI).

---

# Part 1 — Feasibility analysis

## Bottom line

Feasible, and further along than raw LOC suggests. The migration is a 4-phase plan
(A→D). **Phase A is done, Phase B ~90% done.** "Everything to Go" = Phase C+D, not yet
started except agent detection (already ported). One unavoidable asterisk: the terminal
engine stays Zig/C (libghostty-vt via CGO) — that's fine and shouldn't be fought.

## Phase status

| Phase | Scope | Status |
|---|---|---|
| **A** | Go/rweb browser gateway attached to the unmodified Rust server (bincode wire client) | ✅ Done |
| **B** | Go owns PTY + VT (go-libghostty) behind the `termhost` daemon; Rust stays orchestrator | 🟡 ~90% — "gap #4" left |
| **C** | Port the portable Rust core (app state, layout, session, detection, UI) to Go | ⬜ Not started (detection done) |
| **D** | Delete Rust entirely | ⬜ Not started |

**Done in Go (~8k LOC):** Phase-A gateway (bincode codec, frame diffing, mouse, clipboard,
paste, hyperlinks, titles); Phase-B `termhost` daemon (PTY+VT per pane); OSC 7/9/0/2/52
passthrough; **full agent detection** (17 JSON manifests + rules engine); selection, text/
scrollback extraction, input-mode reporting; **persistence** — panes survive client detach,
herdr crash/restart (live shells), and live binary handoff.

**Remaining Phase-B step ("retire-Rust gap #4"):** flip termhost to default and delete Rust's
in-process PTY/ghostty/terminal path. "Mostly subtraction"; nothing technical blocks it.
Completing it means **Rust no longer links libghostty at all** (~5.4k LOC of FFI retires).

## Size of the Rust core

136,809 total LOC but **~53% is inline tests** → **~64.5k production LOC**. Two big chunks
already handled: VT emulation (`src/ghostty`, ~5.4k) done via go-libghostty; PTY (`src/pty`)
moved to Go. **Net to port ≈ 55–58k prod LOC** (before the web-only deletions in Part 2).

### Rust module inventory (total LOC / est. prod LOC)

| Module | Total | ~Prod | Port difficulty | Notes |
|---|---:|---:|---|---|
| `src/app` | 34,668 | ~12,400 | 🟡 Medium | Orchestrator core (event-loop + channels) |
| `src/*.rs` top-level | 20,654 | mixed | mixed | `update.rs` 3471 prod; `pane.rs`/`raw_input.rs` ~all test |
| `src/server` | 11,037 | ~2,300 | 🟡 Medium | `headless.rs` = 7,680 (mostly tests) |
| `src/ui` | 7,814 | ~6,200 | 🔴 Hard | ratatui chrome — **web-only deletes this** |
| `src/pane` (dir) | 7,318 | ~3,900 | 🟡 Medium | runtime, OSC, xtgettcap |
| `src/ghostty` | 5,870 | ~5,400 | ✅ Already Go | libghostty FFI |
| `src/integration` | 5,913 | ~5,900 | 🟢 Easy | shell-hook installers + embedded assets |
| `src/terminal` | 4,645 | ~840 | ✅/🟡 | mostly test |
| `src/api` | 4,108 | ~3,400 | 🟢 Easy | JSON control API |
| `src/config` | 4,078 | ~1,500 | 🟢 Easy | TOML keybinds/model |
| `src/detect` | 3,981 | ~2,900 | ✅ Already ported | `internal/detect` |
| `src/cli` | 3,757 | ~3,400 | 🟢 Easy | subcommands |
| `src/protocol` | 3,584 | ~810 | 🟡 Medium | wire.rs + render_ansi.rs |
| `src/persist` | 3,155 | ~1,100 | 🟡 Medium | session snapshot/restore |
| `src/workspace` (dir) | 3,100 | ~2,100 | 🟢 Easy | tabs, git status |
| `src/platform` | 2,891 | ~2,200 | 🟡 Med-Hard | macOS 1120 / linux 759 / win 710 |
| `src/remote` (dir) | 2,496 | ~1,700 | 🔴→delete | SSH thin-client — **web-only deletes this** |
| `src/client` | 2,354 | ~1,800 | 🔴→delete | native attach client — **web-only deletes this** |
| `src/pty` | 1,849 | ~640 | ✅ Already Go | creack/pty |
| `src/input` | 1,814 | ~910 | 🟡→mostly delete | crossterm key/mouse decode |
| `src/termhost` | 1,723 | ~1,200 | retires | Rust side of the Phase-B seam |

## Dependencies → Go

24 direct crates (lean). Hard ones: **ratatui** (no Go equivalent — the #1 UI risk, deleted
in web-only), **crossterm** (no single lib — deleted in web-only). Everything else is
🟢/🟡: tokio→goroutines+channels, bincode→hand-rolled codec (already have `internal/wire`),
portable-pty→creack/pty (done), libc SCM_RIGHTS→`x/sys/unix`, serde_json→encoding/json,
toml→go-toml, regex→regexp (RE2 — audit manifests for lookaround), tracing→slog,
unicode-width→go-runewidth, base64/sha2/png→stdlib.

## Concurrency — a favorable surprise

Not combinator-heavy async. Event-loop-over-channels: 357 mpsc refs, only 112 `.await`
points, 13 `Arc<Mutex>`. Maps ~1:1 onto goroutines + `select` + channels. Little "async
coloring" to unwind. One of the *easier* aspects conceptually.

## The "pure Go" asterisk

The VT engine is Zig `libghostty-vt`, statically linked via CGO in go-libghostty. A fully
migrated herdr is *"a Go app with one isolated CGO dependency,"* not zero-native. Don't
reimplement a VT emulator in Go. Practical cost: toolchain fragility (libghostty-vt pins Zig
0.15.2; macOS 26.5 dropped the `arm64-macos` linker slice → SDK-patch + Zig-shim workaround,
already scripted under `.tools/`; CI must cache the prebuilt `.a`).

## The two hard problems (and how web-only defuses them)

1. **UI/ratatui (~6.2k)** — no Go equivalent. **Web-only deletes it**: render chrome as
   HTML/Element, panes as canvases. This is the whole point of the web-only decision.
2. **Live handoff (SCM_RIGHTS)** — fiddly syscall + re-exec dance. **Likely obsolete**: the
   persistent Go daemon already keeps shells alive across an orchestrator restart, so handoff
   becomes "new orchestrator reconnects." Delete rather than port.

## Loose ends

- Uncommitted scenario-C handoff fix in the Rust working tree (6 files) — review/commit.
- Version skew: local Rust is v0.6.10/proto 13; installed binary is 0.7.0/proto 14.
- 3b gaps: single-writer only via serial-Attach (no explicit lock); `data_dir()` socket path
  can exceed `sockaddr_un`'s ~104B in deep config dirs.
- Kitty graphics deferred both sides.

---

# Part 2 — Phase C work-breakdown (web-front-end only)

**Decision recorded:** the browser is the **only** front-end. The native ratatui TUI, the
native attach client, and the SSH thin-client are **retired, not ported**. Remote access
becomes "serve the gateway over the network + auth."

## Target end-state architecture

```
        Browser  (the only front-end)
          │  WS ↑ : layout tree + per-pane cell-grid diffs + chrome state (titles/agents/toasts)
          │  WS ↓ : key / mouse / paste / resize / commands
          ▼
    ┌──────────── Go herdr server (single binary, no Rust) ────────────┐
    │  web gateway (rweb + Element)              [evolve cmd/gateway]    │
    │  orchestrator: workspaces · tabs · BSP     [WS1 + WS2 — new port]  │
    │      layout · focus · session · detection                          │
    │  control API / CLI / config                [WS4]                   │
    │  platform: clipboard · notify · procinfo   [WS6]                   │
    └───────────────────────────┬──────────────────────────────────────┘
                                 │  (keep the socket → free crash-survival, reuse Phase-B persistence)
                                 ▼
    ┌──────────── termhost daemon (persistent) ────────────────────────┐
    │  pane = PTY (creack/pty) + Emulator (go-libghostty)   [DONE]      │
    └───────────────────────────┬──────────────────────────────────────┘
                                 ▼ CGO
                          libghostty-vt (Zig)   ← the one native dependency
```

**Key architectural choice — chrome is HTML, not a composited cell grid.** Today Rust
composites the *entire* UI (sidebar/tabs/panes) into one semantic grid that the browser
canvas renders. In web-only we stop compositing chrome: the browser renders sidebar/tabs/
status/dialogs as **HTML (Element)** and renders **each pane as its own canvas** fed by that
pane's cell grid — which `internal/orchestration` already produces (`pane_frame`). This is
what makes web-only cheap: it deletes both `src/ui` *and* server-side chrome compositing, and
reuses the per-pane grid machinery already built.

**Second choice — keep the termhost daemon split** (don't collapse in-process). It's already
built, and it buys pane survival across an orchestrator restart for free (Phase-B
persistence). The orchestrator talks to it over the existing socket protocol.

## What gets DELETED (not ported) under web-only

| Rust | ~Prod LOC | Replaced by |
|---|---:|---|
| `src/ui` (ratatui chrome) | ~6,200 | WS8 — HTML/Element chrome |
| `src/client` (native attach) | ~1,800 | the browser is the client |
| `src/remote` (SSH thin-client) | ~1,700 | WS10 — network-served gateway + auth |
| `render_ansi.rs` + `TerminalAnsi` encoding | ~1,600 | browser gets per-pane cell grids |
| `src/input` crossterm decode | ~700 | browser sends structured events; go-libghostty encodes |
| server-side chrome compositing (in `app`) | (subset) | browser composites via HTML |
| **≈ deleted** | **~12k** | |

**Revised net-to-port for web-only ≈ 44k prod LOC** (55–58k − ~12k deletions − detection
already done), **plus** net-new web-UI work (WS8/WS9/WS10).

## Workstreams

Sizes are T-shirt (S ≤ ~1 wk, M ~2–4 wk, L ~1–2 mo) and rough — calibrate after WS1.

### WS0 — Finish Phase B "gap #4" *(prerequisite, S)*
Flip termhost to default; delete Rust in-process PTY/ghostty/terminal path. Clean starting
line: Go is the sole terminal backend, Rust no longer links libghostty.
**Depends on:** nothing. **Deliverable:** Rust builds without the `ghostty`/Zig toolchain.

### WS1 — Core data model & layout *(M, foundation)*
Port `src/layout.rs`, `src/workspace/`, and the pane-tree (`Node::{Pane,Split}`, `Rect`
math, ratio resize, cardinal navigation). Pure algorithm, no FFI/async. **Port the Rust unit
tests as Go table tests first** — they're the spec.
**Depends on:** nothing. **Everything below depends on this.** **Risk:** low.

### WS2 — Orchestrator / app state & event loop *(L, the big one)*
Port `src/app` minus rendering: the `AppEvent` loop, command handling (create/close/split/
focus/resize pane, switch tab/workspace), keybinding dispatch, input routing to the focused
pane. Drive panes through the existing `internal/orchestration` client (in-process call into,
or socket to, the termhost daemon). Model as goroutines + a central `select` loop.
**Depends on:** WS1, WS9 (protocol). **Risk:** medium (volume, not concept). Largest chunk.

### WS3 — Session persistence & restore *(M)*
Port `src/persist` + `src/session.rs`: JSON snapshot/restore of workspaces/tabs/layout/cwds;
respawn panes via orchestration with `initial_history` seeding (already built). Replace
handoff with the persistent-daemon reconnect (already built) — **delete `handoff.rs`, don't
port it**.
**Depends on:** WS1, WS2. **Risk:** low-medium.

### WS4 — Config, keybindings, CLI, control API *(M, easy)*
Port `src/config` (TOML → go-toml), `src/cli` (subcommands), `src/api` (JSON control API +
schema). Mechanical. High LOC, low difficulty.
**Depends on:** WS1/WS2 for the commands they invoke. **Risk:** low.

### WS5 — Detection wiring *(S, mostly done)*
`internal/detect` is ported. Remaining: wire detection state into WS2's pane model, surface
`pane_agent` to the browser (WS9), and port the manifest-update fetcher (`manifest_update.rs`)
if you want in-app updates.
**Depends on:** WS2, WS9. **Risk:** low.

### WS6 — Platform integration *(M, per-OS)*
Clipboard, notifications, process introspection. macOS (cgo where needed) + Linux; Windows
optional/stub. procscan already exists in `internal/detect` for macOS/Linux.
**Depends on:** none hard. **Risk:** medium (per-OS quirks).

### WS7 — Integration / shell hooks *(M, easy)*
Port `src/integration`: shell-hook installers + embedded agent-detection/title hook assets.
Mostly file-writing + `//go:embed`. High LOC, low difficulty.
**Depends on:** WS4 (CLI entry points). **Risk:** low.

### WS8 — Web front-end: HTML chrome + per-pane canvases *(M–L, partly built already)*
**Replaces `src/ui`.** **Starting asset (Phase A, working today in `cmd/gateway/web/index.html`):**
a canvas cell-grid renderer plus keyboard input, SGR mouse, text+image paste, OSC 52
clipboard, OSC 8 hyperlinks, window title, and notify toasts — all proven end-to-end. So the
per-pane *content* renderer and the input path are **not** net-new.
**What IS net-new** (the real WS8 work):
1. **Split one composited grid into per-pane canvases.** Today the browser renders a *single*
   grid because Rust composites chrome+panes together. Move to one canvas per pane, positioned
   by the BSP-tree rects from WS1 (CSS grid/flex), each fed its own `pane_frame` diff.
2. **Render chrome as HTML/Element** instead of as composited cells: sidebar, tab bar, status
   line, dialogs/menus, driven by chrome-state messages (WS9) rather than a cell grid.
3. Wire focus/split/close/resize/tab-switch UI gestures to WS9 commands.
**Depends on:** WS9 (protocol), WS1 (layout shape). **Risk:** medium — the hard rendering/input
primitives already exist; the effort is chrome-as-HTML + per-pane layout, not a from-scratch UI.
Still one of the larger deliverables, but smaller than a greenfield estimate.

### WS9 — Browser-facing protocol *(M)*
Define the single WS protocol between Go server and browser, consolidating Phase-A wire +
Phase-B orchestration: layout tree, per-pane grid diffs, chrome state (titles/agent-status/
notifications/cwd), and inbound key/mouse/paste/resize/command events. Reuse the
`internal/orchestration` frame shape and the `cmd/gateway` WS bridge. `internal/wire`
(bincode) becomes vestigial once Rust is gone (keep only during transition if still attaching
to a Rust server).
**Depends on:** informs WS2 & WS8. **Risk:** low-medium.

### WS10 — Remote access & auth *(S-M, new)*
Web-only drops the SSH thin-client, so remote access = serving the gateway over the network.
Add auth (token/password), TLS, and a session/origin check. Today the gateway is
localhost-only.
**Depends on:** WS8/WS9. **Risk:** medium (security-sensitive — do it deliberately).

### WS11 — Cutover, packaging, CI *(M)*
Delete the Rust repo from the runtime path. Ship a single Go binary + per-platform
libghostty-vt `.a`. CI caching of the Zig build (the toolchain workaround). Migrate the
session-file format if it changed.
**Depends on:** everything. **Risk:** medium (the Zig build in CI is the sharp edge).

## Suggested sequencing

```
WS0 ─▶ WS1 ─┬─▶ WS2 ─┬─▶ WS3
            │        ├─▶ WS4 ─▶ WS7
            │        └─▶ WS5
            ├─▶ WS9 ─▶ WS8 ─▶ WS10
            └─▶ WS6
                              WS11 (last)
```

1. **WS0 + WS1** — clean starting line + foundation (data model/layout with ported tests).
2. **WS9 then WS2** — lock the browser protocol, then build the orchestrator against it.
3. **WS8 in parallel with WS2** — the web UI and the orchestrator co-evolve against WS9.
4. **WS3/WS4/WS5/WS6/WS7** — fill in persistence, config/CLI/API, detection wiring, platform,
   hooks (mostly independent, parallelizable).
5. **WS10** — remote/auth once the local web app is solid.
6. **WS11** — cutover, delete Rust, package.

## Rough effort

~44k prod LOC to port + net-new web UI. With the concurrency model being a natural Go fit and
the tests serving as a spec, estimate **~2.5–4 focused months** for one experienced engineer;
WS2 (orchestrator) and WS8 (web UI) dominate. The estimate's biggest lever is scope discipline
on WS8 (match current chrome, don't gold-plate).

## Open decisions to lock before coding

1. **Daemon split vs. in-process** terminal backend (recommend: keep the split — free
   persistence, already built).
2. **Chrome = HTML** (recommend: yes — the reason web-only is cheap) vs. server-composited
   grid (would drag ratatui-equivalent rendering back in).
3. **Windows support** — port `platform/windows` + winio, or macOS/Linux only?
4. **In-app manifest updates** — port `manifest_update.rs` or drop.
5. **Auth model** for WS10 (token vs. password vs. reverse-proxy-delegated).
</content>
