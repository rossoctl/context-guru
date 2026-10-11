#!/usr/bin/env python3
"""Claude Code statusLine command. Layouts (--layout=minimal|balanced|detailed) pick elements:
money saved (an `≈` estimate against the same request uncompacted, net of context-guru's own
spend) next to money spent (observed), the prompt-cache state (warm / expiring / cold, the proxy's
server-side truth overriding Claude Code's own guess), context fill against the window actually
served, and at most one actionable hint. See skills/statusline/SKILL.md for flags and mockups.

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

`cg!` means UNREACHABLE, specifically — refused, DNS failure, anything that is not "answered, just
not within 0.6s". A proxy that is merely slow (its own aggregate endpoint can legitimately take
some time under load, especially behind many concurrent readers — see dash/api.go's jsonCache)
renders the last good cached numbers with a small stale marker instead, or, with no cache yet to
fall back on, a short "stats loading…"; if a cache DOES exist but has gone stale enough that
every fetch since has still timed out, "cg? not responding" instead — reachable, unlike `cg!`,
but no longer plausibly just slow either. See _fetch_stats and its STATS_* result codes for all
four. This used to not be true: EVERY URLError/OSError, including a plain `TimeoutError`,
rendered `cg!`, so a slow-but-healthy proxy looked exactly like a dead one on every render until
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
import sys
import threading
import time

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
RESET, GREEN, YELLOW, RED, DIM = "\033[0m", "\033[32m", "\033[33m", "\033[31m", "\033[2m"

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
    """`proxy: Xms · upstream: Yms` — labelled so ContextGuru's own added latency cannot be
    misread as the provider's, or vice versa. Both are /api/stats' own averages
    (dash.Overview.CGLatencyMsAvg / UpstreamMsAvg) — measured, not derived here.
    """
    if not isinstance(stats, dict):
        return None
    proxy_ms = stats.get("cg_latency_ms_avg")
    upstream_ms = stats.get("upstream_ms_avg")
    if not isinstance(proxy_ms, (int, float)) or not isinstance(upstream_ms, (int, float)):
        return None
    return f"{DIM}proxy:{RESET} {proxy_ms:.0f}ms {DIM}·{RESET} {DIM}upstream:{RESET} {upstream_ms:.0f}ms"


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
    permanent `cg!` — a down-proxy warning — in a project whose ANTHROPIC_BASE_URL pointed at the
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
    # tempfile.gettempdir() without importing tempfile (~11 ms of a render that has a 50 ms budget).
    return os.environ.get("TMPDIR") or "/tmp"


class _HTTPError(OSError):
    """A non-2xx answer: the proxy is reachable, it just has nothing for us."""


def _quote(s: str) -> str:
    return re.sub(r"[^A-Za-z0-9_.~-]", lambda m: "%%%02X" % ord(m.group()), s)


def _http_get(port: str, path: str) -> bytes:
    """GET http://127.0.0.1:port/path, bounded by HTTP_TIMEOUT_SECONDS end to end. Raw socket
    rather than urllib.request: importing that costs ~24 ms per render, half the whole budget, for
    one loopback GET. HTTP/1.0 + Connection: close means a plain read-to-EOF with no chunking.
    Raises TimeoutError (slow), _HTTPError (non-2xx), OSError (unreachable)."""
    deadline = time.monotonic() + HTTP_TIMEOUT_SECONDS
    with socket.create_connection(("127.0.0.1", int(port)), timeout=HTTP_TIMEOUT_SECONDS) as sock:
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


# _fetch_stats' second return value. "down" is the only one that should ever render `cg!` — see
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

    url = "/api/stats?lean=1"
    if session_id:
        url += "&session=" + _quote(session_id)
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
        return _timeout_result(cached, cached_age)
    except _HTTPError:
        return None, STATS_EMPTY  # reached the proxy; it just has nothing (e.g. --dashboard is off)
    except OSError:
        return None, STATS_DOWN  # anything else unreachable: the proxy really is down

    try:
        stats = json.loads(body)
    except (ValueError, TypeError):
        return None, STATS_EMPTY
    if not isinstance(stats, dict):
        return None, STATS_EMPTY

    try:
        tmp = cache_path + f".{os.getpid()}.tmp"
        with open(tmp, "wb") as fh:
            fh.write(body)
        os.replace(tmp, cache_path)
    except OSError:
        pass  # caching is an optimization; failing to write one is not this script's problem
    return stats, STATS_OK


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


def _keepalive_segment(stats: dict) -> str | None:
    """The ONE saving with a cause the developer did nothing to earn: cache misses the keep-alive
    pings PREVENTED, and the NET money that saved (credit minus what the pings themselves cost).
    Shown only when that net is positive — a net loss or a never-recorded account says nothing,
    same rule as the IBM deployment's renderer.
    """
    net = stats.get("keepalive_net_usd")
    if not isinstance(net, (int, float)) or net <= 0:
        return None
    prevented = stats.get("keepalive_misses_avoided", 0) or 0
    parts = "ka"
    if prevented >= 1:
        parts += f" ≤{int(prevented)}miss"
    return f"{parts} ${net:.2f}"


# ---- redesigned elements ---------------------------------------------------------------------
# Layouts pick which elements show; --hide/--show toggle one element each; the width degrades them
# in a fixed order. Everything below is display logic over three inputs: Claude Code's stdin, the
# proxy's lean /api/stats (session + today) and /api/cachestate (server cache truth).
LAYOUTS = {
    "minimal": ("save", "cache", "hint"),
    "balanced": ("update", "save", "cache", "ctx", "hint"),
    "detailed": ("update", "save", "cache", "ctx", "latency", "tools", "hint"),
}
ELEMENTS = {e for els in LAYOUTS.values() for e in els} | {"ka"}
# Dropped/shrunk first to last when the line is wider than the terminal. The hint is never in this
# list: it is the one thing worth the space, and is truncated instead.
DEGRADE = (("tools", None), ("latency", None), ("ka", None), ("update", None), ("hint", 1), ("save", 1),
           ("ctx", 1), ("save", 2), ("ctx", None), ("save", None), ("cache", 1))
DEFAULT_WIDTH = 100
ROUTE_MEMO_SECONDS = 60
EXPIRING_SECONDS = 60
HINT_COLD_MIN_TOKENS = 100_000  # a cold cache under this is cheap enough not to nag about
HINT_FULL_FRACTION = 0.85
CACHESTATE_TTL_SECONDS = 2.0
DAY_TTL_SECONDS = 60.0

_ANSI_RE = re.compile(r"\033\[[0-9;]*m")


def _no_color() -> bool:
    return bool(os.environ.get("NO_COLOR")) or os.environ.get("TERM") == "dumb"


if _no_color():
    RESET = GREEN = YELLOW = RED = DIM = ""


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
    layout = opt.get("layout") if opt.get("layout") in LAYOUTS else "balanced"
    on = set(LAYOUTS[layout])
    on |= {e for e in opt.get("show", "").split(",") if e in ELEMENTS}
    on -= set(opt.get("hide", "").split(","))
    if "--cache" in argv:
        on.add("cache")
    if "--keepalive" in argv:
        on.add("ka")
    return layout, on, _width(opt.get("width"))


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


def _fetch_json(port: str, query: str, tag: str, ttl: float) -> tuple[dict | None, float]:
    """(body, age_seconds) for one cheap GET, through a disk cache. A failed fetch serves the last
    body up to STALE_STATS_MAX_AGE_SECONDS old WITH its age, so the caller can tell fresh from
    stale; nothing cached and nothing fetched is (None, 0). Never raises."""
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
    try:
        body = _http_get(port, query)
        obj = json.loads(body)
        if not isinstance(obj, dict):
            raise ValueError
    except Exception:  # noqa: BLE001 - any failure means "use what we have"
        return (cached, age) if cached is not None and age < STALE_STATS_MAX_AGE_SECONDS else (None, 0.0)
    try:
        tmp = path + f".{os.getpid()}.tmp"
        with open(tmp, "wb") as fh:
            fh.write(body)
        os.replace(tmp, path)
    except OSError:
        pass
    return obj, 0.0


def _num(d: object, k: str) -> float | None:
    v = d.get(k) if isinstance(d, dict) else None
    return float(v) if isinstance(v, (int, float)) and not isinstance(v, bool) else None


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
    srv, tokens, usd = None, _num(pc, "recache_tokens_if_cold"), None
    last, ttl, now_ms = _num(cs, "last_ts"), _num(cs, "ttl_s"), _num(cs, "now_ms")
    if last is not None and ttl is not None and now_ms is not None:
        srv = (last + ttl * 1000 - now_ms) / 1000 - cs_age
        tokens = _num(cs, "prefix_tokens") or tokens
        usd = _num(cs, "rewrite_usd") if cs.get("priced") else None
    if srv is not None and (cs_age <= 3 or cli is None or srv >= cli):
        return {"remaining": srv, "source": "proxy", "tokens": tokens, "usd": usd}
    if cli is not None:
        return {"remaining": cli, "source": "client", "tokens": tokens, "usd": None}
    return {"remaining": None, "source": None, "tokens": tokens, "usd": usd}


def _cache_segment(view: dict, level: int | None = 0) -> str | None:
    rem = view["remaining"]
    if level is None:
        return None
    if rem is None:
        return f"{DIM}◌ no cache yet{RESET}" if level == 0 else f"{DIM}◌{RESET}"
    if rem <= 0:
        return f"{RED}○ cold{RESET}"
    if rem <= EXPIRING_SECONDS:
        return f"{YELLOW}◐ {int(rem)}s left{RESET}" if level == 0 else f"{YELLOW}◐ {int(rem)}s{RESET}"
    mins = int(rem // 60)
    when = f"{mins}m" if mins < 60 else f"{mins // 60}h{mins % 60:02d}m"
    return f"{GREEN}● warm {when}{RESET}" if level == 0 else f"{GREEN}● {when}{RESET}"


def _ctx_numbers(payload: dict, cs: dict | None, cs_age: float) -> tuple[float, float] | None:
    """(used tokens, served window). Both prefer the proxy's own figures when it answered just
    now: the window when it is a MEASURED one for the model actually served, and the tokens of the
    last request it forwarded (Claude Code only updates its count after a response it accepts, so
    after a failed or refused turn its bar lags what was really sent)."""
    cw = payload.get("context_window")
    used = _num(cw, "total_input_tokens")
    window = _num(cw, "context_window_size")
    if cs_age <= 3 and (_num(cs, "prefix_tokens") or 0) > 0:
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
        return f"{colour}ctx {fraction * 100:.0f}%{RESET}"
    filled = int(round(fraction * BAR_CELLS))
    bar = f"{colour}{'█' * filled}{DIM}{'·' * (BAR_CELLS - filled)}{RESET}"
    return f"{bar} {colour}{_human(used)}/{_human(window)} {fraction * 100:.0f}%{RESET}"


def _save_segment(stats: dict | None, day: dict | None, level: int | None = 0, fresh: bool = False) -> str | None:
    """Saved is a COUNTERFACTUAL (vs the same request uncompacted, at gateway prices, net of CG's
    own model spend and keep-alive pings), so it carries `≈`; spent is OBSERVED (gateway-priced
    cost of the requests actually sent). The two are never added or divided into each other."""
    saved = _num(stats, "total_saved_usd")
    if saved is None or level is None or fresh:
        return None
    if level == 2:
        return _money(saved, True)
    if level == 1:
        return f"saved {_money(saved, True)}"
    out = f"saved {_money(saved, True)}"
    spent = _num(stats, "cost_usd")
    if spent is not None:
        out += f" {DIM}(spent{RESET} ${spent:.2f}{DIM}){RESET}"
    today = _num(day, "total_saved_usd")
    if today is not None:
        out += f" {DIM}· day{RESET} {_money(today, True)}"
    return out


def _fresh_session(payload: dict) -> bool:
    """Nothing spent and nothing sent yet: dividing a saving by that is nonsense, so say nothing.
    A real nonzero total with zero saved still prints."""
    cost, cw = payload.get("cost"), payload.get("context_window")
    usd, i, o = _num(cost, "total_cost_usd"), _num(cw, "total_input_tokens"), _num(cw, "total_output_tokens")
    return usd is None or i is None or o is None or (usd == 0 and i == 0 and o == 0)


def _hint(payload: dict, view: dict, ctx: tuple[float, float] | None) -> tuple[str, str] | None:
    """At most ONE actionable line (long, short form), most expensive first."""
    rem, tokens = view["remaining"], view["tokens"]
    if rem is not None and rem <= 0 and tokens and tokens >= HINT_COLD_MIN_TOKENS:
        price = f"≈${view['usd']:.2f}" if view["usd"] is not None else f"{_human(tokens)} tokens"
        return (f"{RED}cold on {_human(tokens)}: next send rewrites {price} — /compact?{RESET}",
                f"{RED}cold {_human(tokens)}: resend {price} — /compact?{RESET}")
    if ctx and ctx[0] / ctx[1] >= HINT_FULL_FRACTION:
        full = f"{RED}context {ctx[0] / ctx[1] * 100:.0f}% full — /compact{RESET}"
        return full, full
    pc = payload.get("prompt_cache") if isinstance(payload.get("prompt_cache"), dict) else {}
    at, cause = _num(pc, "last_miss_at"), pc.get("last_miss_cause")
    causes = cause.get("causes") if isinstance(cause, dict) else None
    if at is not None and time.time() - at < 120 and isinstance(causes, list) and causes:
        miss = f"{YELLOW}last miss: {','.join(str(c) for c in causes[:2])}{RESET}"
        return miss, miss
    return None


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


def _compose(parts: list[str], width: int) -> str:
    return _truncate(f" {DIM}│{RESET} ".join(parts), width)


def _day_start_ms() -> int:
    lt = time.localtime()
    return int(time.mktime((lt.tm_year, lt.tm_mon, lt.tm_mday, 0, 0, 0, 0, 0, -1)) * 1000)


def _gather(port: str, session_id: str | None, on: set[str]) -> dict:
    """Run the (at most three) proxy reads in parallel under one shared deadline, so the worst
    case is one HTTP_TIMEOUT_SECONDS, not three. Each read is disk-cached; a render that finds all
    three fresh makes no network call at all."""
    out: dict = {}
    jobs = {"stats": lambda: _fetch_stats(port, session_id)}
    if session_id and on & {"cache", "ctx", "hint"}:
        jobs["cs"] = lambda: _fetch_json(
            port, "/api/cachestate?session=" + _quote(session_id),
            "cs-" + session_id, CACHESTATE_TTL_SECONDS)
    if "save" in on:
        jobs["day"] = lambda: _fetch_json(
            port, f"/api/stats?lean=1&since={_day_start_ms()}", "day", DAY_TTL_SECONDS)

    def run(name, fn):
        try:
            out[name] = fn()
        except Exception:  # noqa: BLE001 - a failed read is an absent element, never a failed render
            pass

    threads = [threading.Thread(target=run, args=kv, daemon=True) for kv in jobs.items()]
    for t in threads:
        t.start()
    deadline = time.monotonic() + HTTP_TIMEOUT_SECONDS + 0.2
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

    got = _gather(port, session_id, on)
    stats, status = got.get("stats", (None, STATS_TIMEOUT))
    update_seg = _update_available_segment()  # local file only - independent of the proxy
    notice = {
        STATS_DOWN: "cg!",  # three characters: the proxy is unreachable, nothing else is worth saying
        # Not `cg!`: the proxy may be healthy and merely slow on this one aggregate. It redraws on
        # its own with real numbers.
        STATS_HUNG: "cg? not responding",  # was working; every fetch since has timed out
    }.get(status, "cg: stats loading…" if status == STATS_TIMEOUT and stats is None else None)
    if notice:
        print(notice + (" | " + update_seg if update_seg else ""))
        return
    stale = status == STATS_TIMEOUT  # stats is real, but from a past fetch, not this one

    cs, cs_age = got.get("cs", (None, 0.0))
    day, _ = got.get("day", (None, 0.0))
    view = _cache_view(payload, cs, cs_age)
    ctx = _ctx_numbers(payload, cs, cs_age)
    shown = {
        "update": lambda lv: update_seg if lv == 0 else None,
        "save": lambda lv: _save_segment(stats, day, lv, _fresh_session(payload)),
        "cache": lambda lv: _cache_segment(view, lv),
        "ctx": lambda lv: _ctx_segment(*ctx, lv) if ctx else None,
        "ka": lambda lv: (_keepalive_segment(stats) if stats and lv == 0 else None),
        "latency": lambda lv: _latency_segment(stats) if lv == 0 else None,
        "tools": lambda lv: _recommend_segment(payload) if lv == 0 else None,
    }
    order = [e for e in ("update", "save", "cache", "ctx", "ka", "latency", "tools") if e in on]
    level: dict[str, int | None] = {e: 0 for e in order}
    hint = _hint(payload, view, ctx) if "hint" in on else None
    if layout == "minimal":
        level["save"] = 1  # minimal: `saved ≈$0.31`, no spent/day
    hint_level = 0

    def build() -> list[str]:
        parts = [s for s in (shown[e](level[e]) for e in order) if s]
        if hint:
            parts.append(f"{DIM}▸{RESET} {hint[hint_level]}")
        return parts

    parts = build()
    for element, lv in DEGRADE:
        if _vlen(" │ ".join(parts)) <= width:
            break
        if element == "hint":
            hint_level = lv
            parts = build()
        elif element in level and (level[element] or 0) < (lv or 9):
            level[element] = lv
            parts = build()
    line = _compose(parts, width)
    if stale and line:
        # A small, honest marker that every number to its left is from the last successful
        # fetch rather than this render - never withheld, never dressed up as fresh.
        line += f" {DIM}⏳{RESET}"
    print(line or "cg · waiting for the first request")


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
