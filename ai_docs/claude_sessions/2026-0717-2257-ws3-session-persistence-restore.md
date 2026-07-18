# WS3: session persistence & restore (gateway2)

**Date:** 2026-0717-2257 · **Branch:** `roh/phase-b` (herdr-web)
**Continues:** `2026-0715-1752-ws4-wait-for-output-raw-byte-stream.md`; picks up the
recommendation of `ai_docs/2026-0715-1902-workstream-status-and-next.md` (WS4 done →
WS3 next: gateway2's `newOrch` always rebuilt a fresh 1-pane session; `InitialHistory`
was unwired).

> gateway2 now **persists the session model on every mutation and restores it at
> startup**: the workspace/tab/pane tree, split ratios, focus, zoom, custom
> names, and public numbering all survive a gateway restart. Against a live
> persistent termhost the surviving PTYs are **adopted** (shells keep running —
> live-process fidelity); on a **cold start** (daemon gone too) panes are
> re-spawned in their saved cwd with captured scrollback replayed via β
> `create_pane.initial_history` — the termhost analogue of herdr's
> `seed_history_ansi`. Live-verified end-to-end for both scenarios.

---

## The mechanism (four layers)

**1. `internal/layout` (`persist.go`).** `SavedNode`/`SavedSplit` — a
JSON-friendly mirror of the BSP tree (the `Node` interface can't round-trip
JSON); `SaveTree` / `(*SavedNode).Tree()` convert both ways, with validation
(one variant per node, both children, direction range, ratio clamp).
`ReservePaneIDs` CAS-raises the global pane-id counter past restored ids
(mirrors `ReserveWorkspaceIDs`). `FromSaved`/`Root()` already existed.

**2. `internal/workspace` + `internal/app` (`persist.go` each).**
`Tab.Snapshot`/`Workspace.Snapshot`/`Session.Snapshot` and
`workspace.Restore`/`app.RestoreSession`. Key points:
- The **public-numbering counters are persisted verbatim** (`next_pane_number`,
  `next_tab_number`) — numbers are never reused after a close, so re-deriving
  max+1 would resurrect a closed pane's handle. Restore takes
  `max(persisted, derived)` so a stale counter can never go backwards.
- Restore **re-spawns every pane's terminal through the `PaneSpawner` seam**
  (TerminalIDs are not persisted), preserving the invariant that every attached
  terminal came from the spawner.
- Everything validates loudly — out-of-tree focus/root, tree-vs-state pane
  mismatch, out-of-range indices, **duplicate pane ids across workspaces** (the
  daemon keys PTYs by pane id) — and the caller falls back to a fresh session.
- `RestoreSession` reserves both global id counters itself. FocusLast history
  (`layout.prev`) is deliberately not persisted.

**3. `internal/persist` (untagged, no ghostty).** Two versioned JSON state
files with different rhythms, atomic writes (same-dir temp + fsync + rename),
0600/0700 perms:
- `session.json` — `{version, session: app.Snapshot, pane_cwds}`. Small,
  written debounced on every mutation. `pane_cwds` is each pane's last
  OSC 7-reported cwd so a cold restore re-spawns shells where they were.
- `history.json` — `{version, panes: {id: ANSI}}`. Larger, written by the
  periodic capture sweep + final capture.
- Default dir `$XDG_STATE_HOME/herdr` → `~/.local/state/herdr` (state ≠
  config; same XDG-with-fallback style as the config file).

**4. `cmd/gateway2` wiring (`persist.go` + edits).**
- **Restore at startup** (`main.go buildOrch`): load → `RestoreSession` →
  `newOrchWith` (refactored out of `newOrch`); any problem beyond "no file"
  logs and starts fresh — never a dead gateway. Seeds/cwds install **only when
  the model restored** (against a fresh session their pane ids would collide
  with newly allocated ones).
- **Debounced model save** (500ms): hooked into the three mutation sinks —
  `applyModel`, `BroadcastLayout`, `BroadcastPaneTitle` — plus `pane_cwd`
  daemon events. `saveNow` merges live rt.cwd over not-yet-respawned restored
  cwds so back-to-back restarts don't lose them.
