# Termhost persistence — design / scoping (retire-Rust #3)

**Status:** scoping (no code yet). **Date:** 2026-06-24.
**Repos:** `~/projs/go/herdr-web` (Go daemon) · `~/projs/rust/herdr` (orchestrator).

Goal: make termhost panes survive the three "the herdr process or client goes away"
scenarios, on par with the in-process backend. This is the last big correctness gap
before termhost can be the default and the Rust in-process PTY path retired.

---

## The three scenarios (and current termhost status)

herdr already handles these for in-process panes via two mechanisms — fd-passing
handoff (`src/server/handoff.rs`) and cold session restore (`src/persist/restore.rs`,
`seed_history_ansi`).

| Scenario | In-process mechanism | Termhost status today |
|---|---|---|
| **A. Client detach / reattach** (server stays up; TUI client comes and goes; persistence mode) | Server process persists; panes untouched | **Should already work** — the daemon connection is a process-wide `static CLIENT` (termhost/mod.rs), independent of TUI clients, so the daemon + panes survive a client detach. **Verify** with a manual detach/reattach run. |
| **B. Cold session restore** (herdr process exits and restarts) | `restore.rs` re-spawns each shell + `seed_history_ansi` (replays saved scrollback as text; the live process is gone) | **Broken** — the daemon dies with herdr (`--exit-on-disconnect`), and `create_pane` has no way to carry seeded history. `snapshot_history` now *captures* history (gap #2 ✅) but nothing replays it. |
| **C. Live handoff** (binary upgrade: old herdr → new herdr, shells must keep running) | Old server passes live PTY master fds (SCM_RIGHTS) to a spawned `--handoff-import` server | **Broken** — termhost PTYs live in the *daemon*, not herdr, so there are no fds to pass (the `PaneRuntimeIo::Termhost` handoff arms are already no-ops). |

## Key realization: a persistent daemon unifies B and C — and beats both

For B and C the PTYs live in the Go daemon. If the **daemon outlives the herdr
process** and a new herdr **reconnects + resyncs**, then:

- **C (handoff)** becomes "new herdr reconnects to the existing daemon" — *no fd
  passing*, simpler than the in-process path.
- **B (restart)** becomes "reconnect and resync" — and the **shells SURVIVE the
  restart** (live processes, not just replayed history text). Strictly better than
  in-process cold restore, which loses the live process.

So the durable element is the daemon; herdr becomes a reconnectable front-end. This
is also the endgame shape (Go owns terminals; herdr orchestrates).

There is still a **cold-seed fallback** (B'): if no daemon is alive to reconnect to
(first run, daemon was killed, crash), herdr must re-create panes on a fresh daemon
and seed history text — the termhost analogue of `seed_history_ansi`.

---

## Work breakdown

### Phase 3a — cold-seed + verify detach (small, high value, low risk)

Gets B working at the "replayed history" level (parity with in-process restore) and
confirms A. No persistent-daemon architecture yet.

1. **`create_pane` carries seeded history.** Add `initial_history` (ANSI string) to
   the `CreatePane` command. Go feeds it to the emulator (`emu.Write(ansi)`) right
   after creation, before the child's first output — the analogue of
   `seed_history_ansi`. herdr's `finish_termhost` passes `initial_history_ansi`
   (already threaded through `restore.rs` → `spawn_with_initial_history`) into the
   `PaneSpec`.
2. **Verify A** (client detach/reattach) with a manual run; add a regression e2e if
   cheap (detach client, reconnect, assert frames resume).
3. Result: a restored session re-spawns termhost shells with their scrollback
   replayed — same UX as in-process restore. Shells are fresh (not the originals).

### Phase 3b — persistent daemon + reconnect/resync (large, the real piece)

Makes shells *survive* a herdr restart and live handoff. This is the architecture
change.

1. **Persistent lifecycle mode.** A daemon mode that does NOT exit on disconnect:
   keep panes alive, wait for a new herdr to reconnect. Replaces/augments
   `--exit-on-disconnect`. Needs a GC policy (below).
2. **Stable, discoverable socket.** Key the socket by *session*, not pid (pid changes
   on restart). e.g. `data_dir()/herdr-termhost-<session-id>.sock` (mirrors
   `handoff_socket_path`). New herdr looks there first; spawns a fresh daemon only if
   none is alive/reachable.
3. **Reconnect + resync protocol.** On `hello` to an existing daemon, the daemon
   replays current state for every live pane: a `pane_created`-style list (or per
   pane: full `pane_frame` + `pane_modes` + `pane_cwd` + `pane_title` + `pane_agent`
   + scroll). herdr adopts them into the restored session instead of `create_pane`.
4. **Pane-ID reconciliation.** `create_pane` already keys panes by *herdr's* pane_id,
   and the session restore preserves pane_ids — so on reconnect herdr can match "do
   you still have pane X?" and adopt. Need to confirm pane_id stability across
   restart and define behavior for panes the daemon has but the session doesn't (and
   vice-versa).
5. **Single-writer ownership.** Only one herdr drives a daemon at a time. Persistent
   mode must accept a *new* connection after the old drops (today managed mode serves
   one then exits). On handoff the old herdr disconnects → new connects. Guard against
   two concurrent herdrs (token/lock, like the handoff manifest token).
6. **GC / shutdown policy.** A persistent daemon must not linger forever:
   - explicit `shutdown` command on herdr's *clean* quit (vs. a crash/handoff, which
     just disconnects) — so quit kills it, restart/handoff keeps it;
   - idle timeout (no reconnect within N minutes → exit);
   - exit when the last pane closes.
