#!/usr/bin/env python3
"""Claude Code statusLine command: render what the local context-guru proxy's KV cache is doing.

Why Python, not another shell script like the hooks: this reads JSON off stdin and makes one
timeout-bounded HTTP call, and that is what Python's stdlib (`json`, `urllib.request`) does
directly. Bash would need `jq` (not guaranteed installed) for the first and a `curl` subprocess
for the second — two extra failure points for a script whose one job is to never fail.

Claude Code invokes this on (almost) every render — a state change, or the configured
`refreshInterval` — so it has to be cheap and it has to be silent about failure. Two hard rules
follow from that, and both are enforced structurally rather than by discipline:

* READ-ONLY. This never pings the proxy or writes anything the proxy would see. Sending a
  keep-alive ping from a command that fires on every keystroke would turn a display hook into an
  unbounded traffic generator billed to the user — see skills/keepalive/SKILL.md for the actual,
  explicit, opt-in way to do that.
* NEVER BLOCKS, NEVER FAILS THE RENDER. Two blocking calls exist: reading stdin, and the HTTP GET
  to /api/stats. The GET carries its own short timeout. Reading stdin normally returns instantly
  (Claude Code writes the payload and closes the pipe before running this), but a `signal.alarm`
  backstop bounds it too, so a wedged parent cannot hang this script indefinitely. Worst case is
  therefore the sum of the two bounds, a couple of seconds — not unbounded. Every exit is 0; on
  any error this prints whatever partial line it already has, or nothing.

Self-gating: exactly like the SessionStart/UserPromptSubmit hooks, this does nothing in a project
that is not routed through OUR proxy. The gate reads CLAUDE_PLUGIN_OPTION_PORT the same way those
two hooks do, but never TRUSTS it alone — it still checks $ANTHROPIC_BASE_URL for that exact port,
because that env block is what actually routes the traffic and so cannot be wrong. Matching any
loopback ".../anthropic" URL regardless of port would treat another local proxy on its own port
(litellm defaults to 127.0.0.1:4000) as ours whenever it happened to be a project's real routing.
"""

from __future__ import annotations

import json
import os
import re
import signal
import sys
import tempfile
import time
import urllib.error
import urllib.request

# How long the whole script may run before its own backstop fires. Generous relative to the
# urlopen timeout below (0.6s) so that timeout is what normally fires first; this is the net
# under it, covering a stuck stdin read or anything else unaccounted for.
SCRIPT_BUDGET_SECONDS = 2

# The HTTP call's own timeout. /api/stats on a healthy local proxy answers in single-digit
# milliseconds; 0.6s is already generous and still leaves headroom under the script budget above.
HTTP_TIMEOUT_SECONDS = 0.6

# How long a cached /api/stats response is reused before a fresh fetch is attempted. Claude Code
# can re-render on every token during a stream, and this is what stops that from becoming a
# request-per-render storm against the proxy.
STATS_CACHE_TTL_SECONDS = 2.0

# The plugin's own default port, matching context-guru-plugin/.claude-plugin/plugin.json's
# "port" option default — used only as a fallback when CLAUDE_PLUGIN_OPTION_PORT is absent.
DEFAULT_PORT = "8787"


class _Budget(Exception):
    """Raised by the SIGALRM backstop; caught once, at the very top."""


def _on_alarm(_signum, _frame):
    raise _Budget()


def _read_stdin_json() -> dict:
    try:
        raw = sys.stdin.read()
    except Exception:
        return {}
    if not raw:
        return {}
    try:
        data = json.loads(raw)
    except (ValueError, TypeError):
        return {}
    return data if isinstance(data, dict) else {}


def _our_port() -> str | None:
    """The port this project is routed to OUR proxy on, or None if it is not routed through us.

    Reads CLAUDE_PLUGIN_OPTION_PORT the same way check-proxy.sh and start-proxy.sh do (default
    8787), then checks the ACTUAL routing value: does $ANTHROPIC_BASE_URL name exactly that port.
    Accepting ANY loopback ".../anthropic" URL here — instead of a SPECIFIC configured port —
    would treat litellm's own default (127.0.0.1:4000/anthropic) as ours whenever it happens to
    be a project's real routing, which is precisely the class of bug those two hooks' own tests
    (matching the port, not merely "localhost") exist to catch.
    """
    port = (os.environ.get("CLAUDE_PLUGIN_OPTION_PORT") or DEFAULT_PORT).strip() or DEFAULT_PORT
    if not port.isdigit():
        return None
    base = os.environ.get("ANTHROPIC_BASE_URL", "")
    pattern = rf"^https?://(?:127\.0\.0\.1|localhost|\[::1\]):{re.escape(port)}/anthropic/?$"
    return port if re.match(pattern, base) else None


