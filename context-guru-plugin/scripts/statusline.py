#!/usr/bin/env python3
"""Claude Code statusLine command: by default, render what THIS session saved against what it
has spent so far. The prompt-cache TTL countdown and the keep-alive ping counter are opt-in
extras (--cache / --keepalive) — see skills/statusline/SKILL.md.

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
import urllib.parse
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

# A session_id Claude Code did not itself generate. Guards the one place this script puts a
# stdin-supplied string into a URL and a tempfile path: an id that does not look like the UUID
# Claude Code actually sends is treated as absent rather than trusted.
_SESSION_ID_RE = re.compile(r"^[A-Za-z0-9_-]{1,128}$")


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


def _stats_cache_path(port: str, session_id: str | None) -> str:
    # Keyed by session too, not just port: /api/stats is now fetched SCOPED to one session (see
    # _fetch_stats), so two terminals sharing one proxy on one port must not read each other's
    # cached response back as their own.
    suffix = f"-{session_id}" if session_id else ""
    return os.path.join(tempfile.gettempdir(), f"context-guru-statusline-{port}{suffix}.json")


def _fetch_stats(port: str, session_id: str | None) -> tuple[dict | None, bool]:
    """Returns (stats, proxy_down). stats is None when unavailable for any reason; proxy_down is
    True only when the proxy could not be reached at all (vs. answered but had nothing useful —
    e.g. the dashboard flag is off, or sent something this script cannot parse).

    Scoped to `session_id` (the dashboard's existing `?session=` filter, dash/query.go's
    Filter.Session — nothing new added to the proxy) so the savings figure returned pairs with
    THIS session's own cost/tokens rather than the proxy's whole retained window, which is what
    /api/stats reports unscoped and is process-wide across every project routed through this
    proxy — see _default_segment for why mixing the two would be dishonest.
    """
    cache_path = _stats_cache_path(port, session_id)
    try:
        st = os.stat(cache_path)
        if time.time() - st.st_mtime < STATS_CACHE_TTL_SECONDS:
            with open(cache_path, encoding="utf-8") as fh:
                return json.load(fh), False
    except (OSError, ValueError):
        pass  # no usable cache; fetch for real

    url = f"http://127.0.0.1:{port}/api/stats"
    if session_id:
        url += "?session=" + urllib.parse.quote(session_id, safe="")
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


def _session_totals(payload: dict) -> tuple[float | None, float | None]:
    """This SESSION's own running cost and tokens, read off Claude Code's own statusLine
    payload — no network call, and it is the only session-scoped source there is (confirmed by
    grepping the installed CLI: `strings <claude binary> | grep total_cost_usd` shows the payload
    literal `cost:{total_cost_usd:...,total_duration_ms:...,...}`, and Claude Code's own SDK
    schema describes `cost.total_cost_usd` as "Cost and usage accumulated by the current
    session"; a real capture confirmed it climbing turn over turn against one session_id).

    Tokens come from `context_window.total_input_tokens` + `total_output_tokens`, NOT from a
    token field inside `cost` — there isn't one. Claude Code's own SDK schema has a
    `model_usage` map that would give an exact per-turn sum, but the object literal that would
    add it to THIS payload is dead code in the installed CLI (`cost:{total_cost_usd:ru(),...!1,
    total_duration_ms:...}` — that `...!1` spreads the literal `false`, a no-op), so it is never
    actually present to read. `context_window`'s pair is instead the size of the latest turn's
    own usage (fresh input, plus whatever cache tiers it hit, plus the reply) — the same number
    behind Claude Code's own `/context` view, and, for a conversation that only grows, in
    practice the running total a status line means by "what this session has used" even though
    it is not a strict per-turn sum. Returns (None, None) where the payload cannot support
    either — a malformed stdin, or a real one before the first response has anything to report.
    """
    usd = None
    cost = payload.get("cost")
    if isinstance(cost, dict) and isinstance(cost.get("total_cost_usd"), (int, float)):
        usd = float(cost["total_cost_usd"])

    tokens = None
    cw = payload.get("context_window")
    if isinstance(cw, dict):
        i, o = cw.get("total_input_tokens"), cw.get("total_output_tokens")
        if isinstance(i, (int, float)) and isinstance(o, (int, float)):
            tokens = float(i) + float(o)

    if usd is None or tokens is None:
        return None, None
    return usd, tokens


def _default_segment(payload: dict, stats: dict | None) -> str | None:
    """What THIS session saved, against what it has spent so far — the default and, unless a
    toggle below is turned on, the ONLY thing this status line shows.

    Omitted, not shown as zeroes, in the two cases that mean "nothing to report yet" rather than
    a broken feature: a malformed/absent stdin payload, and a genuinely brand-new session (its
    own total is 0 and 0 before the first response has usage to track — dividing "saved" by a
    total of nothing is nonsensical, not merely undramatic, so this returns before that division
    is ever written). A real, nonzero total with zero saved DOES still print (`$0.00/0 saved of
    ...`), same reasoning as the old segment's negative-net case: a real $0 saved this session is
    not the same fact as no session having happened yet.
    """
    total_usd, total_tokens = _session_totals(payload)
    if total_usd is None or (total_usd == 0 and total_tokens == 0):
        return None
    if stats is None:  # no session-scoped savings figure to pair the total with
        return None
    saved_usd = stats.get("total_saved_usd", 0) or 0
    saved_tokens = stats.get("saved_unique", 0) or 0
    return (f"${saved_usd:.2f}/{_human(saved_tokens)} saved of "
            f"${total_usd:.2f}/{_human(total_tokens)}")


def _cache_segment_enabled() -> bool:
    return "--cache" in sys.argv[1:]


def _keepalive_segment_enabled() -> bool:
    return "--keepalive" in sys.argv[1:]


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
    session_id = payload.get("session_id")
    if not isinstance(session_id, str) or not _SESSION_ID_RE.match(session_id):
        session_id = None  # absent, or not the shape Claude Code actually sends: don't trust it

    stats, proxy_down = _fetch_stats(port, session_id)
    if proxy_down:
        print("cg!")  # three characters: the proxy is unreachable, nothing else is worth saying
        return

    # Priority is savings-vs-session-total first and always on; the cache TTL stopper and the
    # keep-alive ping counter are opt-in extras, off unless their flag is passed — see
    # skills/statusline/SKILL.md for the one-line command that turns either on.
    parts = []
    default_seg = _default_segment(payload, stats)
    if default_seg:
        parts.append(default_seg)
    if _cache_segment_enabled():
        parts.append(_cache_stopper(payload))
    if stats is not None and _keepalive_segment_enabled():
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
