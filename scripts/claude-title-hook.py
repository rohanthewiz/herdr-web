#!/usr/bin/env python3
"""Claude Code hook: advertise the running session's friendly name to herdr-web.

Wire this to the SessionStart and Stop hooks. On each call it reads the hook
payload on stdin, finds the session's latest auto-generated title (the
`ai-title` line in the transcript), and POSTs it to the herdr-web gateway, which
broadcasts it to connected browsers as the tab title (document.title).

herdr forwards no usable window title to the gateway, so this push is the only
way the browser tab can reflect "what's running" instead of the static pane name.

The ai-title is generated a turn or two into a session, so at SessionStart it is
usually absent; we fall back to "Claude Code · <cwd basename>" until it exists.

Gateway URL defaults to http://localhost:8420 (override with HERDR_GATEWAY_URL).
All failures are swallowed: a missing gateway must never disrupt the session.
"""

import json
import os
import sys
import urllib.request


def latest_ai_title(transcript_path: str) -> str:
    """Return the most recent aiTitle in the transcript, or "" if none."""
    title = ""
    try:
        with open(transcript_path, "r", encoding="utf-8") as f:
            for line in f:
                try:
                    obj = json.loads(line)
                except ValueError:
                    continue
                if obj.get("type") == "ai-title" and obj.get("aiTitle"):
                    title = obj["aiTitle"]  # keep the last (newest) one
    except OSError:
        pass
    return title.strip()


def main() -> int:
    try:
        payload = json.load(sys.stdin)
    except (ValueError, OSError):
        payload = {}

    title = latest_ai_title(payload.get("transcript_path", ""))
    if not title:
        cwd = (payload.get("cwd") or "").rstrip("/")
        base = os.path.basename(cwd) or "session"
        title = f"Claude Code · {base}"

    url = os.environ.get("HERDR_GATEWAY_URL", "http://localhost:8420").rstrip("/")
    body = json.dumps({"title": title}).encode("utf-8")
    req = urllib.request.Request(
        url + "/title", data=body, headers={"Content-Type": "application/json"}
    )
    try:
        urllib.request.urlopen(req, timeout=1).read()
    except Exception:
        pass  # gateway not running / unreachable — ignore

    return 0


if __name__ == "__main__":
    sys.exit(main())
