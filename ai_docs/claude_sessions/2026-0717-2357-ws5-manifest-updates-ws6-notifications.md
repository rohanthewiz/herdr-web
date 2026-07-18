# WS5 manifest-update fetcher + WS6 agent notifications

**Date:** 2026-0717-2357 · **Branch:** `roh/phase-b` (herdr-web)
**Continues:** `2026-0717-2257-ws3-session-persistence-restore.md` (same day; WS3
closed → the status doc's remaining candidates were WS8 polish, WS5 tail, WS6
notifications, WS11).

> Two workstream tails in one session. **WS5**: termhost now fetches
> agent-detection manifest updates from the live herdr.dev TOML catalog at
> startup and layers committed remote manifests over the embedded set — the
> port of `manifest_update.rs`. **WS6**: notification-worthy agent state
> transitions (blocked / background finish) now reach browsers as toasts +
> permission-gated **native Notifications** (click reveals the pane) and the
> control API as a new `pane_notify` event. Both live-verified: WS5 against the
> real catalog (17 manifests; unknown agents `devin`/`maki` skipped), WS6 with
> a fake `claude` script driving the real detection pipeline end-to-end.

---

## WS5 — remote agent-detection manifests (commit `6b14635`)

**Where it runs:** detection lives in the termhost daemon (`orchestration/host.go`
→ `detect.Detect`), so the overlay + fetcher live there, not in gateway2.

**Format decision:** the catalog at
`https://herdr.dev/agent-detection/index.toml` is **live and TOML** (fetched
during the session), and Rust herdr installs consume it — so the Go port speaks
the same format via `pelletier/go-toml/v2` (new dep). The YAML-over-TOML
preference is about herdr's own config; this is a hosted wire format. The Go
raw manifest structs carry `toml` tags alongside `json` (the embedded JSON
manifests are 1:1 conversions of the Rust TOML), and pelletier v2 flattens the
embedded `rawGate` exactly like `encoding/json`.

**`internal/detect/manifest.go`:** the store went from `sync.Once` to
RWMutex + invalidation (`SetRemoteManifestDir`, `Reload`, `ensureManifests`;
`Detect` takes one RLock on the hot path). `loadManifests(remoteRoot)` builds
embedded-then-overlay; a broken remote file logs and **falls back to the
bundled manifest** — never a missing agent. Raw structs gained
`Version`/`MinEngineVersion`/`UpdatedAt`/`Aliases`/rule `ID`.

**`internal/detect/update.go`** (port of `manifest_update.rs`):
- `CheckAndUpdate(stateDir, url)`: fetch catalog → per-agent manifest fetch →
  `processAgentManifest` → status. Per-agent failures are recorded, not fatal.
- Strict TOML decode (`DisallowUnknownFields`, like serde's
  `deny_unknown_fields`); id must match; version + `min_engine_version`
  required; **`EngineVersion = 2`** gate; complexity limits for untrusted
  manifests (128 rules, gate depth 8, 512 gates, 1024 matchers, 512 chars).
- Versions are dotted-numeric with **trailing-zero equality** ("1.2" ==
  "1.2.0"); downgrades and same-version content changes are rejected as
  tampering; same-version-same-content is a silent no-op.
