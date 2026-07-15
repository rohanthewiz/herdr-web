# WS4 leftovers: read-only query methods + ergonomic herdrctl verbs + YAML config

**Session id:** `a4df7492-3d93-4b98-be5b-0bbd93b249b9`
**Date:** 2026-0715-1454 · **Branch:** `roh/phase-b` (herdr-web)
**Continues:** `2026-0715-1037-live-test-browser-copymode-two-pane-doc-fix.md`, whose "still open
from WS4" list drove the whole session. Worked three leftovers end-to-end, each committed after a
live verification pass.

> Three self-contained features, three commits. **(1)** Read-only §7 query commands
> (`session.get`, `*.list`, `pane.get`) + a `.gitignore` for the built binaries. **(2)** An
> ergonomic positional-argument verb layer for `herdrctl` (`split h 2`, `focus 1`, `panes`, …) over
> the raw `<method> --params` escape hatch. **(3)** A YAML config file for gateway2 — server
> settings (flag > config > default), theme colours, and copy-mode keybindings — making
> `server.reload_config` real. Every feature has unit tests **and** a live end-to-end run against a
> real gateway2 + persistent termhost.

User preference captured: **YAML over TOML** for herdr config (memory `pref-yaml-over-toml`).

---

## Commit 1 — `967ca6d` read-only query commands + ignore built binaries

**`.gitignore`:** added patterns for the built `gateway2`/`herdrctl`/`termhost` binaries (root and
`cmd/*/`); untracked the committed 8.5 MB root `termhost` (`git rm --cached`, file kept on disk).

**Query commands** (five, added to the protocol-neutral §7 table so both the browser and the
control-API / `herdrctl` reach them with no per-front-end code):

| Command | Returns |
|---|---|
| `session.get` | active workspace, focused-pane handle, workspace/pane counts, cwd |
| `workspace.list` | every workspace (id, name, active, tab count) |
| `tab.list` | tabs of a workspace (`workspace` optional → active); num/name/active/zoomed/pane count |
| `pane.list` | every pane across all workspaces/tabs (internal `pane` id + public `handle`, name, focused-within-tab, viewport-visible) |
| `pane.get` | one pane (optional target → focused) |

- Queries are **synchronous, read-only** — answered straight from `app.Session`, **no** `Backend`
  effect or async round-trip. Assembly in a new `internal/app/query.go` (keeps `session.go` free of
  the command vocabulary); result/param structs in `command_vocab.go`.
- `pane.list` returns **both** the internal addressing id and the public `w1:p3` handle, so a caller
  lists then acts.
- Re-exported the new **command-name constants** into `browserproto` for vocabulary uniformity, but
  **not** the result structs (browserproto already has its own `WorkspaceInfo`/`TabInfo` for layout
  down-messages — the query results belong to the control-API path).
- `herdrctl` picked them up automatically (validates against `app.CommandNames()`, pretty-prints the
  JSON `Data`).

**Verified:** 5 new unit tests (each also asserts the query drove no backend effect) + a live run —
all five queries on a single-pane session, then split/tab/workspace mutations tracked correctly
(per-tab focused, viewport-only visible), both error paths (`unknown workspace`, `no such pane`)
returned exit 1, torn down via `herdrctl server.stop` (termhost survived).

## Commit 2 — `1f80054` ergonomic herdrctl positional subcommands

A **verb layer** over the raw path (`cmd/herdrctl/subcommands.go` + tests). 30 verbs covering the
whole §7 vocabulary, e.g.:

```
herdrctl split h 2                → pane.split {"direction":"h","pane":2}
herdrctl focus 1                  → pane.focus {"pane":1}
herdrctl rename-pane 1 build srv  → pane.rename {"pane":1,"name":"build srv"}   (multi-word name)
herdrctl panes / session / tabs   → the read-only queries
herdrctl new-tab / stop / reload  → no-param commands
```

- Each verb maps to one method and **builds params by reusing the `app.*Params` structs**, so the
  wire shape can't drift from the server's.
- The raw `<method> [--params json]` form stays the **full-coverage escape hatch** (only way to
  reach `read`'s `rect` or `capture`'s `ansi`/`unwrap`, which the verbs omit).
- `herdrctl help` lists the verbs (aligned table); `herdrctl commands` still lists raw method names.
- Deliberate limitation (documented): global flags (`--socket`, `--json`) go **before** the verb —
  Go's `flag` stops parsing at the first positional operand.

