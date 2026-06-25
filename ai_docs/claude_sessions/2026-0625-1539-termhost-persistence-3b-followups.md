# herdr-web (Go) — Phase B: termhost persistence 3b follow-ups (orphan-close + clean-quit shutdown)

**Date:** 2026-0625-1539 · **Session:** 73da5cf4-bb13-4940-861c-4ac85372e90e
**Repo:** `~/projs/go/herdr-web` (Go daemon) · paired with `~/projs/rust/herdr` (orchestrator).
**Branch (Go):** `roh/phase-b` · **Branch (Rust):** `roh/phase-b-termhost-client`

> Continuation of the 3b session (`…1817…`). After the persistent daemon + reconnect/
> adopt landed, knocked out two of the documented 3b follow-ups. **Both branches
> pushed** at end of session.

---

## What shipped (Rust only — Go side was already complete from `…1817…`)

### 1. Close orphan termhost panes after restore — herdr `ed1a8be`
On reconnect the daemon reports its live panes in `welcome.panes`. Restore adopts the
ones the session references; the daemon may also hold a pane the session doesn't
(e.g. one spawned just before the prior herdr crashed, before its session autosave) —
a live shell that would otherwise leak until the idle timeout.
- `client.rs`: `close_orphans()` closes each `surviving_panes` entry not in the client's
  `panes` map (i.e. not adopted/created this run); returns the count.
- `mod.rs`: `close_restored_orphans()` **peeks** the cached client (`CLIENT.get()`, no
  forced connect — if the backend was never used this run there's nothing to do) + logs.
- `headless.rs`: called once after restore in **both** server startup paths (normal +
  handoff-import), after seeding, before the server loop.
- **Race-free** because restore is synchronous: every restored pane registers in the
  client (`adopt_pane`/`create_pane`) before `App::new` returns.
- Unit test: `welcome.panes=[1,2,3]`, adopt 2, `close_orphans` closes exactly 1 and 3.

### 2. Clean server quit stops the persistent daemon — herdr `49cec55`
`run_server()` never tore the daemon down, so a clean exit left it lingering to the
idle timeout. The trick was distinguishing **clean quit** (kill it) from **handoff**
(keep it — the replacement reconnects/adopts); both exit via the same `complete_shutdown`.
- `headless.rs`: new `handed_off` flag set in `perform_live_handoff`. After
  `server.run()` returns, `run_server` calls `termhost::shutdown()` **only when
  `!handed_off`**. Clean quit / Ctrl-C / API shutdown → false → daemon torn down;
  completed handoff → true → daemon kept; crash → never reaches the call → daemon
  survives.
- e2e `termhost_clean_server_quit_stops_daemon`: SIGINT herdr → assert the daemon
  exits and removes its socket.

## The lifecycle contract — now proven by a trio of e2e tests

| Exit path | Daemon | Test |
|---|---|---|
| Clean quit (SIGINT / Ctrl-C / API shutdown) | **dies** | `termhost_clean_server_quit_stops_daemon` |
| Crash (SIGKILL) | **survives** | `termhost_managed_daemon_is_persistent_and_survives_herdr_death` |
| Restart (after crash) | survives → **reconnect + adopt** | `termhost_pane_survives_herdr_restart` |

**Design note surfaced:** a clean SIGTERM/Ctrl-C of the *server* now kills the daemon,
so the persistence-preserving restart path is specifically the **live handoff** (binary
upgrade) or a crash. A deliberate stop+start within the idle-timeout window also still
reconnects. Matches the design doc's "quit kills it, restart/handoff keeps it" intent.

## Verification (all green)

- Rust: feature-off build + clippy clean; feature-on build + clippy clean (only the
  pre-existing `actions.rs:2722` warning remains). `close_orphans` unit test passes
  (full bin suite **1921**, +1 from the prior 1920). e2e: clean-quit + persistence +
  restart **3/3** against the real daemon.
- *Env:* same zig-0.16-vs-0.15 workaround as `…1817…` — reused the prebuilt
  `vendor/libghostty-vt/zig-out/lib/libghostty-vt.a` via a no-op `ZIG` wrapper to build
  the herdr test binary; Go daemon builds with `-tags ghostty` directly.

## Pushed

```
Go   (roh/phase-b):                 df8daef..bac2710  (7 commits this 3b arc)
Rust (roh/phase-b-termhost-client): e974e5f..49cec55  (3 commits: 222f0cb, ed1a8be, 49cec55)
```

## Remaining 3b follow-ups (documented in `ai_docs/termhost-persistence-design.md`)

Neither blocks gap #4:
- **Live handoff (scenario C) e2e**: shares the reconnect/adopt path and works because
  the old server doesn't kill the daemon on handoff (`handed_off`), but isn't
  *separately* e2e'd (restart/SIGKILL = scenario B is). Note: during handoff the new
  server's `connect()` may briefly block on the daemon's serial-Attach until the old
  server detaches — worth confirming in that e2e.
- **Single-writer hardening**: serial `Attach` gives the core guarantee; no explicit
  token/lock against two concurrent herdrs yet.
- **Socket path length**: `data_dir()`-based socket can exceed `sockaddr_un`'s ~104B
  limit in pathologically deep config dirs (fine for normal `~/.config`; only bit us
  with the very long scratchpad path when hand-launching a shared test daemon).

Then **gap #4** = flip termhost default + delete the in-process PTY path (mostly
subtraction). Kitty graphics still back-burnered.
