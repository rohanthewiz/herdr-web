# WS0 Stages A+B — termhost default + ghostty-free input mirror (Rust)

**Session id:** `2c92c617-b9d1-457a-8328-0465082a4d15` (same session as WS1)
**Date:** 2026-0702-0024 · **Repos:** `~/projs/rust/herdr` (branch `roh/phase-b-termhost-client`)
+ this repo (docs). **Continues:** `2026-0701-2310-ws1-layout-workspace-port.md`.

> **Implementation session (Rust side).** Executed WS0 Stages A and B from
> `ai_docs/phase-c-ws0-ws1-tasks.md`. Stage C (deletes + ~150-test rewrite) is next.

---

## Commits

- herdr `2f267ef` **feat: termhost is the default terminal backend (WS0 stage A)**
- herdr `c789343` **feat: ghostty-free input mirror for termhost panes (WS0 stage B)**
- herdr-web `5c6d3d6` / `b1c40e9` — task-doc checkoffs for A and B.

## Stage A — termhost default (decisions RECORDED)

- `Cargo.toml`: `default = ["termhost"]` (`--no-default-features` = legacy in-process-only
  build until stages C/D).
- **A2 policy (user decided):** unreachable/unconfigured daemon = **hard error** at pane
  creation (`termhost::required_backend()` → `io::Error` with guidance). Escape hatch
  `HERDR_TERMHOST_INPROCESS=1` (transitional; dies with the in-process path at stage C).
- **Discovery (user decided):** `herdr-termhost` (or dev `termhost`) **sibling of the herdr
  executable**, then `herdr-termhost` on PATH; `HERDR_TERMHOST_BIN`/`_SOCKET` env override.
- Unit tests keep pre-flip in-process behavior via `cfg(test)` in `required_backend` until C6.
- **Verified:** e2e 12/12 vs live daemon in BOTH modes (hand-launched socket + managed) —
  note: those tests **silently SKIP-pass** when env unset; headless-server smokes confirmed
  discovery→spawn→connect (daemon survives herdr death), hard error at startup-workspace
  create, and the escape hatch spawning in-process. `target/debug/herdr-termhost` left in
  place so dev herdr discovers it with zero env setup.

## Stage B — types relocation + InputMirror

- **B1:** `src/terminal/types.rs` owns `FocusEvent`+`encode_focus` (now **pure** `CSI I`/`CSI O`
  — ghostty's `ghostty_focus_encode` FFI wrapper deleted) and `KittyImage*`/
  `KittyPlacementRenderInfo`. Consumers repointed; ghostty imports them back.
- **B2 (decision: small Rust encoder now; WS9 moves encoding to Go):** `PaneTerminal` is an
  enum — `Ghostty(GhosttyPaneTerminal)` in-process / `Mirror(InputMirror)` termhost; all ~26
  call sites unchanged; Mirror's buffer-reading methods return empty defaults (Go-backend arms
  answer first). **Termhost spawn constructs no Zig terminal at all.** New
  `src/pane/input_mirror.rs` = plain-data copy of Go-reported modes (`PaneSignal::Modes`) +
  pure `crate::input` encoders (pre-existed, was test-only) + pure `KittyKeyboardTracker`.
- **Differential parity test** (`pane::input_mirror::tests`) drives mirror vs ghostty-backed
  mirror over 45 mode/encoding/kitty-flag combos × key/mouse matrix. It forced 4 pure-encoder
  fixes: Esc→`CSI 27 u` under kitty; Shift+Tab→`CSI 9;2u`; DECCKM SS3 only under legacy
  protocol; no wheel reports in X10. New pure entry points:
  `encode_terminal_key_with_modes`, `encode_mouse_moved`.
- **Known divergence (documented in test):** kitty flags bits 2/8 (report-event-types /
  report-all-keys) degrade to legacy-compatible output until WS9.

## Test-suite state & the honest correction

- 1923 unit tests green; clippy clean; e2e 12/12 vs live daemon post-mirror.
- **Stage-A gap found while investigating "flakes":** post-flip, pane-creating integration
  tests hard-error without a discoverable daemon (masked locally by the sibling binary; CI
  would break). Fixed in stage-B commit: all 20 non-termhost integration spawn sites pin
  `HERDR_TERMHOST_INPROCESS=1` (C6 rewires them onto termhost).
- **Pre-existing environmental failures on this machine** (fail identically at `417b4b1` in
  isolation; all agent-detection-dependent): `api_ping::events_subscribe_streams_output_and_
  agent_status_events`, `cross_area_two_clients_shared_view_and_single_detach_stability`,
  `cross_area_agent_process_survives_detach_and_reattach`.

## Build gotchas (macOS 26.5)

- `cargo build` needs the pinned Zig: `ZIG=<herdr-web>/.tools/zig-wrapped cargo build`.
- Go daemon build: `PKG_CONFIG_PATH=~/projs/rust/herdr/vendor/libghostty-vt/zig-out/share/
  pkgconfig go build -tags ghostty ./cmd/termhost`.
- Unix socket paths must stay under ~104 bytes (deep tmp dirs fail with `bind: invalid
  argument`).

## Next: WS0 Stage C (then D)

- C1 drop `client_if_enabled()` guard / sole constructor `finish_termhost`; C2 delete
  `PaneRuntimeIo::Actor` + arms (convert `TestChannel` to the test double); C3 delete
  in-process read path + Rust detection task; C4 collapse ~15 accessors; C5 redefine
  `from_handoff_fd` (decision to record); **C6 test rewrite (~150+ emulator-bound tests) is
  the biggest cost** — `InputMirror::seed_*` hooks are already in place for C5.