**Verified:** builder unit tests (param shape + error cases + a registry-integrity test that every
verb maps to a real method) + a live run of verbs, error paths (exit 2 with usage hints), the
escape hatch, and `stop` (termhost survived).

## Commit 3 — `f20bbd8` YAML config file (server + theme + keybindings)

Scope confirmed with the user up front (AskUserQuestion): **server settings + keybindings
(copy-mode only) + theme**; deferred shell/scrollback and live-push to open browsers.

**New `internal/config` package** (first YAML dep — `goccy/go-yaml`; house style = stdlib
`errors`/`fmt` + prefixed `log`, **not** serr despite the skill):

- **Server** — addr, termhost/control sockets, auth, session-ttl, tls. Precedence
  **flag > config > default**: `main.go` seeds effective values from the config, then `flag.Visit`
  lets only explicitly-set flags win. Password is **env/flag-only**, never in the file.
- **Theme** — the 13 `:root` CSS custom properties + font, injected as a `<style>` override.
- **Keybindings** — copy-mode action → keys, injected as `window.__herdrKeys`; `index.html`'s
  `copyModeKey` now switches on the resolved **action** (was a literal `switch (e.key)`), with a JS
  fallback table for standalone use.
- Missing file = defaults (no error); **partial configs merge key-wise** (set one colour / rebind
  one action, keep the rest — needed a manual map-merge because goccy replaces maps wholesale, and
  an empty-document short-circuit because goccy zeroes the struct on empty input). Bad
  enum/duration/keybinding fails loudly at startup.

**`server.reload_config` is now real** (`orch.ReloadConfig`, was a no-op): re-reads the file and
**atomically re-renders** the served page (`atomic.Pointer[[]byte]`, because the HTTP `/` handler
and the reload run on different goroutines) — theme/keys apply to new page loads without a restart;
server settings still need one.

**Injection is sanitised** — `renderPage` (untagged `cmd/gateway2/page.go`, so its test runs without
the ghostty toolchain) strips markup-breaking chars from CSS values and relies on `json.Marshal`'s
HTML escaping so a config value can't escape `<style>`/`<script>` (unit-tested with hostile inputs).
Added `config.example.yaml` documenting every knob.

**Verified:** config + page-render unit tests (13 pkgs green, ghostty gateway2 tests pass); a node
check of the reverse-map keybinding logic (fallback + rebind); and a **live run** — served page
carried the injected theme + rebound `yank:[y,c]`/`exit:[Escape,q,x]`; `herdrctl reload` updated the
live page (`--bg` → new colour); `--addr` overrode the config addr (flag>config); `auth: sometimes`
rejected at startup (exit 1); the example config loads and serves.

## Files

- **New:** `internal/app/query.go` (+`query_test.go`), `cmd/herdrctl/subcommands.go`
  (+`subcommands_test.go`), `internal/config/config.go` (+`config_test.go`),
  `cmd/gateway2/page.go` (+`page_test.go`), `config.example.yaml`.
- **Modified:** `.gitignore`, `internal/app/command_vocab.go`, `internal/app/commands.go`,
  `internal/browserproto/cmd.go`, `cmd/herdrctl/main.go`, `cmd/gateway2/main.go`,
  `cmd/gateway2/gateway.go`, `cmd/gateway2/web/index.html`, `go.mod`, `go.sum`.
- **Untracked (left as-is):** `ai_docs/ai_todo.md` (scratch).

## Notes / leftovers (WS4, still open)

- **Streaming methods** — `events.subscribe`, `pane.wait_for_output`. The biggest remaining item:
  needs a new envelope on `ctlproto` (today it's strictly one-request/one-response) so the browser
  and control-API can push/await instead of poll.
- **Repeatable browser-driven check** — capture the ad-hoc Playwright harness from the 07-15-1037
  session as a project `/run` skill (dev-server line, short `/tmp` sockets, the two Chrome flags,
  one representative interaction). Copy-mode keybindings with a **rebound** key weren't
  click-tested in a real browser this session (verified: injection unit test + node reverse-map
  logic + JS syntax); a browser drive would close that.
- **Minor polish deferred:** the copy-mode toast hint (`"hjkl/arrows move · v select …"`) is still
  hardcoded to default keys (a hint string, not behaviour). Pre-existing cosmetic: the
  `serving at http://localhost<addr>` log line reads oddly now that config makes full `host:port`
  addrs common.
