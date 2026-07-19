# Packaging + CI: vendored libghostty-vt, Makefile, GitHub Actions

**Session id:** `58a89454-a80f-434c-9d2f-187db971ebbb` (same session as the
agent-resume doc — continued directly)
**Date:** 2026-0718-2032 · **Branch:** `roh/phase-b` (herdr-web)
**Continues:** `2026-0718-2002-agent-session-persistence-resume-on-restore.md`
(WS11's packaging/CI slice; cutover/delete-Rust remains).

> The repo is now **self-contained and CI-covered**: libghostty-vt's Zig
> source is vendored in `third_party/`, a portable build script + Makefile
> drive everything, and GitHub Actions runs untagged checks plus
> ghostty-tagged race tests on **ubuntu and macos — all green** (first-ever
> Linux run; it immediately caught a real dash-vs-bash test bug). A release
> workflow ships per-platform tarballs on `v*` tags (unexercised until the
> first tag).

---

## Vendoring (commit `7a6393d`)

- `third_party/libghostty-vt/` — copied from the Rust repo's
  `vendor/libghostty-vt` (upstream ghostty `0f7cd84b`, dist 1.3.2-HEAD,
  ~23M/1255 files; provenance kept in `third_party/libghostty-vt.vendor.json`).
  Survives the Rust repo's retirement — no Rust checkout needed to build.
- **`vendor/` is unusable** — the Go toolchain treats a repo-root `vendor/`
  as module vendoring and breaks every build. Hence `third_party/`.
- Build outputs gitignored (`zig-out/`, `.zig-cache/`): the generated
  pkgconfig **bakes an absolute prefix at build time**, so every checkout
  (and every CI runner path) must build in place.
- **Hermetic `--system zig-pkg` mode abandoned**: Zig executes every
  `lazyDependency()` call at configure time regardless of the requested
  step, so offline mode demands imgui, fonts, then snowballs into vaxis/
  libxev/zig_objc/… — most of ghostty's dep tree. Kept the vendor tree
  byte-identical to herdr's (zig-pkg = uucode only); the few lazy deps fetch
  from deps.files.ghostty.org into the Zig global cache, which CI caches.

## Build script + Makefile (commit `0a67cd2`)

- `scripts/build-libghostty-vt.sh` rewritten: OS/arch dispatch
  (macOS arm64/x86_64, Linux x86_64/aarch64) with pinned **Zig 0.15.2**
  per-platform sha256s; defaults to the in-repo `third_party` tree
  (`GHOSTTY_VT_DIR` still overrides); macOS-26 SDK arm64-slice patch + xcrun
  shim kept but darwin-only (idempotent/harmless on older SDKs — passed on
  the GH macos runner); **clean exit now required** —
  `-Demit-exe=false -Demit-xcframework=false` skips the xcframework step the
  old script had to tolerate failing (flag tip found in the vendored tree's
  own CLAUDE.md).
- `Makefile`: `vt` · `build`/`test` (untagged) · `build-ghostty`/
  `test-ghostty`/`race-ghostty`/`vet-ghostty` (PKG_CONFIG_PATH wired) ·
  `binaries` (gateway2/termhost/herdrctl → bin/, `-trimpath`) · `dist`
  (versioned `git describe` tarball + config.example.yaml + README) ·
  `fmt-check` (cmd+internal only) · `check` = the CI sequence.
- `config.example.yaml` synced to current schema: `server.hook_socket`,
  `persistence` (incl. `resume_agents`), `worktrees`. New
  `TestExampleConfigParses` drift-guard asserts it parses AND matches
  `Default()` for the scalar sections.
- README: new Build & packaging section; stale Rust-repo PKG_CONFIG path
  fixed.

## CI (`.github/workflows/ci.yml`)

- `quick` (ubuntu): fmt-check, vet, build, test — fast untagged signal.
- `ghostty` matrix (ubuntu-latest, macos-latest): two caches —
  (1) `.tools` + `~/.cache/zig` keyed on script+build.zig.zon,
  (2) `zig-out` + `.zig-cache` keyed on the vendored source hash — then
  `make vt`, `vet-ghostty`, `race-ghostty`.
- Concurrency group cancels superseded runs; `v*` tags excluded (release's
  job).

## release.yml

On `v*` tags: matrix (ubuntu-latest, ubuntu-24.04-arm, macos-latest) →
`make vt` → `test-ghostty` → `dist` → softprops/action-gh-release attaches
the tarballs. CGO forbids cross-compiling, hence per-platform runners.
**Unexercised until the first tag is pushed.**

## First Linux run caught a real bug (commit `3261c84`)

`TestHostReportsAgent` / `TestHostReportsAgentWorkingState` used
`exec -a codex …` under `cp.Command = "/bin/sh"` — a bashism; Ubuntu's
/bin/sh is **dash**, so the fake agent never spawned and both tests timed
out (15 s ReadMessage). Fixed: `/bin/bash` explicitly. (Linux detection
itself is fine: `identifyPidLinux` reads /proc cmdline argv, catching the
`exec -a` argv[0].) All three CI jobs green after the fix (run 29668540859).

## Gotchas / techniques

- Shell working directory **persists between tool calls** — two mishaps this
  session (mkdir'd third_party inside the vendored tree; ran go test from
  inside it). cd to repo root explicitly in compound commands.
- CI log download needs auth: token via `git credential fill` (osxkeychain),
  `Authorization: Bearer` against the jobs/logs API. No gh CLI installed.
- zsh: `status` is a read-only variable — broke a polling loop; use `st`.
- Moving the built tree invalidates the .pc (absolute prefix) → rebuild.

## Files

- **New:** `third_party/libghostty-vt/` (+`.vendor.json`), `Makefile`,
  `.github/workflows/ci.yml`, `.github/workflows/release.yml`.
- scripts/build-libghostty-vt.sh (rewrite), .gitignore (zig-out/.zig-cache),
  config.example.yaml, internal/config/config_test.go (drift guard),
  internal/orchestration/host_test.go (bash fix), README.md.

## Notes / leftovers

- release.yml untested — push a `v0.x.y` tag to exercise it.
- Old `cmd/gateway` (Phase A) still builds in CI via `go build ./...`;
  candidates for deletion at cutover.
- Per the workstream map, WS11 remainder: **the cutover itself**
  (default-on, delete Rust) + deferred niche chrome (global launcher, pane
  drag-reorder, bell/activity markers, onboarding). Also still open from
  last session: resumed pane exits with its agent (no respawn-shell), dedupe
  winner by pane id not traversal order.