- Fetch via `net/http` (replacing Rust's curl exec): 256 KB cap, 15s budget,
  2 retries, UTF-8 check. `httptest` replaces Rust's `file://` in tests.
- Commits are atomic (same-dir temp + fsync + rename) to
  `<state>/agent-detection/remote/<agent>.toml` — the **same layout as Rust
  herdr, so the cache is shared** across the transition (observed live: the
  daemon reported "up to date" against manifests a prior Rust run had
  committed). Status goes to `status.json` (Go writes no TOML; Rust's
  `status.toml` coexists untouched).
- `AutoUpdate` wraps it for the daemon: env URL override
  (`HERDR_AGENT_DETECTION_MANIFEST_CATALOG_URL`), reload-on-change, logging.

**termhost:** at startup, `SetRemoteManifestDir(persist.DefaultDir() +
"/agent-detection")` + `go detect.AutoUpdate(...)` behind a new
`-manifest-update` flag (default true).

## WS6 — agent notifications (commit `c3144dc`)

**Scope finding:** herdr notifies on agent **state transitions** only
(`app/actions.rs`); a child's OSC 9 is detection input, never a notification —
so **no β protocol change was needed** (gateway2 already receives `pane_agent`).
The `browserproto.Notify` message had existed since WS9 with a stub front-end
handler and **zero server call sites** — this session wired it.

**Classification (`notifyKind`, ported):** requires a detected agent and an
actual state change (dedupes termhost's repeated re-emissions — it re-emits
`blocked` every detection tick, observed live). Any change into `blocked` ⇒
`"attention"`; into `idle` from `working`/`blocked`, or from `unknown` with the
same agent label ⇒ `"finished"`.

**Emission (`cmd/gateway2/notify.go`):** the `MsgPaneAgent` dispatch closure
became `onPaneAgent` (chrome caching unchanged) + notify: message
"`<agent> needs attention`/`finished`", body = herdr's context string
("workspace · N [· tab]"), plus `Pane` + `Pub` (new Notify fields) for
click-to-reveal. Also emits `pane_notify` (new `events.subscribe` vocabulary
entry, `app.PaneNotifyEvent`).

**Suppression is client-side by design:** herdr knows its single host
terminal's focus; the gateway serves many browsers, each knowing its own
focus/visibility — so the server always sends and each front-end applies
herdr's rule (suppress when `document.hasFocus()` and the pane is in the
viewport `panes` map). Otherwise: toast; and when unfocused, a native
`Notification` (tagged per pane to coalesce) whose click focuses the window
and sends `agent.focus` (crosses workspaces/tabs). Permission is requested
once on the first pointer/key gesture.

## Verification

- **Unit** (full suite green, untagged + `-tags ghostty -race`):
  `detect/update_test.go` (version compare incl. trailing zeros, bad-segment
  rejection, commit/downgrade/tamper/no-op matrix, remote-manifest validation
  incl. unknown-field + engine gates, catalog validation incl. unknown-agent
  skip, httptest end-to-end where the committed overlay drives `Detect` after
  reload, broken-overlay fallback); `gateway2/notify_test.go` (classification
  table; orch-level notify with pane/pub/context + `pane_notify` + same-state
  dedupe; finished path).
- **Live WS5:** one-off probe committed **17 manifests from the real
  herdr.dev catalog** (unknown `devin`/`maki` skipped); the persistent termhost
  then logged "agent manifests up to date" through the shared state cache.
- **Live WS6:** fake agent — a shell script at `bin/claude` (procscan
  identifies via argv) printing `claude.json`'s `dynamic_workflow_prompt`
  blocked markers ("run a dynamic workflow?" / "esc to cancel") — drove the
  **real** pipeline: `pane_agent claude idle → blocked` produced exactly one
  `pane_notify {kind:attention}` in `herdrctl events`, repeated blocked
  re-emissions never re-notified; a second pane's agent produced its own; a
  connected wsprobe2 tallied `*browserproto.Notify:1`.

## Files

- **WS5:** `internal/detect/manifest.go`, **new** `internal/detect/update.go`
  (+`update_test.go`), `internal/detect/manifest_test.go` (store API),
  `cmd/termhost/main.go`, `go.mod`/`go.sum` (+`pelletier/go-toml/v2`).
- **WS6:** **new** `cmd/gateway2/notify.go` (+`notify_test.go`),
  `cmd/gateway2/daemon.go`, `internal/app/events.go`,
  `internal/browserproto/down.go`, `cmd/gateway2/web/index.html`.

## Notes / leftovers

- **termhost re-emits `pane_agent`** with an unchanged state every detection
  tick (seen live: a stream of identical `blocked` events). Harmless (notify
  dedupes; browsers just re-render), but worth a change-gate in
  `host.go`/`detectstate.go` someday to quiet the event stream.
- **WS6 not ported:** sounds (herdr's afplay Request/Done — browser autoplay
  policies make this a deliberate skip) and herdr's optional toast
  **delay/pending machinery** (`toast_config.delay_seconds` debounce) —
  termhost's detection stabilization already damps flapping.
- **WS5 not ported:** herdr's **local manifest override** layer
  (`local_override_shadowing_remote` — a user's own manifest shadowing both),
  periodic re-fetch (Rust runs once per app start; termhost is long-lived, so
  a daily timer might be worth adding), and surfacing update status in a UI
  (Rust shows it in settings; `status.json` + logs only for now).
- Manifest catalog lists agents newer than the embedded set (`devin`, `maki`)
  — adding an agent still requires shipping an embedded manifest + procscan
  label (by design: the embedded set defines "known").
- Per the workstream map, what remains before WS11 cutover: **WS8 polish**
  (dialogs/menus/command-palette + remaining chrome) and **WS7** (shell-hook
  installers), plus the small leftovers above.
