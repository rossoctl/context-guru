#!/usr/bin/env python3
"""Claude Code statusLine command. Layouts (--layout=minimal|balanced|detailed) pick elements:
money saved (against the same request uncompacted, net of context-guru's own
spend) next to money spent (measured), the prompt-cache state (warm / expiring / cold, the proxy's
server-side truth overriding Claude Code's own guess), context fill against the window actually
served, and a weekly tip line. See skills/statusline/SKILL.md for flags and mockups.

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

`cg ✕ proxy offline` means UNREACHABLE, specifically — refused, DNS failure, anything that is not "answered, just
not within 0.6s". A proxy that is merely slow (its own aggregate endpoint can legitimately take
some time under load, especially behind many concurrent readers — see dash/api.go's jsonCache)
renders the last good cached numbers with a small stale marker instead, or, with no cache yet to
fall back on, a short "stats loading…"; if a cache DOES exist but has gone stale enough that
every fetch since has still timed out, "cg? not responding" instead — reachable, unlike `cg ✕ proxy offline`,
but no longer plausibly just slow either. See _fetch_stats and its STATS_* result codes for all
four. This used to not be true: EVERY URLError/OSError, including a plain `TimeoutError`,
rendered `cg ✕ proxy offline`, so a slow-but-healthy proxy looked exactly like a dead one on every render until
the aggregate happened to answer in under 0.6s.

Self-gating: exactly like the SessionStart/UserPromptSubmit hooks, this does nothing in a project
that is not routed through OUR proxy. The gate is settings.resolve_routed_port, which every port
consumer in this plugin now shares: it reads the port off $ANTHROPIC_BASE_URL — the env block that
actually routes the traffic, so it cannot be wrong about the port — and then confirms from a
settings file or an install record that WE wrote that URL. Provenance is required because matching
any loopback ".../anthropic" URL regardless of port would treat another local API proxy on its own
port (127.0.0.1:4000 is a common one) as ours whenever it happened to be a project's real routing.

This script is NOT a hook, which is what the gate used to get wrong: CLAUDE_PLUGIN_OPTION_PORT
reaches hook environments only, so reading the port from there with 8787 as a fallback meant 8787
was the answer on every render, and every project allocated another port rendered nothing at all.
"""

from __future__ import annotations

import json
import os
import re
import signal
import socket
import subprocess
import sys
import threading
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import cgcache  # noqa: E402  (the shared on-disk request budget; stdlib only, import-cheap)

# How long the whole script may run before its own backstop fires. Generous relative to the
# urlopen timeout below (0.6s) so that timeout is what normally fires first; this is the net
# under it, covering a stuck stdin read or anything else unaccounted for.
SCRIPT_BUDGET_SECONDS = 2  # historical net; the real bound is RENDER_BUDGET_SECONDS below

# The whole render must finish in half a second, Python start-up included (25-70 ms before this
# module even runs). The budget is measured from import, so it is set well under 0.5 s; every network
# read is sized from what is left of it (see _remaining), and the detached refresher lifts it.
_T0 = time.monotonic()
RENDER_BUDGET_SECONDS = 0.36
RENDER_SAFETY_SECONDS = 0.03  # kept back for composing and printing the line

# The HTTP call's own timeout. /api/stats on a healthy local proxy answers in single-digit
# milliseconds; 0.6s is already generous and still leaves headroom under the script budget above.
HTTP_TIMEOUT_SECONDS = 0.6  # ceiling for one read; the render budget is what usually bounds it
COLD_READ_SECONDS = 0.15  # a read with NOTHING cached to show may wait this long for its first answer

SLOW_UPSTREAM_MS = 3000  # the detailed layout names the provider only when it is this slow

# How long a cached /api/stats response is reused before a fresh fetch is attempted. Claude Code
# can re-render on every token during a stream, and this is what stops that from becoming a
# request-per-render storm against the proxy.
STATS_CACHE_TTL_SECONDS = 30.0  # CONTRACT section 4: at least 30 s

# How long a cached response may still be shown, marked stale, when a fresh fetch TIMES OUT rather
# than being refused outright. The proxy's own aggregate endpoint can legitimately take tens of
# seconds under load (dash/api.go's jsonCache serves a stale body itself while it recomputes, but a
# reader can still arrive in the gap before that recompute lands) — that is a slow proxy, not a
# down one, and showing the last real numbers with a small marker is strictly more honest than
# either blocking this render or claiming the proxy is unreachable. Ten minutes is generous
# relative to STATS_CACHE_TTL_SECONDS precisely because it is the FALLBACK for the case the fast
# path failed to refresh in time, not the normal path.
STALE_STATS_MAX_AGE_SECONDS = 600

# There is deliberately NO default port constant here any more. A statusLine command is not a hook,
# so CLAUDE_PLUGIN_OPTION_PORT never reaches it, which made a "fallback" of 8787 the answer on every
# render — see settings.resolve_routed_port, which this script now asks instead.

# A session_id Claude Code did not itself generate. Guards the one place this script puts a
# stdin-supplied string into a URL and a tempfile path: an id that does not look like the UUID
# Claude Code actually sends is treated as absent rather than trusted.
_SESSION_ID_RE = re.compile(r"^[A-Za-z0-9_-]{1,128}$")

# Context-window bar thresholds, as fractions of the window — same bands as the IBM deployment's
# renderer, so a developer moving between the two reads the same colour the same way.
GREEN_MAX = 0.50
YELLOW_MAX = 0.70
BAR_CELLS = 8
RESET, GREEN, YELLOW, RED, DIM, CYAN = "\033[0m", "\033[32m", "\033[33m", "\033[31m", "\033[2m", "\033[36m"
PROXY_FRESH_SECONDS = 45.0  # a proxy copy younger than this drives the clock; covers a refresh delayed by back-off or a slow server (see _cache_view)
CTX_PROXY_FRESH_SECONDS = 11.0  # the context bar takes the proxy's token count only from a copy this young (the cachestate TTL plus a second)

# How many tool_use blocks of transcript tail this reads per render, looking for unused-tool
# usage. Bounded for the same reason _fetch_stats bounds its own read: this runs on (almost)
# every render.
#
# ponytail: no cross-session persistence here (unlike the IBM deployment's statusline-state.json)
# — this plugin has no established per-plugin data directory to write one into, and a session-only
# share is still the number the "remove / move / keep" bands were defined against. Add
# CLAUDE_PLUGIN_DATA-backed cross-session totals if a genuine need for the longer window shows up.
TRANSCRIPT_TAIL_BYTES = 512 * 1024

RECOMMEND_REMOVE_MAX = 0.01
RECOMMEND_PROJECT_MAX = 0.20

_SKILL_NAME_RE = re.compile(r"^name:\s*(\S+)\s*$", re.MULTILINE)


def _latency_segment(stats: dict | None) -> str | None:
    """`upstream slow: 7.0s`, only when the provider is slow (/api/stats' own average,
    dash.Overview.UpstreamMsAvg): a healthy 340ms is not news, and raw milliseconds are noise."""
    upstream_ms = stats.get("upstream_ms_avg") if isinstance(stats, dict) else None
    if not isinstance(upstream_ms, (int, float)) or upstream_ms < SLOW_UPSTREAM_MS:
        return None
    return f"{YELLOW}upstream slow:{RESET} {upstream_ms / 1000:.1f}s"


def _configured_tool_names() -> list[str]:
    """This plugin's own skills, plus any MCP server named in the user's settings.json —
    best-effort, for the unused-tool recommendation. Not a system-prompt enumeration (nothing
    exposes that); see _recommend_segment for what that means for the claim this segment can make.
    """
    names: set[str] = set()
    try:
        skills_dir = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "skills")
        import glob  # noqa: PLC0415 - only the opt-in tools element needs it; ~8 ms of every render otherwise
        for path in glob.glob(os.path.join(skills_dir, "*", "SKILL.md")):
            try:
                with open(path, encoding="utf-8") as fh:
                    text = fh.read(4096)
            except OSError:
                continue
            match = _SKILL_NAME_RE.search(text)
            if match:
                names.add(match.group(1))
    except OSError:
        pass
    try:
        settings_path = os.path.join(os.path.expanduser("~"), ".claude", "settings.json")
        with open(settings_path, encoding="utf-8") as fh:
            settings = json.load(fh)
        servers = settings.get("mcpServers") if isinstance(settings, dict) else None
        if isinstance(servers, dict):
            names.update(n for n in servers if isinstance(n, str) and n)
    except (OSError, ValueError):
        pass
    return sorted(names)


def _tool_use_counts(transcript_path: object) -> dict[str, int]:
    """Skill and MCP-SERVER use counts from the tail of THIS session's own transcript, keyed to
    match what _configured_tool_names reports as "available": a Skill block counts under its own
    skill name, and an `mcp__server__tool` block counts under `server` — NOT `tool` — because
    _recommend_segment compares this against configured SERVER names, and a server with three
    tools used between them is used, not "barely used" three separate, undercounted ways. Everything
    else (a Claude Code built-in) is excluded.
    """
    counts: dict[str, int] = {}
    if not isinstance(transcript_path, str) or not transcript_path:
        return counts
    try:
        size = os.path.getsize(transcript_path)
        start = max(0, size - TRANSCRIPT_TAIL_BYTES)
        with open(transcript_path, "rb") as fh:
            fh.seek(start)
            text = fh.read(size - start).decode("utf-8", "replace")
    except OSError:
        return counts
    for line in text.split("\n"):
        if '"tool_use"' not in line:
            continue
        try:
            record = json.loads(line)
        except ValueError:
            continue
        if not isinstance(record, dict) or record.get("type") != "assistant":
            continue
        message = record.get("message")
        content = message.get("content") if isinstance(message, dict) else None
        if not isinstance(content, list):
            continue
        for block in content:
            if not (isinstance(block, dict) and block.get("type") == "tool_use"):
                continue
            name = block.get("name")
            if not isinstance(name, str) or not name:
                continue
            if name == "Skill":
                skill = block.get("input", {})
                skill = skill.get("skill") if isinstance(skill, dict) else None
                key = skill.split(":")[-1] if isinstance(skill, str) and skill else "Skill"
            elif name.startswith("mcp__"):
                parts = name.split("__")
                # parts[1] is the SERVER — "mcp", "<server>", "<tool>", ... — which is the name
                # _configured_tool_names reports and _recommend_segment compares this against.
                key = parts[1] if len(parts) > 1 and parts[1] else name
            else:
                continue  # a Claude Code built-in: not one of the two categories this tracks
            counts[key] = counts.get(key, 0) + 1
    return counts


