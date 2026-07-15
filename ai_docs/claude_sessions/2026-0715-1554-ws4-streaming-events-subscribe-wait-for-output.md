# WS4 streaming methods: events.subscribe + pane.wait_for_output

**Session id:** `ad19a7b0-bb99-4903-be3c-3148a93199c8`
**Date:** 2026-0715-1554 · **Branch:** `roh/phase-b` (herdr-web)
**Continues:** `2026-0715-1454-ws4-query-methods-herdrctl-verbs-yaml-config.md`, whose "still open
from WS4" list named streaming as "the biggest remaining item: needs a new envelope on ctlproto."

> Both streaming control-API methods, in one pass, committed together. The key move was
> recognising the two methods are **different shapes**, so only one needed a new envelope:
> **`pane.wait_for_output`** rides the *existing* unary request/response envelope (a slow
> reply, like read/capture) and only needed a longer server backstop; **`events.subscribe`** is
> the sole streaming method (ack Response, then a stream of Event frames until the client
> disconnects). Unit-tested at every layer (untagged + ghostty) and live-verified end-to-end
> against a real gateway2 + persistent termhost with a headless WS input injector.

Decisions confirmed up front (AskUserQuestion): **wait_for_output matches captured screen text**
(screen-scrape via the existing capture round-trip — no termhost/protocol change) over raw-byte
streaming; **events set = pane_exited + pane_agent + pane_title + pane_cwd** (the pane
lifecycle/chrome the orchestrator already sees), deferring structural events (added/removed/focus).

---

## The architecture split

Three method kinds over the newline-framed ctlproto transport:

- **Unary** (all existing §7 commands + ping): one Request → one Response → close. Unchanged.
- **Await** (`pane.wait_for_output`): still one Request → one Response, only the response is
  delayed until the pattern matches (or the wait times out). **Rides the unchanged envelope.**
  It is a real §7 command (in `app.CommandNames()`, routed through `app.Dispatcher`), async like
  read/capture — the server just grants it a longer backstop.
- **Stream** (`events.subscribe`): server writes an ack Response, then zero-or-more Event frames
  on the same connection until the client disconnects. The **one** method needing the new layer.
  It is a transport method (like `ping`), deliberately **not** in `app.CommandNames()` / not
  routed through `Dispatcher`.

## `pane.wait_for_output` (unary/await path)

- **app** (`command_vocab.go`): `CmdWaitForOutput = "pane.wait_for_output"` added to the table +
  `CommandNames()`. `WaitForOutputParams{Pane, Pattern, Regex, TimeoutMs, Lines}` and
  `WaitForOutputResult{Matched, Text}`. `Matcher()` compiles a substring or regexp predicate that
  returns the **matched line** (for the result's context Text) and validates the pattern (empty /
  bad regex → error the dispatcher reports as bad params before registering). `WaitTimeout(ms)` +
  `MaxWaitTimeout=10m` / default 30s bounds shared by backend and any client sizing its deadline.
  Dispatcher case mirrors read/capture: no-reply short-circuit, matcher-validate, PaneExists +
  DaemonConnected gates, then `Backend.StartWaitForOutput`.
- **gateway2 waiter machinery** (`gateway.go`): there is **no raw-output stream** from the daemon
  (orch holds an unfed local emulator), so a waiter re-scans the pane's **captured recent text**.
  Registration kicks off one capture-check (catches already-present output); each subsequent
  **frame** for the pane triggers another, **coalesced to one round-trip in flight** via a per-pane
  `waiterCheck` flag (a burst of 60 Hz frames → at most one capture, re-triggered by the next
  frame). The check reuses the **existing pending/reqText machinery** — a `waiterResponder` whose
  `OK` matches the captured text against each waiter (`onWaiterText`), whose `Fail` just clears the
  in-flight flag so the next frame retries. Resolution: match → `Matched:true` + line; own timer or
  pane-exit → `Matched:false`; daemon drop → `Fail` (infra error). Frame-trigger is
  **not** viewport-gated (waiters observe off-screen panes). Hooks in `daemon.go`:
  `triggerWaiterCheck` on MsgPaneFrame, `resolveWaitersOnExit` on MsgPaneExited, `flushWaiters`
  alongside `flushPending` on disconnect.