7. **Handoff integration.** Make the handoff export/import skip termhost panes (no fds
   to pass) and have the new herdr's daemon-reconnect adopt them. The existing
   fd-passing path stays for any remaining in-process panes during the transition.

---

## 3b status — SHIPPED (2026-06-24)

The persistent daemon + reconnect/resync landed and is e2e-proven: a termhost
shell **survives a full herdr restart** (scenario B at live-process fidelity —
strictly better than in-process cold restore). Commits: Go `c2710bc` (lifecycle),
`35207c0` (request_resync), `f7752c8` (SIGHUP); Rust `222f0cb`.

Resolved decisions: socket keyed by **session** (`data_dir()/herdr-termhost.sock`);
GC = clean-quit `shutdown` command **+** 10-min idle timeout; daemon detached via
**setsid** and ignores **SIGHUP** so it outlives herdr; reconnect repaint via an
explicit per-pane **`request_resync`** (avoids the connect-time replay race);
single-writer enforced by the daemon's **serial Attach**.

How it works: persistent daemon decouples pane lifetime from the connection
(`Start`/`Attach`/`Stop`); on `hello` it returns `welcome.panes` (live ids) and
replays each pane's state; a restarted herdr's restore reconciles its session
against `surviving_panes()` and **adopts** matches (`adopt_pane`, no `create_pane`)
instead of re-spawning, then `request_resync` repaints.

### Remaining 3b follow-ups (not blockers for #4)

- ~~**Orphan panes:**~~ DONE (Rust `ed1a8be`). `client.close_orphans()` closes
  `surviving_panes − adopted` after restore (`close_restored_orphans()` wired into
  both server startup paths). Reaps a live shell the daemon kept that the restored
  session doesn't reference (e.g. spawned just before the prior herdr crashed).
- ~~**Server-mode clean-quit shutdown:**~~ DONE (Rust `49cec55`). `run_server()` now
  calls `termhost::shutdown()` after `server.run()` returns, gated on a new
  `handed_off` flag (set in `perform_live_handoff`): a clean quit / Ctrl-C / API
  shutdown tears the daemon down; a completed handoff leaves it running; a crash skips
  teardown entirely. e2e `termhost_clean_server_quit_stops_daemon` (SIGINT → daemon
  exits) pairs with the SIGKILL-survives and restart-adopts tests.
- **Live handoff (scenario C):** shares the reconnect/adopt path and works because the
  old server doesn't kill the daemon; not yet separately e2e'd (restart/SIGKILL = B is).
- **Single-writer hardening:** serial Attach gives the core guarantee; no explicit
  token/lock against two concurrent herdrs yet.
- **Socket path length:** `data_dir()`-based socket can exceed `sockaddr_un`'s ~104B
  limit in pathologically deep config dirs (fine for normal `~/.config`).

## Open decisions (resolved — see 3b status above)

- **Daemon socket keying & discovery:** session-id vs user vs workspace. How does a
  new herdr know *which* daemon is "its" daemon? (Probably: one daemon per herdr
  session, socket named by the session id that the session file also records.)
- **GC policy specifics:** clean-quit `shutdown` + idle-timeout is the likely combo;
  pick the timeout and confirm the clean-vs-crash distinction is detectable (herdr's
  `shutdown()` already runs on graceful exit — send `shutdown` there; skip it on
  panic/handoff).
- **Crash recovery:** if the *daemon* crashes (not herdr), panes are lost — fall back
  to cold-seed (3a). Restart-on-crash supervision is a separate smaller item.
- **Resync payload size:** replaying full frames for many panes on reconnect could be
  large; acceptable (one-time) but worth a cap/stream.
- **Security:** the persistent socket outlives herdr — perms (0600, user-only dir),
  and the single-writer token, matter more than for the ephemeral managed socket.

## Suggested sequencing

1. **3a first** — ship cold-seed (`create_pane.initial_history`) + verify detach.
   Small, gets B to in-process parity, unblocks "termhost survives restart (as
   replayed history)". Low risk.
2. **3b** as a dedicated effort — persistent daemon. Decide the open questions, then:
   lifecycle mode → stable socket/discovery → reconnect/resync → ownership/GC →
   handoff integration. Each is independently testable (the e2e harness already
   spawns real daemons + herdr and can kill/restart them).

After 3, gap #4 (flip default + delete the in-process path) is mostly subtraction —
the local `PaneTerminal` only serves mirrored modes by then.

## Anchors (file:symbol)

- Cold restore: `src/persist/restore.rs:565` `spawn_with_initial_history`;
  `src/pane/terminal.rs:663` `seed_history_ansi`; `src/pane.rs:1652` history seeding.
- Handoff: `src/server/handoff.rs` (`spawn_handoff_import`, `handoff_socket_path`,
  manifest token); `src/pane.rs:899` `duplicate_handoff_fd` /
  `PaneRuntimeIo::Termhost` no-op arms; `src/pane.rs:1605` `from_handoff_fd`.
- Detach: `src/app/input/modal.rs:973` (client-detach in persistence mode).
- Daemon lifecycle: herdr-web `cmd/termhost/main.go` (`--exit-on-disconnect`);
  `src/termhost/mod.rs` (`client_if_enabled`, `spawn_and_connect`, `shutdown`,
  `static CLIENT`).
- Create/seed seam: herdr-web `internal/orchestration/protocol.go` `CreatePane`;
  `host.go` `createPane`; `internal/terminal` `Emulator.Write`.
</content>