def _recommend_segment(payload: dict) -> str | None:
    """One flagged configured tool/skill, unused or barely used THIS SESSION: `remove` (<=1% of
    this session's tool/skill uses), `move` (<=20%, i.e. to project scope rather than global).

    SESSION-SCOPED, not a system-prompt enumeration — see _configured_tool_names for what
    "configured" means here. `counts` is keyed to match: a skill by its own name, an MCP SERVER
    by its name with every one of its tools' uses summed into it (see _tool_use_counts) — so a
    server is scored by its combined usage, not by whichever single tool happened to be called
    most. A name absent from `counts` is unused in what this session's transcript tail shows,
    which is what the segment can actually claim.
    """
    available = _configured_tool_names()
    if not available:
        return None
    counts = _tool_use_counts(payload.get("transcript_path"))
    total = sum(counts.values())
    if total <= 0:
        return None  # nothing used yet this session; too early to call anything "unused"
    flagged: list[tuple[float, str, str]] = []
    for name in available:
        share = counts.get(name, 0) / total
        if share <= RECOMMEND_REMOVE_MAX:
            flagged.append((share, name, "remove"))
        elif share <= RECOMMEND_PROJECT_MAX:
            flagged.append((share, name, "move"))
    if not flagged:
        return None
    flagged.sort(key=lambda t: t[0])
    share, name, verdict = flagged[0]
    label = name if len(name) <= 12 else name[:11] + "…"
    return f"{DIM}◇{RESET} {label} {share * 100:.0f}% {YELLOW}{verdict}{RESET}"


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

    Delegates to settings.resolve_routed_port, which is the single implementation of this rule for
    every consumer. It used to be answered here, from CLAUDE_PLUGIN_OPTION_PORT with 8787 as a
    fallback, gated on $ANTHROPIC_BASE_URL naming exactly that port — and a statusLine command is
    NOT a hook, so that option variable is never set for this process. The fallback was therefore
    the answer on every render, and in any project whose allocated port is not 8787 the gate failed
    to match and this returned None: a correct install with a permanently blank status line, and no
    error anywhere to explain it.

    THE GATE, deliberately, and not settings.resolve_reportable_port: this is the one consumer with
    no second check of its own (both hooks re-compare the URL against the port after the delegate
    answers), so whatever this returns is rendered. Asked the report question instead, it printed a
    permanent `cg ✕ proxy offline` — a down-proxy warning — in a project whose ANTHROPIC_BASE_URL pointed at the
    real API and whose only sin was having an install recorded on disk.

    The anti-false-positive property the old gate existed for is kept, and strengthened, in the
    shared rule: a port is only ours when a settings file or an install record says we put it there,
    never because the URL looks like a local proxy. Another local API proxy on
    127.0.0.1:4000/anthropic is still not us.

    Import is local and swallowed: this script's contract is that it never fails a render. A missing
    or broken settings.py renders no context-guru segment, exactly as an unrouted project does.
    """
    # The answer only changes when settings files do, but computing it imports settings.py and
    # walks the project (~25 ms, half the render budget), so it is remembered for a minute per
    # (base URL, cwd). An unrouted project is remembered too, as an empty answer.
    import zlib  # noqa: PLC0415 - C module, effectively free
    key = zlib.crc32(f"{os.environ.get('ANTHROPIC_BASE_URL', '')}\0{os.getcwd()}\0{os.environ.get('HOME', '')}".encode())
    memo = os.path.join(_tmpdir(), f"context-guru-statusline-route-{key:08x}.txt")
    try:
        if time.time() - os.stat(memo).st_mtime < ROUTE_MEMO_SECONDS:
            with open(memo, encoding="ascii") as fh:
                memo_port = fh.read(8).strip()
            return memo_port if memo_port.isdigit() else None
    except OSError:
        pass
    try:
        sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
        import settings  # noqa: PLC0415 - see docstring
        port, _source = settings.resolve_routed_port()
    except Exception:  # noqa: BLE001 - a status line must never raise
        return None
    port = port if port and port.isdigit() else None
    try:
        with open(memo, "w", encoding="ascii") as fh:
            fh.write(port or "")
    except OSError:
        pass
    return port


def _human(n: float) -> str:
    n = float(n)
    for suffix, scale in (("M", 1_000_000), ("k", 1_000)):
        if abs(n) >= scale:
            return f"{n / scale:.1f}{suffix}"
    return f"{n:.0f}"


def _tmpdir() -> str:
    """Where this script keeps its cache files: the private per-user `${XDG_CACHE_HOME:-~/.cache}/context-guru/`
    (0700), never a shared /tmp path (foreign files there break things on shared hosts). The name is
    historical."""
    return cgcache.cache_dir()


class _HTTPError(OSError):
    """A non-2xx answer: the proxy is reachable, it just has nothing for us."""


def _quote(s: str) -> str:
    return re.sub(r"[^A-Za-z0-9_.~-]", lambda m: "%%%02X" % ord(m.group()), s)


def _remaining() -> float:
    """Seconds of the render budget left for a read, never below 20 ms (a read that cannot start
    in time times out at once, which every caller already treats as 'slow')."""
    return max(0.02, _T0 + RENDER_BUDGET_SECONDS - RENDER_SAFETY_SECONDS - time.monotonic())


def _http_get(port: str, path: str, cap: float | None = None) -> bytes:
    """GET http://127.0.0.1:port/path, bounded by HTTP_TIMEOUT_SECONDS end to end. Raw socket
    rather than urllib.request: importing that costs ~24 ms per render, half the whole budget, for
    one loopback GET. HTTP/1.0 + Connection: close means a plain read-to-EOF with no chunking.
    Raises TimeoutError (slow), _HTTPError (non-2xx), OSError (unreachable)."""
    limit = min(HTTP_TIMEOUT_SECONDS, _remaining(), cap or HTTP_TIMEOUT_SECONDS)
    deadline = time.monotonic() + limit
    with socket.create_connection(("127.0.0.1", int(port)), timeout=limit) as sock:
        sock.sendall(f"GET {path} HTTP/1.0\r\nHost: 127.0.0.1\r\n\r\n".encode("ascii"))
        buf = b""
        while len(buf) < (1 << 20):
            sock.settimeout(max(0.001, deadline - time.monotonic()))
            chunk = sock.recv(65536)
            if not chunk:
                break
            buf += chunk
    head, _, body = buf.partition(b"\r\n\r\n")
    parts = head.split(b" ", 2)
    if len(parts) < 2 or not parts[1].startswith(b"2"):
        raise _HTTPError(head[:40].decode("ascii", "replace"))
    return body


def _stats_cache_path(port: str, session_id: str | None) -> str:
    # Keyed by session too, not just port: /api/stats is now fetched SCOPED to one session (see
    # _fetch_stats), so two terminals sharing one proxy on one port must not read each other's
    # cached response back as their own.
    suffix = f"-{session_id}" if session_id else ""
    return os.path.join(_tmpdir(), f"context-guru-statusline-{port}{suffix}.json")


# _fetch_stats' second return value. "down" is the only one that should ever render `cg ✕ proxy offline` — see
# the module docstring. The other three all mean "the proxy answered, or at least exists", just
# with varying amounts of fresh data behind the answer.
STATS_OK = "ok"  # a fresh fetch landed, or a within-TTL cache hit answered without a fetch at all
STATS_TIMEOUT = "timeout"  # urlopen's own clock ran out; the proxy is probably just slow right now
STATS_HUNG = "hung"  # timing out AND it has been going on long enough that "just slow" has
# stopped being the likely explanation — see STALE_STATS_MAX_AGE_SECONDS below. Distinguished
# from STATS_TIMEOUT (which still renders "stats loading…", the right read for "nothing heard
# yet, could be starting up") only by there having been a WORKING cache before: this fires once
# that cache is older than this script would ever serve it, and every fetch since has still
# timed out rather than coming back refused or answering.
STATS_DOWN = "down"  # refused, DNS failure, or anything else that means NOT REACHABLE
STATS_EMPTY = "empty"  # reached the proxy; it had nothing usable (dashboard off, bad payload)


def _timeout_result(cached: dict | None, cached_age: float | None) -> tuple[dict | None, str]:
    """What a timed-out fetch should return, given whatever was cached before it. Shared by both
    of _fetch_stats' timeout sites (the direct socket.timeout/TimeoutError and the one URLError
    wraps around a connect-phase timeout) so the two can never answer this differently.

    Three outcomes:
    * No cache at all (a fresh install, or right after a restart): STATS_TIMEOUT, stats None —
      "stats loading…". Nothing heard from the proxy yet is not evidence it is wedged.
    * A cache that is still within STALE_STATS_MAX_AGE_SECONDS: STATS_TIMEOUT, stats the cached
      body — the proxy answered recently enough that "merely slow right now" is still the better
      read, so those numbers are served with a stale marker rather than withheld.
    * A cache older than that, with the fetch that would have refreshed it STILL timing out:
      STATS_HUNG, stats None. There WAS a working proxy, and every attempt to reach it since has
      timed out rather than coming back refused or answered — long enough that "slow" has
      stopped being the likely explanation. Distinct from STATS_TIMEOUT's wording on purpose:
      "loading…" reads as transient and about to resolve itself; this does not pretend that.
    """
    if cached is None:
        return None, STATS_TIMEOUT
    if cached_age is not None and cached_age < STALE_STATS_MAX_AGE_SECONDS:
        return cached, STATS_TIMEOUT
    return None, STATS_HUNG


def _fetch_stats(port: str, session_id: str | None) -> tuple[dict | None, str]:
    """Returns (stats, status) — see STATS_OK / STATS_TIMEOUT / STATS_HUNG / STATS_DOWN /
    STATS_EMPTY above. stats is None except on STATS_OK, and on STATS_TIMEOUT when a
    recent-enough cached body from a PAST successful fetch exists (see
    STALE_STATS_MAX_AGE_SECONDS) — a timeout means this particular render did not get fresh
    numbers, not that no numbers exist.

    Scoped to `session_id` (the dashboard's existing `?session=` filter, dash/query.go's
    Filter.Session — nothing new added to the proxy) so the savings figure returned pairs with
    THIS session's own cost/tokens rather than the proxy's whole retained window, which is what
    /api/stats reports unscoped and is process-wide across every project routed through this
    proxy — see _default_segment for why mixing the two would be dishonest.

    `lean=1` asks the proxy to answer from its cheap path (dash/api.go's statsLean): every field
    this script reads, at full precision, without running the one call among /api/stats' four
    (DeclCreditFor's SelfRemovals, dash/toolapi.go) that does a full scan over tool_declarations
    for the WHOLE tenant regardless of the session filter — the rest are comparably cheap and are
    skipped only because this script does not read what they add. SelfRemovals was NOT the actual
    cause of the 38-45s timeouts this function recovers from, though: on a copy of the affected
    deployment's dashboard DB, every one of those four calls measured well under a second run in
    isolation. The real cause was dash/api.go's jsonCache spawning a brand-new goroutine running
    the FULL /api/stats compute on every single stale-cache read with no deduplication, so under
    real concurrent traffic many duplicate recomputes queued behind one SQLite connection pool at
    once — fixed on the proxy side by jsonCache.beginRefresh/endRefresh, which this script has no
    visibility into or control over. `lean=1` is therefore a reduction in how much work piles up
    behind that fix, not a workaround for it; it is not a substitute for the proxy-side fix.
    """
    if not session_id:
        # No session to scope to: the only unscoped read is the whole proxy's total (the full
        # handler since #510), which is no session's number and the dearest read there is. Ask
        # only whether the proxy is there, so the offline notices still work.
        try:
            _http_get(port, "/healthz")
        except (socket.timeout, TimeoutError):
            return None, STATS_TIMEOUT
        except _HTTPError:
            return None, STATS_EMPTY
        except OSError:
            return None, STATS_DOWN
        return {}, STATS_OK
    cache_path = _stats_cache_path(port, session_id)
    cached, cached_age = None, None
    try:
        st = os.stat(cache_path)
        cached_age = time.time() - st.st_mtime
        with open(cache_path, encoding="utf-8") as fh:
            cached = json.load(fh)
        if cached_age < STATS_CACHE_TTL_SECONDS:
            return cached, STATS_OK
    except (OSError, ValueError):
        cached, cached_age = None, None  # no usable cache; fetch for real

    wait = cgcache.backoff(cache_path)
    if wait:  # a recent failure: no request, and say what the last one said
        if wait.get("kind") == STATS_DOWN:
            return None, STATS_DOWN
        if wait.get("kind") == STATS_EMPTY:
            return None, STATS_EMPTY
        return _timeout_result(cached, cached_age)
    url = "/api/stats?lean=1&session=" + _quote(session_id)
    try:
        body = _http_get(port, url)
    except (socket.timeout, TimeoutError):
        # Caught ahead of the OSError branch below ON PURPOSE: both of these ARE OSErrors
        # (TimeoutError always; socket.timeout too, pre-3.10, where it is a distinct subclass
        # rather than TimeoutError itself — Python 3.10+ makes them literally the same object,
        # but this script has to run under whatever python3 the user has), so without this
        # clause first every slow-but-healthy proxy read the same as a dead one. This was
        # measured for real: a diagnostic capture on a live proxy showed ~10 of these in 10
        # minutes, every one a timeout at exactly HTTP_TIMEOUT_SECONDS with the proxy otherwise
        # healthy, no connection ever refused. This is the READ-phase timeout — urlopen raises it
        # directly once the connection succeeded but no response arrived in time.
        cgcache.record_failure(cache_path, STATS_TIMEOUT)
        return _timeout_result(cached, cached_age)
    except _HTTPError:
        cgcache.record_failure(cache_path, STATS_EMPTY)
        return None, STATS_EMPTY  # reached the proxy; it just has nothing (e.g. --dashboard is off)
    except OSError:
        cgcache.record_failure(cache_path, STATS_DOWN)
        return None, STATS_DOWN  # anything else unreachable: the proxy really is down

    try:
        stats = json.loads(body)
    except (ValueError, TypeError):
        return None, STATS_EMPTY
    if not isinstance(stats, dict):
        return None, STATS_EMPTY

    cgcache.write_atomic(cache_path, body)  # caching is an optimization; failing to write one is not this script's problem
    cgcache.clear_failure(cache_path)
    return stats, STATS_OK


def _stale_stats(port: str, session_id: str | None) -> tuple[dict | None, str]:
    """The fetch did not report within the render budget (a loaded host can starve its thread past
    the deadline): show the cached numbers, marked stale, as a timed-out fetch would. 'Loading'
    is only for when there is genuinely nothing cached."""
    cached, age = None, None
    try:
        path = _stats_cache_path(port, session_id)
        age = time.time() - os.stat(path).st_mtime
        with open(path, encoding="utf-8") as fh:
            cached = json.load(fh)
    except (OSError, ValueError):
        cached = None
    return _timeout_result(cached if isinstance(cached, dict) else None, age)


def _update_check_state_dir() -> str:
    """Same resolution as settings.py's state_dir() / start-proxy.sh's STATE, duplicated rather
    than imported: this script is invoked on (almost) every render and must not fork python a
    second time just to learn a path.
    """
    override = os.environ.get("CONTEXT_GURU_STATE")
    if override:
        return override
    base = os.environ.get("XDG_STATE_HOME") or os.path.join(os.path.expanduser("~"), ".local", "state")
    return os.path.join(base, "context-guru")


def _tips_file() -> str:
    base = os.environ.get("XDG_CONFIG_HOME") or os.path.join(os.path.expanduser("~"), ".config")
    return os.path.join(base, "context-guru", "statusline-tips")


def _tips_off() -> bool:
    """`statusline.py --set-tips=off` turns the tip off for good (`--hide=tip` does it per command)."""
    try:
        with open(_tips_file(), encoding="utf-8") as fh:
            return fh.read(8).strip() == "off"
    except OSError:
        return False


def _set_tips(value: str) -> str:
    if value not in ("on", "off"):
        return "usage: statusline.py --set-tips=on|off"
    path = _tips_file()
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w", encoding="utf-8") as fh:
        fh.write(value)
    return f"status line tip: {value}"


def _tip_segment(level: int | None = 0, now: float | None = None) -> str | None:
    """`Tip: you haven't run /context-guru:save-more for over a week — worth a shot.`, naming every
    command (of the ones this plugin actually has, a skills/<name>/SKILL.md) not run in the last
    TIP_DAYS; nothing when all ran this week. Level 1 is the short form for a narrow terminal, the
    one stale longest: `Tip: try /context-guru:save-more (not run for 7+ days) (+1)`.

    Local files only, no network: the commands record a run with `settings.py record-run <name>`
    into `<state dir>/last-run.json`, and this keeps the first time it saw the install in
    `<state dir>/statusline-first-seen`. A command that never ran counts from that first sighting,
    so a new install sees no tip for TIP_DAYS: the text says "for over a week", and before then
    that is not yet true. A last-run.json that cannot be read means no tip: an unknown history is
    not a stale one."""
    if level is None:
        return None
    now = _now_s() if now is None else now
    state = _update_check_state_dir()
    try:
        with open(os.path.join(state, "last-run.json"), encoding="utf-8") as fh:
            runs = json.loads(fh.read(4096))
    except FileNotFoundError:
        runs = {}
    except (OSError, ValueError):
        return None
    if not isinstance(runs, dict):
        return None
    seen_path = os.path.join(state, "statusline-first-seen")
    try:
        with open(seen_path, encoding="ascii") as fh:
            first = float(fh.read(32).strip())
    except (OSError, ValueError):
        first = now
        try:
            os.makedirs(state, exist_ok=True)
            with open(seen_path, "w", encoding="ascii") as fh:
                fh.write(str(int(now)))
        except OSError:
            pass
    skills = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "skills")
    idle = {c: now - max(_num(runs, c) or 0.0, first) for c in TIP_COMMANDS
            if os.path.isfile(os.path.join(skills, c, "SKILL.md"))}
    stale = sorted((c for c, age in idle.items() if age > TIP_DAYS * 86400), key=lambda c: -idle[c])
    if not stale:
        return None
    if level == 1:
        more = f" (+{len(stale) - 1})" if len(stale) > 1 else ""
        return f"{DIM}Tip: try /context-guru:{stale[0]} (not run for {TIP_DAYS}+ days){more}{RESET}"
    names = " or ".join(f"/context-guru:{c}" for c in TIP_COMMANDS if c in stale)
    return f"{DIM}Tip: you haven't run {names} for over a week — worth a shot.{RESET}"


def _update_available_segment() -> str | None:
    """A small, always-visible fallback for the SessionStart release notice.

    That notice only ever reaches the user if a model relays it, and a hook cannot force that —
    proven in the field: a real session's first message was unrelated, the note was never
    mentioned, and the user had no way to know an update had even been offered until they asked
    directly. This reads the same update-check.yaml record start-proxy.sh writes (a plain file,
    never a fetch — this script's one hard rule) and shows a small marker whenever a release
    exists that the user has not explicitly declined, independent of whether any note was ever
    relayed. `notified=` (a hook merely printed a note once) does NOT suppress this — only an
    actual `skipped=`/`never` answer does, because the whole point is to still be visible when
    the notice alone failed to reach the user.
    """
    state = _update_check_state_dir()
    try:
        with open(os.path.join(state, "update-check.yaml"), encoding="utf-8") as fh:
            text = fh.read(4096)
    except OSError:
        return None
    fields = {"answer": "ask", "skipped": "", "latest": ""}
    for line in text.splitlines():
        m = re.match(r"^(answer|skipped|latest):\s*(.*)$", line)
        if m:
            fields[m.group(1)] = m.group(2).strip()
    latest = fields["latest"]
    if not latest or fields["answer"] == "never" or latest == fields["skipped"]:
        return None
    try:
        with open(os.path.join(state, "proxy-version"), encoding="utf-8") as fh:
            installed = fh.readline().strip()
    except OSError:
        installed = ""
    if installed and (latest == installed or latest == f"v{installed}"):
        return None  # already current
    if fields["answer"] == "auto":
        return f"{GREEN}▲ {latest} auto{RESET}"
    return f"{GREEN}▲ update {latest}{RESET}"


def _keepalive_segment(stats: dict, money: bool = True) -> str | None:
    """This session's keep-alive pings: how many the proxy sent and what they cost (`ka 3 pings
    $0.04`). Nothing until the first ping. Whether a ping is still planned, and how long that holds
    the cache warm, is the cache element's `↻` clock, not this one. On a subscription (`money` False) the pings use
    limit, not dollars, so the count stays and the cost goes."""
    pings = _num(stats, "keepalive_pings")
    if not pings or pings < 1:
        return None
    usd = _num(stats, "keepalive_ping_usd")  # absent is unknown, not free
    cost = "" if usd is None or not money else " <$0.01" if 0 < usd < 0.005 else f" {_money(usd)}"  # a real cost never reads as $0.00
    return f"{DIM}ka{RESET} {int(pings)} ping{'s' if pings >= 2 else ''}{cost}"


# ---- redesigned elements ---------------------------------------------------------------------
# Layouts pick which elements show; --hide/--show toggle one element each; the width degrades them
# in a fixed order. Everything below is display logic over three inputs: Claude Code's stdin, the
# proxy's lean /api/stats (this session) and /api/cachestate (server cache truth).
LAYOUTS = {
    "minimal": ("save", "cache"),
    "balanced": ("update", "save", "cache", "next", "ctx", "ka", "tip"),
    "detailed": ("update", "save", "spend", "cache", "next", "ctx", "ka", "latency", "tools", "tip"),
    # The terminal views as one glanceable line (row 0) over the cache clock (row 1).
    "spark": ("spark", "cache"),   # saved today + 14 days of saving
    "strip": ("strip", "cache"),   # the last 20 requests as hit / partial / miss
    "heat": ("heat", "cache"),     # today's cache misses by hour
    "fill": ("fill", "cache"),     # context: always loaded vs conversation, time to full
    "watch": ("save", "clock0", "wagents"),  # saved, main clock, a mini-bar per sub-agent
}
ELEMENTS = {e for els in LAYOUTS.values() for e in els} | {"ka", "agents"}  # agents: opt-in with --show=agents; the mods show sub-agents
# Dropped/shrunk first to last when a line is wider than the terminal. The tip goes first (its short
# form, then the line): it is the least urgent thing on the line.
DEGRADE = (("tip", 1), ("tip", None), ("tools", None), ("next", 1), ("next", None), ("latency", None), ("ka", None), ("spend", None), ("update", None),
           ("spark", 1), ("strip", 1), ("heat", 1), ("fill", 1), ("wagents", 1),
           ("agents", 1), ("agents", 2), ("save", 1), ("ctx", 1), ("save", 2), ("cache", 1), ("agents", None), ("ctx", None),
           ("save", None))
DEFAULT_WIDTH = 100
ROUTE_MEMO_SECONDS = 60
EXPIRING_SECONDS = 60
CRITICAL_SECONDS = 15  # the last stretch: red, and a "!" so it is not colour alone
SPEND_RATE_MIN_SECONDS = 300  # a $/h figure over less than five minutes of wall clock is noise
HINT_FULL_FRACTION = 0.85  # the context bar's `⚠ rewrite near` from here
TIP_DAYS = 7  # a command not run for this long is named in the tip
TIP_COMMANDS = ("save-more", "remove-mcp-and-skills")
CACHESTATE_TTL_SECONDS = 10.0  # CONTRACT section 4: at least 10 s
# A cachestate read that misses the render budget starts at most one detached refresher per this
# many seconds, so the copy a slow proxy leaves stays about this fresh (#570).
CACHESTATE_REFRESH_WINDOW_SECONDS = 5.0
DAY_TTL_SECONDS = 60.0
WINDOW_TTL_SECONDS = 30.0
CACHE_BAR_CELLS = 6
AGENT_ROWS = 5  # named sub-agents shown before `+N more`
ACTIVE_SECONDS = 10  # a sub-agent whose last request ended this recently is marked `active`
SERIES_TTL_SECONDS = 120.0  # day and hour totals move slowly; a stale one is fine for a glance
REQUESTS_TTL_SECONDS = 4.0
STRIP_REQUESTS = 20
# Which line each element sits on in a two-line layout. `minimal` ignores this: it is one line.
ROW = {"spark": 0, "strip": 0, "heat": 0, "fill": 0, "clock0": 0, "wagents": 1,
       "update": 0, "save": 0, "spend": 0, "cache": 1, "next": 1, "agents": 1, "ctx": 1, "ka": 1, "latency": 1, "tools": 1, "tip": 2}

_ANSI_RE = re.compile(r"\033\[[0-9;]*m")


def _no_color() -> bool:
    return bool(os.environ.get("NO_COLOR")) or os.environ.get("TERM") == "dumb"


if _no_color():
    RESET = GREEN = YELLOW = RED = DIM = CYAN = ""


def _config(argv: list[str]) -> tuple[str, set[str], int]:
    """(layout, enabled elements, width). Flags: --layout=minimal|balanced|detailed,
    --hide=a,b / --show=a,b (element names), --width=N. Env CG_STATUSLINE_LAYOUT / _HIDE / _SHOW
    / _COLUMNS do the same for people who cannot edit the installed command. Unknown names are
    ignored: a typo must not blank the line. The old --cache and --keepalive flags still work."""
    opt: dict[str, str] = {}
    for key in ("layout", "hide", "show", "width"):
        env = os.environ.get("CG_STATUSLINE_" + ("COLUMNS" if key == "width" else key.upper()))
        if env:
            opt[key] = env
    for a in argv:
        if a.startswith("--") and "=" in a:
            k, v = a[2:].split("=", 1)
            if k in ("layout", "hide", "show", "width"):
                opt[k] = v
    chosen = _saved_layout()  # a layout picked with --set-layout outranks the installed command's flag
    layout = chosen or (opt.get("layout") if opt.get("layout") in LAYOUTS else "balanced")
    on = set(LAYOUTS[layout])
    on |= {e for e in opt.get("show", "").split(",") if e in ELEMENTS}
    on -= set(opt.get("hide", "").split(","))
    if "--cache" in argv:
        on.add("cache")
    if "--keepalive" in argv:
        on.add("ka")
    return layout, on, _width(opt.get("width"))


def _layout_file() -> str:
    base = os.environ.get("XDG_CONFIG_HOME") or os.path.join(os.path.expanduser("~"), ".config")
    return os.path.join(base, "context-guru", "statusline-layout")


def _saved_layout() -> str | None:
    try:
        with open(_layout_file(), encoding="utf-8") as fh:
            name = fh.read(32).strip()
    except OSError:
        return None
    return name if name in LAYOUTS else None


def _set_layout(name: str) -> str:
    """`--set-layout=<layout>|auto`: remember a layout across renders (the
    one-key toggle: bind it to a /command or a shell alias). `auto` forgets it again."""
    path = _layout_file()
    if name == "auto":
        try:
            os.unlink(path)
        except OSError:
            pass
        return "status line layout: back to the installed default"
    if name not in LAYOUTS:
        return "usage: statusline.py --set-layout=" + "|".join(LAYOUTS) + "|auto"
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w", encoding="utf-8") as fh:
        fh.write(name)
    return f"status line layout: {name}"


def _width(override: str | None) -> int:
    if override and override.isdigit() and int(override) > 0:
        return int(override)
    cols = os.environ.get("COLUMNS", "")  # Claude Code exports the real terminal width to the command
    if cols.isdigit() and int(cols) > 0:
        return int(cols)
    for fd in (2, 1, 0):  # otherwise: a statusLine command's stdout is a pipe; stderr may be the tty
        try:
            return os.get_terminal_size(fd).columns
        except OSError:
            pass
    return DEFAULT_WIDTH


def _vlen(s: str) -> int:
    return len(_ANSI_RE.sub("", s))


def _money(x: float, approx: bool = False) -> str:
    sign = "-" if x < -0.005 else ""
    return f"{'≈' if approx else ''}{sign}${abs(x):.2f}"


def _fetch_json(port: str, query: str, tag: str, ttl: float,
                refresh_window: float | None = None) -> tuple[dict | None, float]:
    """(body, age_seconds) for one cheap GET, through a disk cache. A failed fetch serves the last
    body up to STALE_STATS_MAX_AGE_SECONDS old WITH its age, so the caller can tell fresh from
    stale; nothing cached and nothing fetched is (None, 0). With refresh_window, a failed fetch also
    starts one detached refresher per window (see _bg_json): a read that is slower than the render
    budget then still lands in the cache for a later render, instead of failing on every render
    until the copy is too old to show (#570). Never raises."""
    path = os.path.join(_tmpdir(), f"context-guru-statusline-{port}-{tag}.json")
    cached, age = None, 0.0
    try:
        age = max(0.0, time.time() - os.stat(path).st_mtime)
        with open(path, encoding="utf-8") as fh:
            cached = json.load(fh)
        if age < ttl:
            return cached, age
    except (OSError, ValueError):
        cached = None
    if cgcache.backoff(path):  # failing lately: serve what we have, ask nothing
        return (cached, age) if cached is not None and age < STALE_STATS_MAX_AGE_SECONDS else (None, 0.0)
    try:
        body = _http_get(port, query)
        obj = json.loads(body)
        if not isinstance(obj, dict):
            raise ValueError
    except Exception as exc:  # noqa: BLE001 - any failure means "use what we have"
        slow = isinstance(exc, (socket.timeout, TimeoutError))
        cgcache.record_failure(path, "timeout" if slow else "error")
        if refresh_window and slow:  # only a SLOW answer can land later; an error would just fail again, a second request
            _spawn_refresh(port, query, path, refresh_window)
        return (cached, age) if cached is not None and age < STALE_STATS_MAX_AGE_SECONDS else (None, 0.0)
    cgcache.write_atomic(path, body)
    cgcache.clear_failure(path)
    return obj, 0.0


REFRESH_WINDOW_SECONDS = 30


def _take_refresh_slot(path: str, window: float = REFRESH_WINDOW_SECONDS) -> bool:
    """True for exactly ONE caller per refresh window, however many renders start at once. The lock
    file's name carries the window number and is created with O_EXCL, so the creation itself is the
    election: no stat-then-write gap, and no stale lock to clear (a lock left by a crashed refresher
    belongs to an earlier window and simply stops mattering). The previous window's file is removed
    best effort."""
    slot = int(time.time() // window)
    try:
        os.close(os.open(f"{path}.lock.{slot}", os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600))
    except FileExistsError:
        return False  # someone else is refreshing this key
    except OSError:
        return False  # cannot lock (read-only tmp): better no refresh than an unbounded number of them
    for old in (f"{path}.lock.{slot - 1}", path + ".lock"):  # last window's, and the pre-slot name
        try:
            os.unlink(old)
        except OSError:
            pass
    return True


def _bg_json(port: str, query: str, tag: str, ttl: float) -> tuple[dict | None, float]:
    """(body, age) for a read that can be too slow to wait for on a render (the day total, the
    series and the spend explanation take seconds on a large history). Fresh cache wins; otherwise
    one inline GET under the normal short timeout; and when that fails, serve whatever is cached however old and start ONE
    detached refresher (a lock file bounds it to one per 30 s) that fills the cache for a later
    render. Never waits longer than the inline timeout, never raises."""
    path = os.path.join(_tmpdir(), f"context-guru-statusline-{port}-{tag}.json")
    cached, age = None, 0.0
    try:
        age = max(0.0, time.time() - os.stat(path).st_mtime)
        with open(path, encoding="utf-8") as fh:
            cached = json.load(fh)
    except (OSError, ValueError):
        cached, age = None, 0.0
    if cached is not None and age < ttl:
        return cached, age
    if cached is None:  # nothing to show yet: one short try for a first answer; never when there IS a stale copy
        try:
            _refresh(port, query, path, 0.0)
            with open(path, encoding="utf-8") as fh:
                return json.load(fh), 0.0
        except Exception:  # noqa: BLE001 - too slow or down: the detached refresher fills it for a later render
            pass
    _spawn_refresh(port, query, path)
    return cached, age


def _spawn_refresh(port: str, query: str, path: str, window: float = REFRESH_WINDOW_SECONDS) -> None:
    """Start ONE detached refresher for `path` per window (the lock file elects it). Never raises."""
    if not _take_refresh_slot(path, window):
        return
    try:
        subprocess.Popen([sys.executable, os.path.abspath(__file__), "--refresh", port, query, path],
                         stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                         start_new_session=True)
    except OSError:
        pass


def _refresh(port: str, query: str, path: str, timeout: float = 60.0) -> None:
    """One GET written atomically to `path`. timeout 0 keeps the normal short bound (inline use);
    the detached refresher passes the default, patient one."""
    global HTTP_TIMEOUT_SECONDS, RENDER_BUDGET_SECONDS
    if timeout:
        HTTP_TIMEOUT_SECONDS, RENDER_BUDGET_SECONDS = timeout, 1e9  # the detached refresher is patient
    try:
        body = _http_get(port, query, None if timeout else COLD_READ_SECONDS)
        json.loads(body)
    except Exception:
        cgcache.record_failure(path, "error")
        raise
    cgcache.write_atomic(path, body)
    cgcache.clear_failure(path)
    if timeout:
        _sweep_tmp(path)


TMP_LEFTOVER_SECONDS = 60


def _sweep_tmp(path: str) -> None:
    """Remove `path`'s own .tmp and lock files older than TMP_LEFTOVER_SECONDS. A render's read runs
    on a daemon thread, and the process can exit in the middle of its write, which leaves a partial
    .tmp that nothing else removes. _take_refresh_slot removes only the previous window's lock, so a
    pause longer than one window leaves older ones. A lock that old belongs to a past window (all
    windows are shorter), so it no longer elects anything. Only the detached refresher sweeps: it
    has no render budget to spend."""
    d, prefix = os.path.split(path)
    prefix += "."
    try:
        with os.scandir(d or ".") as it:
            for e in it:
                rest = e.name[len(prefix):] if e.name.startswith(prefix) else ""
                if (rest.endswith(".tmp") and rest[:-4].isdigit()) or (rest.startswith("lock.") and rest[5:].isdigit()):
                    try:
                        if time.time() - e.stat().st_mtime > TMP_LEFTOVER_SECONDS:
                            os.unlink(e.path)
                    except OSError:
                        pass
    except OSError:
        pass


def _num(d: object, k: str) -> float | None:
    v = d.get(k) if isinstance(d, dict) else None
    return float(v) if isinstance(v, (int, float)) and not isinstance(v, bool) else None


def ttl_s_bad(cs: dict) -> bool:
    t = cs.get("ttl_s")
    return not isinstance(t, (int, float)) or isinstance(t, bool) or t <= 0


def remaining_seconds(entry: dict | None, age: float) -> float | None:
    """Seconds until one cache entry expires (negative = that long cold), from the SERVER's clock:
    the entry's last response end (or its start, when the end is unknown) plus its TTL, minus the
    server's `now_ms` sampled at fetch time, minus `age`, the seconds we have held that sample.
    Only the difference of two server timestamps and a local elapsed time is used, so a client
    clock that is minutes off changes nothing. None when the entry lacks the fields."""
    last, ttl, now_ms = _num(entry, "last_ts"), _num(entry, "ttl_s"), _num(entry, "now_ms")
    if last is None or ttl is None or ttl <= 0 or now_ms is None:
        return None
    end = _num(entry, "end_ts")
    return (max(last, end or 0) + ttl * 1000 - now_ms) / 1000 - age


def entry_state(rem: float, ttl: float) -> str:
    """cold | critical | expiring | warm: the one place the colour bands are decided."""
    if rem <= 0:
        return "cold"
    if rem <= CRITICAL_SECONDS:
        return "critical"
    return "expiring" if rem <= max(EXPIRING_SECONDS, ttl / 5) else "warm"


def _main_cs(cs: dict | None) -> dict | None:
    """/api/cachestate describes the session's latest cache touch on ANY thread, but a sub-agent has
    its own cache entry and a much smaller prompt: shown as the session's cache, a busy sub-agent
    would keep resetting the countdown and shrink the context bar. When the proxy lists threads,
    the main thread's own entry replaces those fields."""
    threads = cs.get("threads") if isinstance(cs, dict) else None
    main = next((t for t in threads if isinstance(t, dict) and t.get("main")), None) if isinstance(threads, list) else None
    if main is None:
        return cs
    merged = dict(cs)
    for k in ("model", "last_ts", "end_ts", "ttl_s", "prefix_tokens", "rewrite_usd", "priced"):
        if k in main:
            merged[k] = main[k]
    if main.get("last_ts") != cs.get("last_ts") and main.get("model") != cs.get("model"):
        # window/window_exact belong to the model of the latest touch, here a sub-agent's on another model;
        # against the main thread's prompt they made the context bar read 100%. Unknown for the main
        # model, so Claude Code's own size is used.
        merged.pop("window", None)
        merged.pop("window_exact", None)
    merged.pop("held_until_ms", None)  # the main thread's own plan, or none: never a sub-agent's
    if "held_until_ms" in main:
        merged["held_until_ms"] = main["held_until_ms"]
    merged["keepalive"] = bool(cs.get("keepalive")) and cs.get("last_ts") == main.get("last_ts")
    return merged


def held_seconds(entry: dict | None, now_ms: object, age: float) -> float | None:
    """Seconds until a cache the proxy plans to keep-alive goes cold (its last planned ping's start
    plus the TTL, `held_until_ms`), on the server's clock like remaining_seconds. None = no plan."""
    held, now = _num(entry, "held_until_ms"), _num({"n": now_ms}, "n")
    return None if held is None or now is None else (held - now) / 1000 - age


AGENT_META_BYTES = 4096  # a .meta.json is ~350 bytes; a longer one is not ours to parse
AGENT_META_SCAN = 20000  # directory entries looked at, at most, for teammate files (names only: ~7 ms)
AGENT_META_OPENS = 24  # meta files opened per render, at most (the proxy lists at most 16 threads)


def _read_meta(path: str) -> dict | None:
    try:
        with open(path, encoding="utf-8") as fh:
            meta = json.loads(fh.read(AGENT_META_BYTES))
    except (OSError, ValueError):
        return None
    return meta if isinstance(meta, dict) else None


def _meta_label(meta: dict | None) -> str:
    return next((v for v in (meta.get("name"), meta.get("agentType")) if isinstance(v, str) and v), "") \
        if meta else ""


def _agent_names(payload: dict, ids: list[str]) -> dict[str, str]:
    """Thread id -> the name Claude Code's agents list shows, for the `a:` ids the proxy listed,
    from this session's own sub-agent files (`<transcript minus .jsonl>/subagents/`). Claude Code
    names an agent two ways on the wire. A plain sub-agent's header is its file id, so its
    `agent-<id>.meta.json` is opened directly. A teammate's is `<name>@<team>`, and its file is
    `agent-a<name>-<hex>.meta.json`, so the directory is listed (names only, at most
    AGENT_META_SCAN entries) and only files with that prefix are opened, then checked against the
    team. At most AGENT_META_OPENS files are opened and AGENT_META_BYTES read from each, so a
    session with thousands of agents costs a render no more than one with a few."""
    tp = payload.get("transcript_path")
    if not isinstance(tp, str) or not tp.endswith(".jsonl") or not ids:
        return {}
    sub = os.path.join(tp[:-6], "subagents")
    names: dict[str, str] = {}
    opens = 0
    teammates: dict[str, list[str]] = {}  # file prefix -> the ids (name@team) it may be
    for tid in ids:
        aid = tid[2:]
        if not aid or "/" in aid or aid.startswith("."):
            continue
        if "@" in aid:
            name = aid.split("@", 1)[0]
            teammates.setdefault(f"agent-a{name}-", []).append(tid)
        elif opens < AGENT_META_OPENS:
            opens += 1
            label = _meta_label(_read_meta(os.path.join(sub, f"agent-{aid}.meta.json")))
            if label:
                names[tid] = label
    if not teammates:
        return names
    try:
        with os.scandir(sub) as it:
            for n, f in enumerate(it):
                if n >= AGENT_META_SCAN or opens >= AGENT_META_OPENS:
                    break
                if not f.name.endswith(".meta.json"):
                    continue
                prefix = next((p for p in teammates if f.name.startswith(p)), None)
                if prefix is None:
                    continue
                opens += 1
                meta = _read_meta(f.path)
                team = meta.get("teamName") if meta else None
                for tid in teammates[prefix]:
                    if isinstance(team, str) and tid.endswith("@" + team) and _meta_label(meta):
                        names[tid] = _meta_label(meta)
    except OSError:
        pass
    return names


def _agent_rows(cs: dict | None, cs_age: float, names: dict[str, str] | None = None) -> list[dict]:
    """Every sub-agent cache the proxy lists: {id, name, rem, ttl, held, ago}, in id order so a row
    stays on its agent. rem <= 0 is cold. held is the keep-alive plan's time until cold, or None.
    ago is the seconds since the agent's last request ended.

    Only `a:` threads, the ones Claude Code named with its agent-id header: it sends one on every
    sub-agent and teammate request (2.1.296). A `p:` thread is a request the proxy grouped by
    prefix, which from Claude Code is a side request (the auto-mode classifier, a title), not an
    agent the user could select."""
    threads = cs.get("threads") if isinstance(cs, dict) else None
    out = []
    for t in threads if isinstance(threads, list) else []:
        tid = str(t.get("id", "")) if isinstance(t, dict) else ""
        if not tid.startswith("a:") or ttl_s_bad(t):
            continue
        rem = remaining_seconds({**t, "now_ms": cs.get("now_ms")}, cs_age)
        if rem is None:
            continue
        last = max(_num(t, "last_ts") or 0, _num(t, "end_ts") or 0)
        out.append({"id": tid, "name": (names or {}).get(tid, ""), "rem": rem, "ttl": float(t["ttl_s"]),
                    "held": held_seconds(t, cs.get("now_ms"), cs_age),
                    "ago": (_num(cs, "now_ms") - last) / 1000 + cs_age,  # remaining_seconds needs now_ms too
                    "saved": _num(t, "saved_usd"), "baseline": _num(t, "baseline_usd")})
    out.sort(key=lambda r: r["id"])
    seen: dict[str, int] = {}
    for r in out:  # two `Explore` agents read `Explore` and `Explore 2`, fixed by id order
        if r["name"]:
            seen[r["name"]] = seen.get(r["name"], 0) + 1
            if seen[r["name"]] > 1:
                r["name"] = f"{r['name']} {seen[r['name']]}"
    return out


def _until_cold(rem: float, held: float | None) -> float:
    """Seconds until a cache goes cold: the keep-alive plan's horizon while the current entry is
    still warm, else the entry's own clock. A plan cannot revive an entry that already expired (a
    ping that failed, or never came), so a cold entry stays cold whatever is planned."""
    return held if held is not None and rem > 0 and held > rem else rem


def _agent_ids(cs: dict | None) -> list[str]:
    """The `a:` thread ids the proxy listed (at most 16): the only names a render needs."""
    threads = cs.get("threads") if isinstance(cs, dict) else None
    return [str(t["id"]) for t in threads if isinstance(t, dict) and str(t.get("id", "")).startswith("a:")] \
        if isinstance(threads, list) else []


def _warm(rows: list[dict]) -> list[dict]:
    return [r for r in rows if _until_cold(r["rem"], r["held"]) > 0]


def _cache_view(payload: dict, cs: dict | None, cs_age: float) -> dict:
    """Merge the two cache sources into {remaining, source, tokens, rewrite_usd}. remaining is
    seconds to expiry (<=0 cold) or None for unknown. The proxy's answer WINS when it is fresh
    (it saw every request, pings included); a stale one only wins while it is the later expiry,
    because a later client expiry means a request happened that the stale copy never saw."""
    pc = payload.get("prompt_cache") if isinstance(payload.get("prompt_cache"), dict) else {}
    cli = None
    exp = _num(pc, "expires_at")
    if exp is not None:
        cli = exp - time.time()
    elif pc.get("warm") is False:
        cli = 0.0
    srv, tokens, usd, ping = None, _num(pc, "recache_tokens_if_cold"), None, False
    ttl = 3600.0 if pc.get("ttl") == "1h" else 300.0
    srv = remaining_seconds(cs, cs_age)
    cold_usd = warm_usd = None
    if srv is not None:
        tokens = _num(cs, "prefix_tokens") or tokens
        usd = _num(cs, "rewrite_usd") if cs.get("priced") else None
        # CONTRACT section 2. An older proxy has neither field: the cold price is then rewrite_usd and the warm one unknown.
        cold_usd = _num(cs, "next_cold_usd") if cs.get("priced") else None
        warm_usd = _num(cs, "next_warm_usd") if cs.get("priced") else None
        if cold_usd is None:
            cold_usd = usd
        ttl, ping = ttl if ttl_s_bad(cs) else cs["ttl_s"], bool(cs.get("keepalive"))
    # ONE source drives the clock at a time. The old rule gave the proxy only the first 3 s of a copy and then
    # let Claude Code's own expiry win whenever it was later (it counts from a different start), so the
    # clock jumped up ~10 s and back down at the next refresh: "two alternating timers".
    if srv is not None and (cs_age <= PROXY_FRESH_SECONDS or cli is None or srv >= cli):
        return {"remaining": srv, "source": "proxy", "tokens": tokens, "usd": usd, "ttl": ttl, "ping": ping,
                "warm_usd": warm_usd, "cold_usd": cold_usd, "model": cs.get("model"), "held": held_seconds(cs, cs.get("now_ms"), cs_age)}
    if cli is not None:
        return {"remaining": cli, "source": "client", "tokens": tokens, "usd": None, "ttl": ttl, "ping": False}
    return {"remaining": None, "source": None, "tokens": tokens, "usd": usd, "ttl": ttl, "ping": False}


def _clock(sec: float) -> str:
    sec = int(max(0, sec))
    return f"{sec // 3600}h{sec % 3600 // 60:02d}m" if sec >= 3600 else f"{sec // 60}:{sec % 60:02d}"


def _cache_segment(view: dict, level: int | None = 0) -> str | None:
    """A bar that DRAINS toward expiry: full = just written, empty = gone. Green while warm, amber
    in the last minute (or last fifth of the lifetime), red for the last 15 s, then red and empty:
    the clock then counts UP how long it has been cold. `↻` marks a cache the proxy keeps warm
    with keep-alive pings: the clock is then the time until it goes COLD (the last planned ping
    plus the TTL), while the bar still drains over the current entry, so each ping refills it."""
    rem = view["remaining"]
    if level is None:
        return None
    if rem is None:
        return f"{DIM}◌ no cache yet{RESET}" if level == 0 else f"{DIM}◌{RESET}"
    if _until_cold(rem, view.get("held") if rem > 0 else None) <= 0:
        return None  # cold is noise: the user knows, so the segment is left out entirely
    ttl = view.get("ttl") or 300.0
    cells = max(0, min(CACHE_BAR_CELLS, int(round(rem / ttl * CACHE_BAR_CELLS)))) if rem > 0 else 0
    held = view.get("held") if rem > 0 else None  # an expired entry is cold, whatever is planned
    rem = _until_cold(rem, held)  # the clock and its colour follow the time until cold
    state = entry_state(rem, ttl)
    colour, glyph = _AGENT_STYLE[state]
    ping = " ↻" if (view.get("ping") or held is not None) and rem > 0 else ""
    if ping and state != "critical":
        colour = CYAN  # kept warm by pings: its own colour, so "pinged" reads at a glance
    if state == "cold":
        # A client-only answer knows it is cold but not since when: say so, don't invent a clock.
        clock = f"cold {_clock(-rem)}" if view.get("source") == "proxy" else "cold"
    else:
        clock = _clock(rem) + ("!" if state == "critical" else "")
    if level == 1:
        return f"{DIM}cache{RESET} {colour}{glyph} {clock}{ping}{RESET}"
    bar = f"{colour}{'█' * cells}{DIM}{'░' * (CACHE_BAR_CELLS - cells)}{RESET}"
    return f"{DIM}cache{RESET} {colour}{glyph}{RESET} {bar} {colour}{clock}{ping}{RESET}"


def _next_money(x: float) -> str:
    return "<$0.01" if 0 < x < 0.005 else _money(x)


def _next_segment(view: dict, level: int | None = 0) -> str | None:
    """What the NEXT request costs for the cached prefix: `next $0.03 warm · $0.30 cold` while the
    cache is warm (so the value of keeping it warm is on the line), just `next $0.30` once it is cold
    (the only price left). A proxy that predates the warm figure gives `next $0.30 if cold`. Nothing
    when the price is unknown: unknown is not $0.00."""
    cold, warm, rem = view.get("cold_usd"), view.get("warm_usd"), view.get("remaining")
    if level is None or cold is None or rem is None:
        return None
    if rem <= 0:
        return None  # cold: nothing is shown
    if warm is None:
        return f"{DIM}next{RESET} {_next_money(cold)} {DIM}if cold{RESET}"
    if level == 1:
        return f"{DIM}next{RESET} {_next_money(warm)}{DIM}/{RESET}{_next_money(cold)}"
    return f"{DIM}next{RESET} {_next_money(warm)} {DIM}warm ·{RESET} {_next_money(cold)} {DIM}cold{RESET}"


_AGENT_STYLE = {"cold": (RED, "○"), "critical": (RED, "◐"), "expiring": (YELLOW, "◐"), "warm": (GREEN, "●")}


def _agents_segment(rows: list[dict], level: int | None = 0) -> str | None:
    """Every sub-agent's cache under the name Claude Code's agents list shows, so the agent selected
    there is found by name (the status line is not told which one is selected): `agents reviewer
    active ● 4:58 saved $1.20 of $6.00 · tester ◐ 0:52 ↻ · +2 more · 1 cold`. Warm ones, most
    recently used first, at most AGENT_ROWS; `active` marks a request in the last ACTIVE_SECONDS;
    each clock is the time until cold, `↻` when pings hold it, then the agent's own `saved $z of
    $q` (see _money_pair) when the proxy priced it; cold ones are counted. Agents with no name on disk (not
    Claude Code, or an older one) keep the unlabelled form, `agents 2 · soonest ◐ 0:50`, because a
    list of clocks without names cannot say which agent is which. Level 1 drops the per-agent money,
    level 2 is the counts alone."""
    if level is None or not rows:
        return None
    warm = _warm(rows)
    cold = len(rows) - len(warm)
    tail = [f"{DIM}{cold} cold{RESET}"] if cold else []
    sep = f" {DIM}·{RESET} "

    def clock(r: dict) -> str:
        left = _until_cold(r["rem"], r["held"])
        colour, glyph = _AGENT_STYLE[entry_state(left, r["ttl"])]
        return f"{colour}{glyph} {_clock(left)}{' ↻' if left > r['rem'] else ''}{RESET}"

    if level == 2:
        return f"{DIM}agents{RESET} " + sep.join(([str(len(warm))] if warm else []) + tail)
    named = sorted((r for r in warm if r["name"]), key=lambda r: r["ago"])
    if not named:
        if not warm:
            return f"{DIM}agents{RESET} " + sep.join(tail)
        warm.sort(key=lambda r: _until_cold(r["rem"], r["held"]))
        head = f"{DIM}agent{RESET} {clock(warm[0])}" if len(warm) == 1 else \
            f"{DIM}agents{RESET} {len(warm)} {DIM}· soonest{RESET} {clock(warm[0])}"
        return sep.join([head] + tail)
    parts = []
    for r in named[:AGENT_ROWS]:
        name = r["name"] if len(r["name"]) <= 14 else r["name"][:13] + "…"
        active = f" {GREEN}active{RESET}" if r["ago"] <= ACTIVE_SECONDS else ""
        pair = _money_pair(r["saved"], r["baseline"]) if level == 0 else None
        parts.append(f"{name}{active} {clock(r)}" + (f" {pair}" if pair else ""))
    if len(warm) > len(parts):
        parts.append(f"{DIM}+{len(warm) - len(parts)} more{RESET}")
    return f"{DIM}agents{RESET} " + sep.join(parts + tail)


def _spend_segment(payload: dict, level: int | None = 0) -> str | None:
    """What this session has cost so far (Claude Code's own running total) and, once there is
    enough wall clock to mean something, the average $/h and the session's age."""
    cost = payload.get("cost")
    usd, ms = _num(cost, "total_cost_usd"), _num(cost, "total_duration_ms")
    if level is None or usd is None or usd <= 0:
        return None
    out = f"{DIM}spent{RESET} {_money(usd)}"
    if ms is not None and ms / 1000 >= SPEND_RATE_MIN_SECONDS:
        out += f" {DIM}·{RESET} {_money(usd / (ms / 3_600_000))}{DIM}/h{RESET} {DIM}· up{RESET} {_clock(ms / 1000)}"
    return out


def _ctx_numbers(payload: dict, cs: dict | None, cs_age: float) -> tuple[float, float] | None:
    """(used tokens, served window). Both prefer the proxy's own figures when it answered just
    now: the window when it is a MEASURED one for the model actually served, and the tokens of the
    last request it forwarded (Claude Code only updates its count after a response it accepts, so
    after a failed or refused turn its bar lags what was really sent)."""
    cw = payload.get("context_window")
    used = _num(cw, "total_input_tokens")
    window = _num(cw, "context_window_size")
    if cs_age <= CTX_PROXY_FRESH_SECONDS and (_num(cs, "prefix_tokens") or 0) > 0:
        used = _num(cs, "prefix_tokens")
    if isinstance(cs, dict) and cs.get("window_exact") and (_num(cs, "window") or 0) > 0:
        window = _num(cs, "window")
    if used is None:
        used = _num(cs, "prefix_tokens")
    if used is None or not window or window <= 0:
        return None
    return used, window


def _ctx_segment(used: float, window: float, level: int | None = 0) -> str | None:
    if level is None:
        return None
    fraction = max(0.0, min(1.0, used / window))
    colour = GREEN if fraction <= GREEN_MAX else (YELLOW if fraction <= YELLOW_MAX else RED)
    if level == 1:
        return f"{DIM}ctx{RESET} {colour}{fraction * 100:.0f}%{RESET}"
    filled = int(round(fraction * BAR_CELLS))
    bar = f"{colour}{'█' * filled}{DIM}{'░' * (BAR_CELLS - filled)}{RESET}"
    near = f" {RED}⚠ rewrite near{RESET}" if fraction >= HINT_FULL_FRACTION else ""
    return f"{DIM}ctx{RESET} {bar} {colour}{fraction * 100:.0f}%{RESET}{near}"


def _money_pair(x: float | None, y: float | None) -> str | None:
    """`saved $x of $y`, or None when either half is missing from the response (unknown is not
    $0.00) or nothing is priced yet (no whole to be part of)."""
    if x is None or y is None or y < 0.005:
        return None
    return f"saved {_money(x)} {DIM}of{RESET} {_money(y)}"


def _manual_ping_mark(stats: dict | None) -> str:
    """` ↻ $0.84`: what this session's manual pings saved (net of their own cost), once there is one."""
    mp = stats.get("manual_pings") if isinstance(stats, dict) else None
    usd = _num(mp, "saved_usd")
    return f" {CYAN}↻ {_money(usd)}{RESET}" if usd is not None and (_num(mp, "count") or 0) > 0 and usd >= 0.005 else ""


def _save_segment(stats: dict | None, money: dict | None, level: int | None = 0, fresh: bool = False) -> str | None:
    """`saved $0.42 of $3.10 session · saved $0.10 of $0.50 subagents`. session is the main
    thread, subagents every other thread of the session together (sub-agents, teammates, and side
    requests such as the auto-mode classifier, which cost money too). Both use one basis, from
    /api/cachestate's money (dash.threadMoney): $y is what those requests would have cost without
    context-guru, at the gateway's prices; $x is the saving against it, net of context-guru's own
    model calls and keep-alive pings, so $x <= $y. A saving is a COUNTERFACTUAL, shown plainly as
    money already saved. The subagents figure
    appears once a thread other than the main one exists. A figure whose fields are missing is
    left out, never shown as $0.00. An older proxy with no money block gets the bare `saved $x`
    from /api/stats."""
    if level is None or fresh:
        return None
    main = money.get("main") if isinstance(money, dict) else None
    if not isinstance(main, dict):
        sess = _num(stats, "total_saved_usd")
        if sess is None:
            return None
        return _money(sess) if level == 2 else f"saved {_money(sess)}"
    x, y = _num(main, "saved_usd"), _num(main, "baseline_usd")
    head = _money_pair(x, y)
    if head is None:
        return None
    if level == 2:
        return _money(x)
    if level == 1:
        return head + _manual_ping_mark(stats)
    out = f"{head} {DIM}session{RESET}" + _manual_ping_mark(stats)
    others = money.get("others")
    pair = _money_pair(_num(others, "saved_usd"), _num(others, "baseline_usd"))
    if (_num(others, "threads") or 0) > 0 and pair:
        out += f" {DIM}·{RESET} {pair} {DIM}subagents{RESET}"
    return out


WINDOW_SECONDS = 5 * 3600  # the length of Claude's short usage window (the `five_hour` limit)


def _subscription(payload: dict) -> dict | None:
    """The `five_hour` limit block of Claude Code's stdin payload, or None. Claude Code fills
    `rate_limits` only on a Claude subscription; with an API key, Bedrock or Vertex it is absent or
    null, and the status line keeps its money view."""
    limits = payload.get("rate_limits")
    five = limits.get("five_hour") if isinstance(limits, dict) else None
    used = _num(five, "used_percentage")
    if used is None or not 0 <= used <= 1000:
        return None
    reset = five.get("resets_at")
    if isinstance(reset, (int, float)) and not isinstance(reset, bool) and reset > 0:
        reset = float(reset) / 1000 if reset > 1e11 else float(reset)  # epoch seconds, or milliseconds
    else:
        reset = None
    week = _num(limits.get("seven_day"), "used_percentage")
    # Claude Code can keep the last block after the window ended, until the next response replaces
    # it. Its percentage and any totals since its start describe a window that is over.
    return {"used": used, "reset": reset, "week": week, "expired": reset is not None and reset <= _now_s()}


def _window_start_ms(sub: dict | None) -> int | None:
    """When the current 5-hour window began: its reset time minus its length."""
    reset = sub.get("reset") if sub and not sub.get("expired") else None
    return int((reset - WINDOW_SECONDS) * 1000) if reset else None


def _saved_tokens(win: dict | None) -> tuple[float, float] | None:
    """(read, write) tokens compaction kept out of this window's prompts, from /api/stats fields:

    * write = `saved_unique`: each distinct removal counted once. Sent as new content it would
      have been billed as a cache WRITE (the proxy prices it at the write rate).
    * read = `replay_tokens` (`saved_gross - saved_unique`): the same removals, absent again from
      every later turn that re-sends the transcript. Billed as a cache READ on a hit.

    These are the proxy's own token estimates, compaction only: cachesplit, keep-alive and
    declaration-filter savings are dollar figures with no token count, so they are not here.
    None when the fields are absent; a window with nothing saved reads (0, 0)."""
    write, read = _num(win, "saved_unique"), _num(win, "replay_tokens")
    if write is None and read is None:
        return None
    return max(read or 0.0, 0.0), max(write or 0.0, 0.0)


def _saved_tokens_text(win: dict | None) -> str | None:
    """`saved 1.2M rd / 85.0k wr`; a kind with nothing saved is left out; None when neither has any."""
    tokens = _saved_tokens(win)
    if not tokens:
        return None
    parts = [f"{_human(n)} {tag}" for n, tag in zip(tokens, ("rd", "wr")) if n >= 1]
    return "saved " + " / ".join(parts) if parts else None


def _window_segment(sub: dict, win: dict | None, level: int | None = 0, detailed: bool = False) -> str | None:
    """`5h 34% · saved 1.2M rd / 85.0k wr` on a subscription: the real used share of the 5-hour
    limit (Claude Code's own number), then the tokens context-guru kept out of this window, by
    kind. Plain counts, unweighted: Anthropic does not publish how the limit weights each kind.
    The detailed layout names what the counts cover (`compaction saved`). No money. At level 2 only
    the 5h share stays: the counts are the proxy's estimates and go first. An ended window shows
    nothing."""
    if level is None or sub.get("expired"):
        return None
    out = f"{DIM}5h{RESET} {sub['used']:.0f}%"
    saved = _saved_tokens_text(win)
    if level == 2 or not saved:
        return out
    if detailed and level == 0:
        if sub.get("week") is not None:
            out += f" {DIM}·{RESET} {DIM}7d{RESET} {sub['week']:.0f}%"
        saved = "compaction " + saved
    return f"{out} {DIM}·{RESET} {saved}"


def _fresh_session(payload: dict) -> bool:
    """Nothing spent and nothing sent yet: dividing a saving by that is nonsense, so say nothing.
    A real nonzero total with zero saved still prints."""
    cost, cw = payload.get("cost"), payload.get("context_window")
    usd, i, o = _num(cost, "total_cost_usd"), _num(cw, "total_input_tokens"), _num(cw, "total_output_tokens")
    return usd is None or i is None or o is None or (usd == 0 and i == 0 and o == 0)


def _truncate(s: str, width: int) -> str:
    """Cut a line to `width` visible cells (ANSI-safe: escapes cost nothing)."""
    if _vlen(s) <= width:
        return s
    out, n, i = [], 0, 0
    while i < len(s) and n < width - 1:
        m = _ANSI_RE.match(s, i)
        if m:
            out.append(m.group())
            i = m.end()
        else:
            out.append(s[i])
            n += 1
            i += 1
    return "".join(out) + "…" + RESET


# ---- terminal-view modes ---------------------------------------------------------------------
# Compact versions of the /context-guru terminal views. They draw with tvlib (imported only when one
# of these layouts is chosen, so the default layouts pay nothing) and read only what the proxy already
# serves, through the same disk-cached, never-blocking reads as everything above.

def _tv(width: int):
    import tvlib  # noqa: PLC0415 - lazy on purpose, see above
    return tvlib, tvlib.Env(color=not _no_color(), width=width)


def _rows(reqs: dict | None) -> list[dict]:
    rows = reqs.get("requests") if isinstance(reqs, dict) else None
    return sorted((r for r in rows if isinstance(r, dict) and not r.get("keepalive")), key=lambda r: r.get("ts", 0)) \
        if isinstance(rows, list) else []


def _lost_today(spend: dict | None, midnight: int) -> float | None:
    """Dollars cache misses cost today, from the proxy's own spend explanation (excess over a hit)."""
    days = spend.get("daily") if isinstance(spend, dict) else None
    if not isinstance(days, list):
        return None
    return sum(_num(d, "rewrite_usd") or 0.0 for d in days if (_num(d, "day") or 0) >= midnight - 3_600_000)


def _spark_row(day: dict | None, series: dict | None, lv: int, tv, e) -> str | None:
    today = _num(day, "total_saved_usd")
    buckets = series.get("buckets") if isinstance(series, dict) else None
    if today is None or not isinstance(buckets, list):
        return None
    # The proxy's buckets are aligned to the UNIX epoch (UTC), so a day bucket is a UTC day. Hourly
    # buckets are summed into the user's LOCAL days instead, each placed by its midpoint (so a zone
    # on a half hour puts a bucket where most of it falls).
    def local_day(ms: float) -> tuple[int, int, int]:
        lt = time.localtime(ms / 1000 + 1800)
        return lt.tm_year, lt.tm_mon, lt.tm_mday

    by_day: dict[tuple[int, int, int], float] = {}
    for b in buckets:
        if isinstance(b, dict) and _num(b, "ts"):
            by_day[local_day(b["ts"])] = by_day.get(local_day(b["ts"]), 0.0) + (_num(b, "saved_usd") or 0.0)
    vals = [by_day.get(local_day(_day_start_ms(n) + 12 * 3_600_000 - 1800_000), 0.0) for n in range(13, 0, -1)] + [today]  # today: same read as the number
    return f"{DIM}saved{RESET} {_money(today)} {DIM}today{RESET}" + \
        ("" if lv else f" {DIM}· last 14 days{RESET}") + f" {e.paint('ok', e.spark(vals))}"


def tv_wide(e) -> bool:
    return e.width >= 100


def _strip_row(reqs: dict | None, spend: dict | None, lv: int, tv, e, money: bool = True) -> str | None:
    rows = _rows(reqs)[-STRIP_REQUESTS:]
    if not rows:
        return None
    paint = {"hit": ("ok", e.g["hit"]), "part": ("warn", e.g["part"]), "miss": ("bad", e.g["miss"])}
    cells = "".join(e.paint(*paint[tv.kind(r)]) for r in rows)
    lost = _lost_today(spend, _day_start_ms()) if money else None
    if lost and lost >= 0.005:
        tail = f" {_money(lost)}" if lv else f"  {_money(lost)} {DIM}lost today{RESET}"
    else:
        tail = "" if lv or not money else f"  {DIM}no misses cost money today{RESET}"
    key = "" if lv else f"  {e.paint('bad', e.g['miss'])} {DIM}miss{RESET}" + (
        f" {e.paint('warn', e.g['part'])} {DIM}partial{RESET} {e.paint('ok', e.g['hit'])} {DIM}hit{RESET}" if tv_wide(e) else "")
    if lv:
        return f"{DIM}last {len(rows)}{RESET} " + cells + (f" {e.paint('bad', e.g['miss'])} {DIM}miss{RESET}" if lost and lost >= 0.005 else "") + \
            (f" {_money(lost)} {DIM}today{RESET}" if lost and lost >= 0.005 else "")
    return f"{DIM}last {len(rows)} requests{RESET} " + cells + key + tail


def _heat_row(hourly: dict | None, spend: dict | None, lv: int, tv, e) -> str | None:
    buckets = hourly.get("buckets") if isinstance(hourly, dict) else None
    if not isinstance(buckets, list):
        return None
    per = [0.0] * 24
    for b in buckets:
        if isinstance(b, dict) and _num(b, "ts"):
            per[time.localtime(b["ts"] / 1000 + 1800).tm_hour] += _num(b, "cache_misses") or 0.0
    top, now_h = max(per), time.localtime(_now_s()).tm_hour
    if top <= 0:
        return f"{e.paint('ok', 'no cache misses today')}"
    def cell(h: int) -> str:  # a shade glyph AND a background, so it reads with colour off
        if h > now_h:
            return f"{DIM}·{RESET}"
        lv = 0 if per[h] <= 0 else min(4, 1 + int(per[h] / top * 3.999))
        return e.heat(lv, e.g["ramp"][lv]) if lv else " "

    cells = "".join(cell(h) for h in range(24))
    lost = _lost_today(spend, _day_start_ms())
    tail = f"  {_money(lost)} {DIM}lost today{RESET}" if lost and lost >= 0.005 else ""
    return f"{DIM}misses{'/hour' if not lv else '/h'}{RESET} " + f"{DIM}0h{RESET}{cells}{DIM}24h{RESET}{tail}"


def _fill_row(reqs: dict | None, ctx: tuple[float, float] | None, lv: int, tv, e, width: int,
              miss_usd: float | None = None) -> str | None:
    if not ctx:
        return None
    used, window = ctx
    frac = max(0.0, min(1.0, used / window))
    main = [r for r in _rows(reqs) if r.get("thread_id", "") == ""]
    ctxs = [(r.get("fresh_input") or 0) + (r.get("cache_read") or 0) + (r.get("cache_write") or 0) for r in main]
    base = min([used] + ctxs) / window if ctxs else None  # the smallest recent prompt: what loads every time
    n = max(10, min(40, width - 36))
    fx, cv = ("=", "#") if e.ascii else ("▒", "█")
    bar = e.stack([(base, fx), (frac - base, cv)], n) if base is not None else e.bar(frac, n)
    used_part = bar.rstrip(e.g["empty"])
    kind_ = "bad" if frac >= 0.85 else "warn" if frac >= 0.65 else "ok"
    try:
        g = tv.growth(main)
    except (KeyError, TypeError):  # a row without the token columns: no estimate rather than a wrong one
        g = 0.0
    eta = (window - used) / g if g > 0 else None
    text = f"{DIM}context{RESET} {e.paint(kind_, used_part)}{DIM}{bar[len(used_part):]}{RESET} {frac * 100:.0f}%"
    extras = []
    if eta and eta < 6 * 3600:
        extras.append(f" {DIM}· full in{RESET} {tv.span(eta)}")
    if miss_usd:
        extras.append(f" {DIM}· a miss now costs{RESET} {_money(miss_usd)}")
    if base is not None:
        extras.append(f"  {DIM}{fx} always loaded {base * 100:.0f}% · {cv} conversation {(frac - base) * 100:.0f}%{RESET}")
    for x in extras if lv == 0 else []:  # as many as fit, most useful first
        if tv.vlen(text + x) <= width - 1:
            text += x
    return text


def _wagents_row(rows: list[dict], lv: int, tv, e) -> str:
    rows = _warm(rows)
    if not rows:
        return f"{DIM}no sub-agents running{RESET}"
    kinds = {"cold": "bad", "critical": "bad", "expiring": "warn", "warm": "ok"}
    out = []
    for r in rows[:4]:
        left = _until_cold(r["rem"], r["held"])
        k = kinds[entry_state(left, r["ttl"])]
        out.append(f"{e.bar(max(0.0, r['rem']) / r['ttl'], 4, k)} {e.paint(k, _clock(left))}")
    if len(rows) > 4:
        out.append(f"{DIM}+{len(rows) - 4} more{RESET}")
    head = f"{DIM}agent caches ({len(rows)}){RESET}"
    return head + (" " + "  ".join(out) if lv == 0 else "")


def _now_s() -> float:
    """The wall clock; CG_STATUSLINE_NOW (epoch seconds) pins it so the day-based views can be tested."""
    pinned = os.environ.get("CG_STATUSLINE_NOW", "")
    return float(pinned) if pinned.replace(".", "", 1).isdigit() else time.time()


def _day_start_ms(days_ago: int = 0) -> int:
    """Local midnight, `days_ago` days back (mktime normalises the day-of-month, DST included)."""
    lt = time.localtime(_now_s())
    return int(time.mktime((lt.tm_year, lt.tm_mon, lt.tm_mday - days_ago, 0, 0, 0, 0, 0, -1)) * 1000)


def _gather(port: str, session_id: str | None, on: set[str], win_start: int | None = None,
            subscription: bool = False) -> dict:
    """Run the (at most three) proxy reads in parallel under one shared deadline, so the worst
    case is one HTTP_TIMEOUT_SECONDS, not three. Each read is disk-cached; a render that finds all
    three fresh makes no network call at all."""
    out: dict = {}
    jobs = {"stats": lambda: _fetch_stats(port, session_id)}
    if session_id and on & {"save", "cache", "agents", "ctx", "fill", "clock0", "wagents"}:
        jobs["cs"] = lambda: _fetch_json(
            port, "/api/cachestate?session=" + _quote(session_id),
            "cs-" + session_id, CACHESTATE_TTL_SECONDS, CACHESTATE_REFRESH_WINDOW_SECONDS)
    now_ms, midnight = int(_now_s() * 1000), _day_start_ms()
    if "spark" in on:
        jobs["series"] = lambda: _bg_json(
            port, f"/api/series?bucket=3600000&since={_day_start_ms(13)}&until={now_ms}", "series14h", SERIES_TTL_SECONDS)
    if "heat" in on:
        jobs["hourly"] = lambda: _bg_json(
            port, f"/api/series?bucket=3600000&since={midnight}&until={now_ms}", "hourly", SERIES_TTL_SECONDS)
    if on & {"strip", "heat"} and not subscription:  # the spend explanation only feeds `lost today` dollars
        tz = time.localtime(_now_s()).tm_gmtoff // 60  # minutes east of UTC, as the spend explanation expects
        jobs["spend"] = lambda: _bg_json(
            port, f"/api/spend/explain?since={midnight}&tz={tz}&now={now_ms}", "spend-today", SERIES_TTL_SECONDS)
    if session_id and "strip" in on:
        jobs["reqs"] = lambda: _fetch_json(
            port, "/api/requests?limit=20&session=" + _quote(session_id), "req20-" + session_id, REQUESTS_TTL_SECONDS)
    if session_id and "fill" in on:
        jobs["reqs"] = lambda: _fetch_json(
            port, "/api/requests?limit=100&session=" + _quote(session_id), "req100-" + session_id, REQUESTS_TTL_SECONDS)
    if "spark" in on:
        jobs["day"] = lambda: _bg_json(
            port, f"/api/stats?lean=1&since={_day_start_ms()}", "day", DAY_TTL_SECONDS)
    if win_start is not None and "save" in on:  # subscription: totals since this 5-hour window began
        jobs["win"] = lambda: _bg_json(
            port, f"/api/stats?lean=1&since={win_start}", f"win-{win_start // 1000}", WINDOW_TTL_SECONDS)

    def run(name, fn):
        try:
            out[name] = fn()
        except Exception:  # noqa: BLE001 - a failed read is an absent element, never a failed render
            pass

    threads = [threading.Thread(target=run, args=kv, daemon=True) for kv in jobs.items()]
    for t in threads:
        t.start()
    deadline = time.monotonic() + _remaining() + 0.04  # a hair after the reads' own timeouts, so they report first
    for t in threads:
        t.join(max(0.0, deadline - time.monotonic()))
    return out


def main() -> None:
    port = _our_port()
    if port is None:
        return  # not routed through us: say nothing, exactly like the other hooks

    payload = _read_stdin_json()
    session_id = payload.get("session_id")
    if not isinstance(session_id, str) or not _SESSION_ID_RE.match(session_id):
        session_id = None  # absent, or not the shape Claude Code actually sends: don't trust it
    layout, on, width = _config(sys.argv[1:])

    sub = _subscription(payload)
    if sub:
        on = on - {"spark"}  # only dollars: a subscription pays in limit, so it has no spark row
    got = _gather(port, session_id, on, _window_start_ms(sub), sub is not None)
    stats, status = got.get("stats") or _stale_stats(port, session_id)
    update_seg = _update_available_segment()  # local file only - independent of the proxy
    notice = {
        STATS_DOWN: "cg ✕ proxy offline",  # unreachable: say so in words, nothing else is worth saying
        # Not `cg ✕ proxy offline`: the proxy may be healthy and merely slow on this one aggregate. It redraws on
        # its own with real numbers.
        STATS_HUNG: "cg? not responding",  # was working; every fetch since has timed out
    }.get(status, "cg: stats loading…" if status == STATS_TIMEOUT and stats is None else None)
    if notice:
        print(notice + (" | " + update_seg if update_seg else ""))
        return
    stale = status == STATS_TIMEOUT  # stats is real, but from a past fetch, not this one

    cs, cs_age = got.get("cs", (None, 0.0))
    day, _ = got.get("day", (None, 0.0))
    agent_ids = _agent_ids(cs) if on & {"agents", "wagents"} else []
    agent_rows = _agent_rows(cs, cs_age, _agent_names(payload, agent_ids) if agent_ids else None)
    cs = _main_cs(cs)
    view = _cache_view(payload, cs, cs_age)
    ctx = _ctx_numbers(payload, cs, cs_age)
    win, _ = got.get("win", (None, 0.0))
    shown = {
        "update": lambda lv: update_seg if lv == 0 else None,
        "save": lambda lv: _save_segment(stats, (cs or {}).get("money"), lv, _fresh_session(payload)),
        "cache": lambda lv: _cache_segment(view, lv),
        "next": lambda lv: _next_segment(view, lv) if not sub else None,
        "agents": lambda lv: _agents_segment(agent_rows, max(lv, 1) if sub and lv is not None else lv),  # no per-agent $ on a subscription
        "spark": lambda lv: _spark_row(day, got.get("series", (None,))[0], lv, *_tv(width)),
        "strip": lambda lv: _strip_row(got.get("reqs", (None,))[0], got.get("spend", (None,))[0], lv, *_tv(width), money=not sub),
        "heat": lambda lv: _heat_row(got.get("hourly", (None,))[0], got.get("spend", (None,))[0], lv, *_tv(width)),
        "fill": lambda lv: _fill_row(got.get("reqs", (None,))[0], ctx, lv, *_tv(width), width, None if sub else view["usd"]),
        "clock0": lambda lv: _cache_segment(view, lv),
        "wagents": lambda lv: _wagents_row(agent_rows, lv, *_tv(width)),
        "spend": lambda lv: _spend_segment(payload, lv) if lv == 0 and not sub else None,
        "ctx": lambda lv: _ctx_segment(*ctx, lv) if ctx else None,
        "ka": lambda lv: (_keepalive_segment(stats, not sub) if stats and lv == 0 else None),
        "latency": lambda lv: _latency_segment(stats) if lv == 0 else None,
        "tools": lambda lv: _recommend_segment(payload) if lv == 0 else None,
        "tip": lambda lv: tips.get(lv),
    }
    if sub:  # a subscription pays in limit, not dollars: the window view replaces the money view
        shown["save"] = lambda lv: _window_segment(sub, win, lv, layout == "detailed")
    order = [e for e in ("update", "save", "spend", "spark", "strip", "heat", "fill", "clock0", "wagents", "cache",
                         "next", "agents", "ctx", "ka", "latency", "tools", "tip") if e in on]
    level: dict[str, int | None] = {e: 0 for e in order}
    tips = {lv: _tip_segment(lv) for lv in (0, 1)} if "tip" in on and not _tips_off() else {}
    if layout in ("minimal", "watch"):
        level["save"] = 1  # minimal: `saved $0.31 of $4.10`, the main thread only
    two_rows = layout != "minimal"

    def build() -> list[str]:
        rows: list[list[str]] = [[], [], []]
        for e in order:
            seg = shown[e](level[e])
            if seg:
                rows[ROW[e] if two_rows else 0].append(seg)
        return [" │ ".join(r) for r in rows if r]

    def widest() -> int:
        return max((_vlen(r) for r in build()), default=0)

    def too_wide_rows() -> set[int]:
        if not two_rows:
            return {0} if widest() > width else set()
        rows = [" │ ".join(filter(None, (shown[e](level[e]) for e in order if ROW[e] == i))) for i in (0, 1, 2)]
        return {i for i, r in enumerate(rows) if _vlen(r) > width}

    for element, lv in DEGRADE:
        bad = too_wide_rows()
        if not bad:
            break
        if (ROW[element] if two_rows else 0) not in bad:
            continue  # only shrink what sits on a line that is actually too wide
        if element in level and (level[element] or 0) < (lv or 9):
            level[element] = lv
    sep = f" {DIM}│{RESET} "
    lines = [_truncate(r.replace(" │ ", sep), width) for r in build()]
    if stale and lines and shown["tip"](level.get("tip")) and len(lines) > 1:
        lines[-2] += f" {DIM}⏳{RESET}"  # on the numbers, not on the tip
    elif stale and lines:
        # A small, honest marker that every number is from the last successful fetch rather than
        # this render - never withheld, never dressed up as fresh.
        lines[-1] += f" {DIM}⏳{RESET}"
    print("\n".join(lines) or "cg · waiting for the first request")


if __name__ == "__main__":
    if len(sys.argv) == 2 and sys.argv[1].startswith("--set-layout="):
        print(_set_layout(sys.argv[1].split("=", 1)[1]))
        sys.exit(0)
    if len(sys.argv) == 2 and sys.argv[1].startswith("--set-tips="):
        print(_set_tips(sys.argv[1].split("=", 1)[1]))
        sys.exit(0)
    if len(sys.argv) == 5 and sys.argv[1] == "--refresh":
        try:
            _refresh(*sys.argv[2:])
        except Exception:
            pass
        sys.exit(0)
    try:
        if hasattr(signal, "SIGALRM"):  # POSIX only, same as the plugin's shell hooks
            signal.signal(signal.SIGALRM, _on_alarm)
            signal.setitimer(signal.ITIMER_REAL, RENDER_BUDGET_SECONDS + 0.05)  # last-resort net under the read deadlines
        main()
    except Exception:
        pass  # never a traceback on a user's terminal; silence is the correct failure mode here
    finally:
        if hasattr(signal, "SIGALRM"):
            signal.alarm(0)
    sys.exit(0)