- **Seed consumption in `createPane`**: attaches `cp.InitialHistory` +
  `cp.Cwd` and deletes them **only on a connected send** — a pre-connection
  create is dropped by `daemon.send` and retried by reconcile, which must still
  find them. `reconcile` deletes seeds/cwds for **adopted survivors** (the
  daemon's live scrollback/cwd beats the stale seed).
- **Periodic capture sweep** (60s, own goroutine posting onto the loop):
  activity-gated via `paneRuntime.histDirty` (set on every `pane_frame`),
  issues `request_text(ansi, recent, history_lines)` through the existing
  pending machinery (`histResponder`; FIFO per (pane, kind) keeps it safe
  alongside waiter seeds and user captures). Bounds seed staleness for the
  daemon-crash case. History writes debounce 1s and prune to model panes.
- **Clean-shutdown final capture**: `Shutdown` (server.stop *or* signal) now
  broadcasts, saves the model, runs a capture of every live pane bounded by a
  1s deadline (`finalCapture` countdown + timer), writes history, then fires
  the stop hook.
- **Signal handling**: SIGINT/SIGTERM route through the same graceful path; a
  second signal force-quits. **Gotcha found live:** rweb's `Run()` installs its
  own SIGINT/SIGTERM handler and returns nil on signal — the old
  `log.Fatal(s.Run())` exited instantly, racing (and beating) the graceful
  path. Fix: check `Run`'s error, then `select {}` and let the signal
  goroutine drive the exit.
- **Config/flags**: new `persistence:` block `{enabled: true, state_dir: "",
  history_lines: 2000}` (0 = whole buffer; negative rejected), flags
  `--persist` / `--state-dir`, precedence flag > config > default as before.

## Verification

- **Unit** (all green untagged + `-tags ghostty -race`): `layout/persist_test.go`
  (round-trip incl. geometry, corrupt-node validation, ratio clamp, id
  reservation); `workspace/persist_test.go` (round-trip incl. burned pane
  number NOT reused after restore, inconsistency rejection);
  `app/persist_test.go` (2-workspace session round-trip: panes, focus, public
  handles, names; new ids never collide; corruption rejection);
  `persist/persist_test.go` (save/load, 0600, missing vs corrupt vs
  wrong-version, dir creation); `cmd/gateway2/persist_test.go` (restored orch
  serves identical viewport; `createPane` consumes seed+cwd over a `net.Pipe`
  daemon exactly once, keeps them when disconnected; debounced save lands a
  restorable file; final capture sweep requests ANSI, writes history, then
  fires done; activity gating).
- **Live e2e** (`/tmp/herdr-live`, persistent termhost + gateway2 `--auth
  none --state-dir …` + herdrctl + wsprobe2):
  - Built: h+v splits, 2 tabs, pane rename "builder", tab rename, ws rename,
    markers typed into panes 1 and 3. `session.json` live-updated (0600).
  - **Warm restart** (SIGTERM gateway2, daemon survives): restored "1
    workspaces, 4 panes" logged; panes/names/focus/tabs identical; pane 1
    contains pre-restart marker AND accepts a new command
    (`ALIVE_AFTER_RESTART`) → **adoption of the live shell**, not a respawn.
  - SIGTERM wrote `history.json` (34KB) via the final capture.
  - **Cold start** (daemon killed, fresh daemon + gateway2): model restored,
    shells re-spawned, `WARM_MARKER_A` / `ALIVE_AFTER_RESTART` /
    `WARM_MARKER_C` all visible in the fresh panes' scrollback (**seeded
    initial_history**), and the new shell answers `COLD_ALIVE`.

## Files

- **New:** `internal/layout/persist.go` (+test), `internal/workspace/persist.go`
  (+test), `internal/app/persist.go` (+test), `internal/persist/persist.go`
  (+test), `cmd/gateway2/persist.go` (+test).
- **Modified:** `internal/config/config.go` (+test), `cmd/gateway2/gateway.go`
  (orch fields, `newOrchWith`, createPane seeds, save hooks, Shutdown),
  `cmd/gateway2/daemon.go` (histDirty, cwd save hook, survivor seed-drop),
  `cmd/gateway2/main.go` (buildOrch restore, flags, signals, rweb-Run fix).

## Notes / leftovers

- **Viewport area is not persisted** — a restart pre-browser spawns at the
  120x32 default and resizes on connect. Cosmetic; worth folding in if resize
  flapping ever matters.
- **Seed-consumption edge:** a daemon that dies *between* accepting a create
  and the next reconcile loses that pane's seed (consumed on the connected
  send; no create-ack in β to confirm against). Accepted.
- The gateway does not re-emulate seeded history — `initial_history` replays
  through the daemon's emulator, so rewrap on later resize behaves exactly like
  herdr's `seed_history_ansi`.
- Pre-existing cosmetics unchanged: `http://localhost127.0.0.1:8431`
  double-prefix log line.
- WS3 termhost-side (persistent daemon, reconcile, `InitialHistory` handling)
  was already done (2026-06-24, `termhost-persistence-design.md` 3b) — this
  session closed the gateway2 side. Per the workstream map, remaining next
  candidates: WS8 polish (dialogs/menus/palette), WS5 manifest-update fetcher,
  WS6 notifications, then WS11 cutover.
