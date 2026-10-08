#!/usr/bin/env python3
"""Claude Code statusLine command: by default, render what THIS session saved against what it
has spent so far, plus the context-window bar and the proxy/upstream latency split. The
prompt-cache TTL countdown and the keep-alive ping counter are opt-in extras (--cache /
--keepalive) — see skills/statusline/SKILL.md.

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

import glob
import json
import os
import re
import signal
import socket
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


def _context_segment(payload: dict) -> str | None:
    """The context-window bar: tokens used against the model's real window, coloured by pressure.

    Same fields and thresholds as the IBM deployment's renderer (context_window.total_input_tokens
    / .context_window_size / .used_percentage) — Claude Code's own statusLine payload, no fetch.
    """
    cw = payload.get("context_window")
    if not isinstance(cw, dict):
        return None
    used = cw.get("total_input_tokens")
    window = cw.get("context_window_size")
    if not isinstance(used, (int, float)) or not isinstance(window, (int, float)) or window <= 0:
        return None
    percent = cw.get("used_percentage")
    fraction = (percent / 100.0) if isinstance(percent, (int, float)) else (used / window)
    fraction = max(0.0, min(1.0, fraction))
    colour = GREEN if fraction <= GREEN_MAX else (YELLOW if fraction <= YELLOW_MAX else RED)
    filled = int(round(fraction * BAR_CELLS))
    bar = f"{colour}{'█' * filled}{DIM}{'·' * (BAR_CELLS - filled)}{RESET}"
    return f"{bar} {colour}{_human(used)}/{_human(window)} {fraction * 100:.0f}%{RESET}"


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
    try:
        sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
        import settings  # noqa: PLC0415 - see docstring
        port, _source = settings.resolve_routed_port()
    except Exception:  # noqa: BLE001 - a status line must never raise
        return None
    return port if port and port.isdigit() else None


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

    url = f"http://127.0.0.1:{port}/api/stats?lean=1"
    if session_id:
        url += "&session=" + urllib.parse.quote(session_id, safe="")
    try:
        with urllib.request.urlopen(url, timeout=HTTP_TIMEOUT_SECONDS) as resp:
            body = resp.read(1 << 20)  # bounded: the real payload is a few KB
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
    except urllib.error.HTTPError:
        return None, STATS_EMPTY  # reached the proxy; it just has nothing (e.g. --dashboard is off)
    except urllib.error.URLError as e:
        if isinstance(e.reason, (socket.timeout, TimeoutError)):
            # The CONNECT-phase timeout: urlopen wraps it in a URLError instead of raising it
            # directly (unlike the read-phase case above), so it has to be unwrapped here rather
            # than caught by the clause above — same meaning either way, the clock ran out, not a
            # refusal. Rare on loopback (a refused connect fails instantly), but a saturated
            # accept queue on a healthy-but-overloaded proxy can produce exactly this.
            return _timeout_result(cached, cached_age)
        return None, STATS_DOWN  # refused, DNS failure, or anything else NOT a timeout
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
    behind Claude Code's own `/context` view. It is a snapshot, NOT a running total: a long
    session with subagents or a compaction processes far more than it. So it only gates the
    fresh-session check in _default_segment and is never printed next to a running sum. Returns (None, None) where the payload cannot support
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
    is ever written). A real, nonzero total with zero saved DOES still print (`$0.00 of ...
    (-0)`), same reasoning as the old segment's negative-net case: a real $0 saved this session is
    not the same fact as no session having happened yet.
    """
    total_usd, total_tokens = _session_totals(payload)
    if total_usd is None or (total_usd == 0 and total_tokens == 0):
        return None
    if stats is None:  # no session-scoped savings figure to pair the total with
        return None
    saved_usd = stats.get("total_saved_usd", 0) or 0
    saved_tokens = stats.get("saved_unique", 0) or 0
    # No token total after the cost: context_window's pair is the CURRENT window size (the
    # context bar already shows it), while saved_unique is a running sum over every request of
    # the session — printing one "of" the other read as 750k removed out of 447k.
    return f"${saved_usd:.2f} of ${total_usd:.2f} (-{_human(saved_tokens)})"


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