- **ctlproto server**: `awaitBackstop = app.MaxWaitTimeout + 15s` for `CmdWaitForOutput` in
  `dispatchAndWait` (sits above the waiter's own clamp so the waiter always resolves first).
- **herdrctl**: `wait <pane> <pattern> [timeout_secs]` verb (fractional secs → ms); `main.go`
  auto-sizes the round-trip deadline to `WaitTimeout + 10s` unless `--timeout` is already larger.
- **Known edge (documented in code):** output that appears *only* in a pane's final frame at exit
  can be missed — the post-exit capture can't reach the torn-down pane → resolves `Matched:false`.

## `events.subscribe` (streaming path)

- **ctlproto** (`proto.go` + new `stream.go`): `Event{Name, Data}` frame; `MethodEventsSubscribe`.
  `Subscriber` seam (`Send(event, data) bool`, non-blocking — a full buffer drops the sub, like the
  browser slow-connection drop); `StreamDispatch(method, params, sub) (cancel, err)`; a per-conn
  buffered sink (`streamBuffer=128`) with a pump goroutine draining to the socket and a background
  `ReadByte` watching for client disconnect. `handleConn` routes subscribe → `handleStream` (ack,
  then pump until disconnect, `defer cancel()`); non-stream requests unchanged. Client `Subscribe`
  dials, sends, reads the ack, loops `readEvent` → callback until EOF / ctx-cancel / callback error.
- **app** (new `events.go`): event name consts (`EventPaneExited/Agent/Title/Cwd`) + `EventNames()`,
  payload structs (`PaneExitedEvent{Pane, ExitCode}`, etc. — Pane is the **internal** id every §7
  command addresses by), and `EventsSubscribeParams{Pane *uint32, Events []string}` with a `Match`
  filter (absent pane = all panes, empty events = all names).
- **gateway2** (`control.go` + `gateway.go`): `controlStream` decodes the filter, registers a
  `ctlSubscriber{sub, filter}` on the loop, returns a cancel that removes it; wired via
  `srv.SetStreamDispatch`. `emitEvent(name, pane, data)` fans out to filter-matching subs on the
  loop goroutine, dropping any whose `Send` returns false. Emission hooked into the four daemon
  dispatch cases (exit/agent/title/cwd).
- **herdrctl**: `events [pane]` verb; `main.go` streaming branch (`runEvents` via `ctlproto.Subscribe`
  under `signal.NotifyContext(os.Interrupt)`) prints one JSON line per event until Ctrl-C. The
  unknown-command guard now also allows `MethodEventsSubscribe` (like ping) so the raw
  `events.subscribe --params '{"events":[…]}'` path reaches a custom filter.

## Verification

- **Unit** (all green, untagged + `-tags ghostty`): `ctlproto/stream_test.go` (Event round-trip,
  subscribe ack→events→cancel, rejection, not-supported, slow-reader drop); `app/commands_test.go`
  (wait no-reply / empty-pattern / bad-regex / unknown-pane / daemon-down / starts, + Matcher
  substring+regex+line-extraction); `gateway2/waiter_test.go` (match / no-match-keeps-waiting /
  multi-per-pane / exit-resolves-false / flush-fails, + emitEvent filter & slow-sub drop);
  `herdrctl/subcommands_test.go` (buildWait/buildEvents + registry-integrity streaming allowance).
- **Live** (real gateway2 `-auth none` + persistent termhost + herdrctl + a stdlib WS injector in
  the scratchpad that does the RFC6455 handshake, sends init v1, then Raw keystrokes):
  - `wait 1 ZZZ_NEVER 3` → `{"matched":false}` at **3.04s** (real capture round-trips, no match).
  - `wait 1 WAITMARKER42 20` + inject `echo WAITMARKER42` → `{"matched":true,"text":"…>echo
    WAITMARKER42"}`.
  - `events 1` + inject `printf '\033]2;LIVETITLE\007'` then `exit` →
    `{"event":"pane_title","data":{"pane":1,"title":"LIVETITLE"}}` then
    `{"event":"pane_exited","data":{"pane":1,"exit_code":0}}`; Ctrl-C exits the subscriber cleanly.
  - `server.stop` → gateway2 stops, termhost survives.

## Files

- **New:** `internal/app/events.go`, `internal/ctlproto/stream.go` (+`stream_test.go`),
  `cmd/gateway2/waiter_test.go`.
- **Modified:** `internal/app/command_vocab.go`, `internal/app/commands.go`
  (+`commands_test.go`), `internal/browserproto/cmd.go`, `internal/ctlproto/proto.go`,
  `internal/ctlproto/server.go`, `cmd/gateway2/gateway.go`, `cmd/gateway2/control.go`,
  `cmd/gateway2/daemon.go`, `cmd/herdrctl/main.go`, `cmd/herdrctl/subcommands.go`
  (+`subcommands_test.go`). ~659 insertions.

## Notes / leftovers

- **Deferred (as scoped):** structural events (`pane_added`/`pane_removed`/`focus_changed`) derived
  by diffing the model on `applyModel`; **raw-byte-stream** matching for wait_for_output (a
  `pane_output` orchestration event → protocol bump + termhost readPump changes) — would close the
  final-frame-at-exit edge and never miss fast-scrolling transient output.
- **Pre-existing cosmetic (not this session):** the `serving at http://localhost127.0.0.1:8477`
  log line double-prefixes host when config supplies a full `host:port` addr.
- **Repeatable browser-driven check** (carried from prior sessions): still worth capturing the
  ad-hoc harness as a project `/run` skill; the scratchpad WS injector built here (init v1 +
  Raw keystrokes) is a reusable seed for a headless input-injection helper.
