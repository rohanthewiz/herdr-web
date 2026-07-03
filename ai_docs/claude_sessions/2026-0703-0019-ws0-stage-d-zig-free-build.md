# WS0 Stage D — Zig/ghostty build dropped; WS0 COMPLETE (Rust)

**Session id:** `e236bf82-bc46-4189-bdb8-09ce7198a60f` (same session as Stage C, name
`stage-c-impl-verif`; this doc covers the Stage D continuation)
**Date:** 2026-0703-0019 · **Repos:** `~/projs/rust/herdr` (branch `roh/phase-b-termhost-client`)
+ this repo (branch `roh/phase-b`: docs only).
**Continues:** `2026-0702-2351-stage-c-impl-verif.md`.

> **Implementation session.** Executed WS0 Stage D (D1–D4) from
> `ai_docs/phase-c-ws0-ws1-tasks.md` (checked off there, commit `f386371`).
> **WS0 (stages A–D) is complete**: `cargo build` needs no Zig toolchain and never links
> libghostty-vt; the Go termhost daemon is the sole terminal backend.

---

## Commits

- herdr `4bc4d39` **feat: drop the Zig/ghostty build — herdr is pure Rust over the Go daemon
  (WS0 stage D)** — 24 files, **−7,851 lines**.
- herdr-web `f386371` docs checkoff (task doc marks WS0 complete).

## What Stage D did

- **D1:** deleted `src/pty/` (actor, backend, fd) and `src/ghostty/` (FFI bindings) + their
  `mod` decls in `main.rs`; also the PTY-era `foreground_process_group_id_for_tty_fd` helpers
  in `platform/{macos,linux}.rs`. Stage C had already removed every external reference — the
  deletes were reference-clean on the first compile. `src/pane/terminal.rs` **survives**
  (contrary to the original D1 line, written pre-C): it now holds only the shared plain-data
  types (`InputState`, `ScrollMetrics`, `TerminalCursorState`, `TerminalDirtyPatch*`,
  test-only `ProcessBytesResult`) and the Mirror/Fake dispatch.
- **D2:** `portable-pty` moved to **[dev-dependencies]** (a new section — tests drive herdr and
  fake agents under real PTYs; `detect/mod.rs` unit tests also use it). Prod uses the new
  **`src/pane/command.rs::CommandBuilder`** — a plain launch description with exactly the
  API subset the code used: `new`/`new_default_prog` (empty argv = user's default shell,
  resolved daemon-side from the `SHELL` env we always set in login mode), `arg`, `cwd`, `env`,
  `get_argv`, `get_cwd`, `get_env` (tests), `iter_extra_env_as_str`, `is_default_prog`.
  `finish_termhost` serializes it into the Go `PaneSpec`. The `termhost` cargo feature and the
  `[features]` block are gone; ~40 `#[cfg(feature = "termhost")]` gates collapsed (incl. the
  `#[cfg(not(feature))]` no-backend error arm in `spawn_command_builder` and the
  `cfg_attr(not(any(test, feature…)))` allows in `protocol/wire.rs`).
- **D3:** `build.rs` reduced from the ~100-line Zig orchestration (zig build -Demit-lib-vt +
  link directives + vendor rerun-if-changed) to just `rerun-if-env-changed` for
  `HERDR_BUILD_{CHANNEL,ID,COMMIT}` (consumed via `option_env!` in `src/build_info.rs`).
  **`vendor/libghostty-vt` stays in-tree** — it is the source herdr-web builds the Go daemon's
  CGO VT engine from (`PKG_CONFIG_PATH=<herdr>/vendor/libghostty-vt/zig-out/share/pkgconfig
  go build -tags ghostty ./cmd/termhost`); the Rust build never touches it.

## Post-delete warning sweep (removing the stale `#![allow]` on `src/termhost/mod.rs` exposed these)

- Test builds cfg out the real spawn tail → allow-rooted `required_client` (mod.rs) and
  `claim_surviving_pane` (client.rs) with `#[cfg_attr(test, allow(dead_code))]` so the whole
  connect/spawn/discovery chain stays dead-code-clean in both build flavors.
- `TerminalBackend::latest_frame` deleted from the trait + impl (`snapshot()` is the real API).
- Wire-compat allows with notes: `proto::Frame.full` (client fold is uniform — full frames just
  never set `skip`), `proto::Welcome.protocol_version`.
- `KittyImageFormat::{Rgb,Png}` allow-noted: unconstructed until kitty graphics flow over the
  seam (in-process producer deleted).
- `tests/termhost_e2e.rs` lost its `#![cfg(feature = "termhost")]` gate.

## D4 acceptance (all with `env -u ZIG` and a zig-free PATH)

- `cargo build --release`: no Zig invocation, no libghostty-vt link; `nm`/`otool` show **zero**
  ghostty symbols in the binary.
- `cargo tree -e normal`: no `portable-pty` (present only as dev-dep in the full tree).
- Unit suite **1701/1701** — down exactly 35 from Stage C's 1736 (the deleted pty actor +
  ghostty module tests).
- `termhost_e2e` **12/12** vs a live daemon (socket + managed modes in one invocation, no skips).
- live_handoff 16/16, client_mode 16/16, server_headless 15/15, detach_reattach 11/11;
  api_ping/multi_client/cross_area show only the **4 pre-existing machine-environmental
  failures** (unchanged; see the Stage C session doc for the baseline verification).
- `cargo check`/`cargo check --tests` are 0-warning; clippy has only the pre-existing
  `actions.rs:2722` lint.

## Notes for later workstreams

- Transitional `allow(dead_code)` markers still standing (by design): detect/process-probe
  helpers ("delete with the detect-port workstream"), `terminal_theme::
  osc_set_default_color_sequence`, `terminal/state.rs::stabilize_agent_detection`,
  `handoff_runtime.rs` (fd-manifest compat for older binaries).
- The `ZIG=<herdr-web>/.tools/zig-wrapped` env is no longer needed for ANY Rust build/test —
  only for rebuilding the vendored libghostty-vt that the **Go daemon** links.
- Integration tests hard-require `target/debug/herdr-termhost` (see
  `tests/support::termhost_daemon_bin()`); a build from herdr-web `21f65ce` is in place.
- WS0 + WS1 are both complete. Next per `fbl_go_port_feasibility_analysis.md` Part 2: the
  later workstreams (app state/rendering ports; WS9 moves key encoding to Go, which retires
  the InputMirror's known kitty bits-2/8 degradation and the Rust XTMODKEYS Enter encoding).
