2. Streaming methods — events.subscribe and pane.wait_for_output (push/await semantics over the control socket).
3. TOML config + keybindings — configuration file support.

Streaming methods — events.subscribe and pane.wait_for_output (push/await semantics over the control socket)

4. Ergonomic herdrctl subcommands — friendlier CLI verbs over the raw command table.
Housekeeping:
5. .gitignore for built binaries — gateway2, herdrctl, and the tracked root termhost are currently not ignored. 
6. Repeatable browser-driven check — capture the ad-hoc Playwright drive.js harness as a project /run skill (dev-server line,  short /tmp sockets, the two Chrome flags, one representative interaction). Right now it lives only in the session scratchpad.