def _cache_segment_enabled() -> bool:
    return "--cache" in sys.argv[1:]


def _keepalive_segment_enabled() -> bool:
    return "--keepalive" in sys.argv[1:]


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


def main() -> None:
    port = _our_port()
    if port is None:
        return  # not routed through us: say nothing, exactly like the other hooks

    payload = _read_stdin_json()
    session_id = payload.get("session_id")
    if not isinstance(session_id, str) or not _SESSION_ID_RE.match(session_id):
        session_id = None  # absent, or not the shape Claude Code actually sends: don't trust it

    stats, status = _fetch_stats(port, session_id)
    if status == STATS_DOWN:
        line = "cg!"  # three characters: the proxy is unreachable, nothing else is worth saying
        update_seg = _update_available_segment()  # local file only — independent of the proxy
        if update_seg:
            line += " | " + update_seg
        print(line)
        return
    if status == STATS_TIMEOUT and stats is None:
        # The proxy did not answer in time and there is no recent-enough cached answer to fall
        # back on (the common case right after the proxy itself restarted). This is NOT `cg!`:
        # the proxy may be perfectly healthy and merely slow on this one aggregate — see
        # _fetch_stats' docstring — and claiming it is down when it is not sends whoever reads
        # this looking in the wrong place. It will redraw with real numbers on its own; nothing
        # here needs to retry or wait for that.
        line = "cg: stats loading…"
        update_seg = _update_available_segment()
        if update_seg:
            line += " | " + update_seg
        print(line)
        return
    if status == STATS_HUNG:
        # Unlike STATS_TIMEOUT above, there WAS a working proxy and every fetch since has kept
        # timing out for long enough (STALE_STATS_MAX_AGE_SECONDS) that "merely slow" has stopped
        # being the better read. Still not `cg!` — this is reachable at the TCP level, which a
        # refused connection is not — but "loading…" would be dishonest here: this is not a
        # render that is about to resolve itself on the next tick the way a genuinely slow proxy
        # is, so it gets a distinct marker rather than reusing either wording.
        line = "cg? not responding"
        update_seg = _update_available_segment()
        if update_seg:
            line += " | " + update_seg
        print(line)
        return
    stale = status == STATS_TIMEOUT  # stats is real, but from a past fetch, not this one

    # The context bar is payload-only (no fetch) so it renders even when stats do not; the rest
    # need `stats`. Priority is savings-vs-session-total first and always on; the cache TTL
    # stopper and the keep-alive ping counter stay OPT-IN extras, off unless their flag is passed
    # — see skills/statusline/SKILL.md for the one-line command that turns either on. The pending-
    # update marker is placed FIRST, ahead of even the context bar, on purpose: it is the one
    # segment meant as a fallback for something the user may not otherwise be told at all, and it
    # must not scroll off the end of a long line or land after a truncation.
    parts = []
    update_seg = _update_available_segment()
    if update_seg:
        parts.append(update_seg)
    context_seg = _context_segment(payload)
    if context_seg:
        parts.append(context_seg)
    default_seg = _default_segment(payload, stats)
    if default_seg:
        parts.append(default_seg)
    if _cache_segment_enabled():
        parts.append(_cache_stopper(payload))
    if stats is not None and _keepalive_segment_enabled():
        keepalive = _keepalive_segment(stats)
        if keepalive:
            parts.append(keepalive)
    latency_seg = _latency_segment(stats)
    if latency_seg:
        parts.append(latency_seg)
    recommend_seg = _recommend_segment(payload)
    if recommend_seg:
        parts.append(recommend_seg)
    line = " | ".join(parts)
    if stale and line:
        # A small, honest marker that every number to its left is from the last successful
        # fetch rather than this render — never withheld, never dressed up as fresh.
        line += f" {DIM}⏳{RESET}"
    print(line)


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