def _cache_stopper(payload: dict) -> str:
    """The countdown to the prompt-cache going cold, from Claude Code's OWN cache tracker.

    `prompt_cache` rides on the statusLine payload already — Claude Code tracks this itself from
    the response usage of every request it sends, independent of context-guru, so it is free
    (zero extra network calls) and it reflects the ACTUAL wire bytes regardless of what any
    component rewrote. It is absent before the first response of a session has usage to track,
    which is a normal state, not an error.
    """
    pc = payload.get("prompt_cache")
    if not isinstance(pc, dict):
        return "cache –"
    expires_at = pc.get("expires_at")
    if not isinstance(expires_at, (int, float)):
        return "cache –"
    remaining = expires_at - time.time()
    if remaining <= 0:  # covers real expiry AND clock skew alike: never show negative time
        return "cache cold"
    mins, secs = divmod(int(remaining), 60)
    return f"cache {mins}:{secs:02d}"


def _human(n: float) -> str:
    n = float(n)
    for suffix, scale in (("M", 1_000_000), ("k", 1_000)):
        if abs(n) >= scale:
            return f"{n / scale:.1f}{suffix}"
    return f"{n:.0f}"


def _stats_cache_path(port: str) -> str:
    return os.path.join(tempfile.gettempdir(), f"context-guru-statusline-{port}.json")


def _fetch_stats(port: str) -> tuple[dict | None, bool]:
    """Returns (stats, proxy_down). stats is None when unavailable for any reason; proxy_down is
    True only when the proxy could not be reached at all (vs. answered but had nothing useful —
    e.g. the dashboard flag is off, or sent something this script cannot parse)."""
    cache_path = _stats_cache_path(port)
    try:
        st = os.stat(cache_path)
        if time.time() - st.st_mtime < STATS_CACHE_TTL_SECONDS:
            with open(cache_path, encoding="utf-8") as fh:
                return json.load(fh), False
    except (OSError, ValueError):
        pass  # no usable cache; fetch for real

    url = f"http://127.0.0.1:{port}/api/stats"
    try:
        with urllib.request.urlopen(url, timeout=HTTP_TIMEOUT_SECONDS) as resp:
            body = resp.read(1 << 20)  # bounded: the real payload is a few KB
    except urllib.error.HTTPError:
        return None, False  # reached the proxy; it just has nothing (e.g. --dashboard is off)
    except (urllib.error.URLError, OSError):
        return None, True  # refused, timed out, or otherwise unreachable: the proxy is down

    try:
        stats = json.loads(body)
    except (ValueError, TypeError):
        return None, False
    if not isinstance(stats, dict):
        return None, False

    try:
        tmp = cache_path + f".{os.getpid()}.tmp"
        with open(tmp, "wb") as fh:
            fh.write(body)
        os.replace(tmp, cache_path)
    except OSError:
        pass  # caching is an optimization; failing to write one is not this script's problem
    return stats, False


def _savings_segment(stats: dict) -> str | None:
    usd = stats.get("total_saved_usd", 0) or 0
    tokens = stats.get("saved_unique", 0) or 0
    if usd == 0 and tokens == 0:
        # Omitted rather than shown as "$0.00/0 saved", matching the dashboard's own convention
        # (see metrics.Snapshot.KeepAlive's omitempty): a fresh install has nothing to say yet,
        # and a row of zeroes there reads as a broken feature rather than a quiet truth.
        #
        # NOT `<= 0`: total_saved_usd can go genuinely negative (an idle keep-alive spending more
        # than it recovers nets the whole figure negative — see dash/overview.go's own waterfall,
        # "a real outcome the dashboard will not hide"), and that is exactly the case this must
        # keep showing rather than quietly folding into the same omission as a fresh install.
        return None
    return f"${usd:.2f}/{_human(tokens)} saved"


def _keepalive_segment(stats: dict) -> str | None:
    pings = stats.get("keepalive_pings", 0) or 0
    if pings <= 0:
        return None
    return f"ka {int(pings)}p"


def main() -> None:
    port = _our_port()
    if port is None:
        return  # not routed through us: say nothing, exactly like the other hooks

    payload = _read_stdin_json()
    cache_seg = _cache_stopper(payload)

    stats, proxy_down = _fetch_stats(port)
    if proxy_down:
        print("cg!")  # three characters: the proxy is unreachable, nothing else is worth saying
        return

    parts = [cache_seg]
    if stats is not None:
        savings = _savings_segment(stats)
        if savings:
            parts.append(savings)
        keepalive = _keepalive_segment(stats)
        if keepalive:
            parts.append(keepalive)
    print(" | ".join(parts))


if __name__ == "__main__":
    try:
        if hasattr(signal, "SIGALRM"):  # POSIX only, same as the plugin's shell hooks
            signal.signal(signal.SIGALRM, _on_alarm)
            signal.alarm(SCRIPT_BUDGET_SECONDS)
        main()
    except Exception:
        pass  # never a traceback on a user's terminal; silence is the correct failure mode here
    finally:
        if hasattr(signal, "SIGALRM"):
            signal.alarm(0)
    sys.exit(0)
