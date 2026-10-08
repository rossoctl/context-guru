#!/usr/bin/env python3
"""wait_causes.py — why a thread waits, and what a keep-alive ping budget would cost.

Reusable analysis for https://github.com/rossoctl/context-guru/issues/424 (and #423).

Two modes:

  1. Transcript mode (--projects-dir given). Reads Claude Code session transcripts
     (one JSONL file per thread: the main session file, plus one file per subagent
     or in-process agent-team teammate under <session>/subagents/*.jsonl — the two
     are told apart by meta.json's taskKind and reported as separate thread kinds,
     "subagent" and "team_thread"). Each thread gives an exact sequence of API
     calls, exact gaps between them, and the exact signal that ended each gap
     (teammate message, background task notification, human text, or the agent's
     own tool result). Optionally also reads a dashboard DB (--db) to cross-check
     cache-miss labels, pull in the REAL keep-alive pings already sent (for the
     OBSERVED cost column), and report upstream failures.

  2. DB-only mode (--db given, no --projects-dir). This is how it must run on the
     production proxy, which has no transcripts. There are no threads and no wake
     causes here — the script says so and groups by (session, model) as an
     approximate thread proxy, flagged as such.

Python 3 standard library only. Run with `python3 -I`.
See README.md in this directory for usage.
"""
from __future__ import annotations

import argparse
import glob
import json
import os
import re
import sqlite3
import sys
from collections import defaultdict
from dataclasses import dataclass, field
from statistics import median
from typing import Iterable, Iterator

# ---------------------------------------------------------------------------
# Constants from issue #424's technical notes.
# ---------------------------------------------------------------------------

PING_INTERVAL_MIN = 4.67  # minutes between keep-alive pings
PING_COST_X = 0.1  # one ping costs 0.1x (x = the prompt's base input price)
MISS_PREMIUM_X = 1.15  # a cache miss costs 1.15x more than a hit
GAP_THRESHOLD_MIN = 4.67  # a gap below this is not "waiting", just normal turn time
PING_BUDGETS = list(range(0, 13))  # N = 0..12, as the issue asks for

WAKE_CAUSES = ("tool_result", "background", "teammate", "human", "retry", "unknown")

# Hardcoded base INPUT price per token, in USD, by model family. This is the "x"
# unit the issue's cost tables use. Cache write (5m) is priced at 1.25x this,
# cache write (1h) at 2x, cache read at 0.1x — which is exactly where PING_COST_X
# and MISS_PREMIUM_X below come from (1.25 - 0.1 = 1.15).
#
# These are list prices per Anthropic's published pricing as of Oct 2026, not
# pulled from a live source. Where the local dashboard DB is available we could
# derive a more exact per-model rate from its cost_usd/cache_write columns, but
# for a first pass a small hardcoded table is enough to get a USD figure in the
# right ballpark. Say so in the report.
MODEL_BASE_INPUT_PRICE_PER_TOKEN = {
    "claude-opus-5-5": 15e-6,
    "claude-opus-4-5": 15e-6,
    "claude-opus-4-1": 15e-6,
    "claude-sonnet-5-5": 3e-6,
    "claude-sonnet-4-5": 3e-6,
    "claude-haiku-5-5": 1e-6,
    "claude-haiku-4-5": 1e-6,
}
DEFAULT_BASE_INPUT_PRICE = 3e-6  # fall back to Sonnet-ish pricing for an unknown model


def base_price_for_model(model: str) -> float:
    if not model:
        return DEFAULT_BASE_INPUT_PRICE
    # Bedrock/vertex model ids carry region/provider prefixes; match on substring.
    for key, price in MODEL_BASE_INPUT_PRICE_PER_TOKEN.items():
        if key in model:
            return price
    return DEFAULT_BASE_INPUT_PRICE


def ping_window_minutes(n_pings: int) -> float:
    """How long N pings keep the cache warm for, in minutes."""
    return PING_INTERVAL_MIN * n_pings + 5.0


def sim_cost_x(gap_min: float, n_pings: int) -> float:
    """Cost of one gap, in units of x, under a budget of N pings.

    Per issue #424's technical notes: a gap g costs 0.1*floor(g/4.67) when it
    ends inside the N-ping warm window, and 0.1*N + 1.15 otherwise (N pings
    spent, then still a miss).
    """
    window = ping_window_minutes(n_pings)
    if gap_min <= window:
        return PING_COST_X * (gap_min // PING_INTERVAL_MIN)
    return PING_COST_X * n_pings + MISS_PREMIUM_X


# ---------------------------------------------------------------------------
# Transcript parsing
# ---------------------------------------------------------------------------

TEAMMATE_RE = re.compile(r"<teammate-message")
TASK_NOTIF_RE = re.compile(r"<task-notification>")


@dataclass
class Call:
    """One API call on a thread, as recorded in the transcript."""

    ts: float  # epoch seconds (response-arrival time; see README caveat)
    message_id: str
    model: str
    is_error: bool
    input_tokens: int = 0
    cache_read: int = 0
    cache_creation: int = 0
    output_tokens: int = 0

    @property
    def is_cache_hit(self) -> bool:
        """A hit: most of the prompt was read from cache, not rewritten.

        cache_read == 0 is the obvious miss (nothing of the prefix matched),
        but a prompt can also come back with SOME cache_read and a cache_creation
        that dwarfs it — most of the prefix still had to be rewritten, which is
        a miss in every way that matters for cost, even though cache_read isn't
        literally zero. The threshold (cache_creation < half of cache_read) is
        the team's working definition; see issue #424 discussion.
        """
        return (not self.is_error) and self.cache_read > 0 and self.cache_creation < 0.5 * self.cache_read

    @property
    def is_cache_miss(self) -> bool:
        return (not self.is_error) and not self.is_cache_hit

    @property
    def prompt_tokens_total(self) -> int:
        return self.input_tokens + self.cache_read + self.cache_creation


@dataclass
class Gap:
    thread_id: str
    thread_kind: str  # "main", "subagent", or "team_thread" (in-process agent-team teammate)
    project: str
    start: Call
    end: Call
    gap_min: float
    cause: str  # one of WAKE_CAUSES
    is_cold_start: bool = False  # end.start is the thread's first call


def iso_to_epoch(ts: str) -> float | None:
    if not ts:
        return None
    try:
        # Claude Code timestamps are ISO-8601 UTC with a trailing 'Z'.
        from datetime import datetime, timezone

        t = ts.replace("Z", "+00:00")
        return datetime.fromisoformat(t).astimezone(timezone.utc).timestamp()
    except Exception:
        return None


def entry_user_text(entry: dict) -> tuple[str, bool]:
    """Return (joined text, saw_only_tool_results) for a 'user' transcript entry."""
    message = entry.get("message") or {}
    content = message.get("content")
    if isinstance(content, str):
        return content, False
    if not isinstance(content, list):
        return "", False
    texts = []
    saw_tool_result = False
    saw_text = False
    for block in content:
        if not isinstance(block, dict):
            continue
        btype = block.get("type")
        if btype == "text":
            texts.append(block.get("text", ""))
            saw_text = True
        elif btype == "tool_result":
            saw_tool_result = True
    return "\n".join(texts), (saw_tool_result and not saw_text)


def classify_wake_cause(between_entries: list[dict]) -> str:
    """Look at the transcript entries between two calls and say what woke the thread.

    Order of signal, per issue #424: a teammate message beats a task notification
    beats plain human text beats a bare tool result (the agent's own next tool
    call, no new external input). We scan in order and take the LAST 'user' type
    entry's signal, since that is the one that immediately preceded the next call.
    """
    last_cause = None
    for entry in between_entries:
        if entry.get("type") != "user":
            continue
        text, only_tool_result = entry_user_text(entry)
        if TEAMMATE_RE.search(text):
            last_cause = "teammate"
        elif TASK_NOTIF_RE.search(text):
            last_cause = "background"
        elif text.strip():
            last_cause = "human"
        elif only_tool_result:
            last_cause = "tool_result"
    return last_cause or "unknown"


def iter_calls_and_gaps(
    path: str, thread_id: str, thread_kind: str, project: str
) -> tuple[list[Call], list[Gap]]:
    """Parse one thread's JSONL file into its calls and its gaps > threshold."""
    calls: list[Call] = []
    seen_message_ids: set[str] = set()
    pending_between: list[dict] = []
    gaps: list[Gap] = []
    prev_call: Call | None = None

    try:
        fh = open(path, "r", encoding="utf-8", errors="replace")
    except OSError:
        return [], []

    with fh:
        for line in fh:
            line = line.strip()
            if not line:
                continue
            try:
                entry = json.loads(line)
            except json.JSONDecodeError:
                continue

            etype = entry.get("type")
            if etype == "assistant":
                message = entry.get("message") or {}
                usage = message.get("usage")
                is_error = bool(entry.get("isApiErrorMessage"))
                mid = message.get("id") or entry.get("uuid")
                if not is_error and usage is None:
                    # A content-block-only assistant entry (tool_use, thinking) with
                    # no usage field: not a call boundary by itself.
                    continue
                if mid in seen_message_ids:
                    # Split content blocks of the SAME API response repeat usage.
                    continue
                ts = iso_to_epoch(entry.get("timestamp", ""))
                if ts is None:
                    continue
                seen_message_ids.add(mid)
                call = Call(
                    ts=ts,
                    message_id=mid or "",
                    model=message.get("model", "") or "",
                    is_error=is_error,
                    input_tokens=int((usage or {}).get("input_tokens", 0) or 0),
                    cache_read=int((usage or {}).get("cache_read_input_tokens", 0) or 0),
                    cache_creation=int((usage or {}).get("cache_creation_input_tokens", 0) or 0),
                    output_tokens=int((usage or {}).get("output_tokens", 0) or 0),
                )
                calls.append(call)

                if prev_call is not None:
                    gap_min = (call.ts - prev_call.ts) / 60.0
                    if gap_min >= GAP_THRESHOLD_MIN:
                        if prev_call.is_error:
                            cause = "retry"
                        else:
                            cause = classify_wake_cause(pending_between)
                        gaps.append(
                            Gap(
                                thread_id=thread_id,
                                thread_kind=thread_kind,
                                project=project,
                                start=prev_call,
                                end=call,
                                gap_min=gap_min,
                                cause=cause,
                            )
                        )
                else:
                    # First call of the thread: cold start, not a gap, but note it
                    # on the Gap list with a zero-length marker for aggregation.
                    gaps.append(
                        Gap(
                            thread_id=thread_id,
                            thread_kind=thread_kind,
                            project=project,
                            start=call,
                            end=call,
                            gap_min=0.0,
                            cause="cold_start",
                            is_cold_start=True,
                        )
                    )

                pending_between = []
                prev_call = call
            elif etype == "user":
                pending_between.append(entry)
            # attachment / other entry types carry no wake-cause signal; ignored.

    return calls, gaps


WORKTREE_SUFFIX_RE = re.compile(r"--claude-worktrees-.*$")


def project_group_name(dir_basename: str) -> str:
    """Fold a Claude Code worktree project dir into its parent project's name.

    Claude Code flattens a worktree's path into the projects dir name, e.g.
    "-Users-davidamid-git-context-guru--claude-worktrees-cache-strategy-wording"
    for a session started in that worktree. These are the same project as
    "-Users-davidamid-git-context-guru"; group them so the report is per real
    project, not per worktree.
    """
    return WORKTREE_SUFFIX_RE.sub("", dir_basename)


def subagent_thread_kind(subdir: str, sub_name: str) -> str:
    """"subagent" (a plain Task-tool subagent) or "team_thread".

    "team_thread" is an in-process agent-team teammate (a committer/reviewer/etc.
    spawned by the AgentTeam skill, meta.json's taskKind == "in_process_teammate").
    These are NOT the same thing as the "teammate" WAKE CAUSE (a thread woken by a
    <teammate-message>, which either kind of thread can experience) — a team_thread
    is itself a long-lived peer session, not a fire-and-forget task, and it is why
    team_thread gaps run much longer (its wait is on peer coordination, not on a
    bounded subagent task finishing).
    """
    meta_path = os.path.join(subdir, sub_name + ".meta.json")
    try:
        with open(meta_path, "r", encoding="utf-8") as fh:
            meta = json.load(fh)
        if meta.get("taskKind") == "in_process_teammate":
            return "team_thread"
    except Exception:
        pass
    return "subagent"


def find_threads(projects_dirs: Iterable[str]) -> list[tuple[str, str, str, str]]:
    """Return (project, thread_id, thread_kind, path) for every thread file found."""
    threads = []
    for pdir in projects_dirs:
        pdir = os.path.expanduser(pdir)
        if not os.path.isdir(pdir):
            continue
        project = project_group_name(os.path.basename(pdir.rstrip("/")))
        for path in sorted(glob.glob(os.path.join(pdir, "*.jsonl"))):
            session_id = os.path.splitext(os.path.basename(path))[0]
            threads.append((project, session_id, "main", path))
            subdir = os.path.join(pdir, session_id, "subagents")
            if os.path.isdir(subdir):
                for sub_path in sorted(glob.glob(os.path.join(subdir, "*.jsonl"))):
                    sub_name = os.path.splitext(os.path.basename(sub_path))[0]
                    thread_id = f"{session_id}/{sub_name}"
                    kind = subagent_thread_kind(subdir, sub_name)
                    threads.append((project, thread_id, kind, sub_path))
    return threads


# ---------------------------------------------------------------------------
# Dashboard DB
# ---------------------------------------------------------------------------


@dataclass
class DBRow:
    id: int
    ts_ms: int
    session_id: str
    model: str
    status: int
    cache_read: int
    cache_write: int
    fresh_input: int
    output_tokens: int
    cost_usd: float
    cache_miss_reason: str
    upstream_ms: float
    keepalive: int
    keepalive_pings: int
    saved_unique: int
    stop_reason: str


def read_db_rows(db_path: str) -> list[DBRow]:
    uri = f"file:{db_path}?mode=ro"
    con = sqlite3.connect(uri, uri=True)
    con.row_factory = sqlite3.Row
    cur = con.cursor()
    cur.execute(
        """
        select id, ts, session_id, model, status, cache_read, cache_write,
               fresh_input, output_tokens, cost_usd, cache_miss_reason,
               upstream_ms, keepalive, keepalive_pings, saved_unique, stop_reason
        from requests
        order by session_id, ts
        """
    )
    rows = [
        DBRow(
            id=r["id"],
            ts_ms=r["ts"],
            session_id=r["session_id"],
            model=r["model"],
            status=r["status"],
            cache_read=r["cache_read"],
            cache_write=r["cache_write"],
            fresh_input=r["fresh_input"],
            output_tokens=r["output_tokens"],
            cost_usd=r["cost_usd"],
            cache_miss_reason=r["cache_miss_reason"],
            upstream_ms=r["upstream_ms"],
            keepalive=r["keepalive"],
            keepalive_pings=r["keepalive_pings"],
            saved_unique=r["saved_unique"],
            stop_reason=r["stop_reason"],
        )
        for r in cur.fetchall()
    ]
    con.close()
    return rows


MATCH_WINDOW_SEC = 60.0  # DB ts is request-START; transcript ts is response-ARRIVAL.


class DBIndex:
    """Matches a transcript Gap's ending call to the DB row it produced.

    The DB has no thread id (issue #423), so matching is by session_id +
    exact cache_read token count + nearest timestamp within MATCH_WINDOW_SEC.
    Used to pull the REAL keepalive_pings already spent on a gap, for the
    OBSERVED cost column, and by mislabel_check. Best-effort, not exact —
    see README.
    """

    def __init__(self, db_rows: list[DBRow]):
        self._by_key: dict[tuple[str, int], list[tuple[float, DBRow]]] = defaultdict(list)
        for r in db_rows:
            self._by_key[(r.session_id, r.cache_read)].append((r.ts_ms / 1000.0, r))
        for v in self._by_key.values():
            v.sort(key=lambda pair: pair[0])

    def match(self, session_id: str, cache_read: int, ts: float) -> DBRow | None:
        candidates = self._by_key.get((session_id, cache_read))
        if not candidates:
            return None
        best, best_dt = None, MATCH_WINDOW_SEC
        for cts, row in candidates:
            dt = abs(cts - ts)
            if dt <= best_dt:
                best_dt, best = dt, row
        return best

    def pings_for(self, gap: "Gap") -> int:
        session_id = gap.thread_id.split("/", 1)[0]
        row = self.match(session_id, gap.end.cache_read, gap.end.ts)
        return row.keepalive_pings if row else 0


# ---------------------------------------------------------------------------
# Aggregation
# ---------------------------------------------------------------------------


def pct(values: list[float], p: float) -> float:
    if not values:
        return 0.0
    s = sorted(values)
    k = (len(s) - 1) * p
    f, c = int(k), min(int(k) + 1, len(s) - 1)
    if f == c:
        return s[f]
    return s[f] + (s[c] - s[f]) * (k - f)


def gap_dist(gaps_min: list[float]) -> dict:
    if not gaps_min:
        return {"count": 0, "median_min": 0.0, "p75_min": 0.0, "p90_min": 0.0, "max_min": 0.0}
    return {
        "count": len(gaps_min),
        "median_min": round(median(gaps_min), 2),
        "p75_min": round(pct(gaps_min, 0.75), 2),
        "p90_min": round(pct(gaps_min, 0.90), 2),
        "max_min": round(max(gaps_min), 2),
    }


def miss_premium_usd(call: Call) -> float:
    """What this call's cache_creation cost over what a hit would have, in USD."""
    price = base_price_for_model(call.model)
    return call.cache_creation * price * MISS_PREMIUM_X


def observed_cost_x_usd(gap: Gap, pings: int) -> tuple[float, float]:
    """What this gap ACTUALLY cost: `pings` real keep-alive pings (0 if unknown —
    the DB does not cover every session) plus a miss premium if it was still a
    miss. This is the "OBSERVED" column: real traffic, real (partial) mitigation,
    using the corrected hit/miss rule (see Call.is_cache_hit). It is deliberately
    NOT the same thing as the N=0 simulation, which assumes zero real pinging
    ever happened; where the DB shows pings already in flight, observed can come
    in below the N=0 simulated cost for the same gap.
    """
    is_miss = gap.end.is_cache_miss
    x = PING_COST_X * pings + (MISS_PREMIUM_X if is_miss else 0.0)
    price = base_price_for_model(gap.end.model)
    ping_usd = pings * PING_COST_X * price * gap.end.prompt_tokens_total
    usd = ping_usd + (miss_premium_usd(gap.end) if is_miss else 0.0)
    return x, usd


def summarize_gaps_by_cause(gaps: list[Gap], db_index: "DBIndex | None" = None) -> dict:
    by_cause: dict[str, list[Gap]] = defaultdict(list)
    for g in gaps:
        if g.is_cold_start:
            continue
        by_cause[g.cause].append(g)

    out = {}
    for cause, gs in sorted(by_cause.items()):
        gap_minutes = [g.gap_min for g in gs]
        dist = gap_dist(gap_minutes)

        hits = sum(1 for g in gs if g.end.is_cache_hit)
        misses = len(gs) - hits

        # OBSERVED: what really happened on this traffic, including real pings
        # already sent (from the DB, where it covers the session) before this
        # call landed.
        x_costs, usd_costs, pings_used = [], [], []
        for g in gs:
            pings = db_index.pings_for(g) if db_index else 0
            x, usd = observed_cost_x_usd(g, pings)
            x_costs.append(x)
            usd_costs.append(usd)
            pings_used.append(pings)
        avg_observed_x = sum(x_costs) / len(gs) if gs else 0.0
        total_observed_usd = sum(usd_costs)

        # SIMULATED: a hypothetical ping budget of N, applied uniformly, assuming
        # NO pinging happens outside the budget. N=0 is therefore 1.15x for every
        # gap over 5 minutes, by construction (sim_cost_x), regardless of what
        # really happened on that gap — it is the "do nothing" baseline the N>0
        # columns are compared against, not a restatement of OBSERVED.
        sim = {}
        for n in PING_BUDGETS:
            costs = [sim_cost_x(g.gap_min, n) for g in gs]
            sim[n] = round(sum(costs) / len(costs), 3) if costs else 0.0
        best_n = min(sim, key=lambda n: sim[n])

        out[cause] = {
            "gap_distribution": dist,
            "hits": hits,
            "misses": misses,
            "observed_cost_x_per_gap": round(avg_observed_x, 3),
            "observed_cost_usd_total": round(total_observed_usd, 4),
            "observed_pings_total": sum(pings_used),
            "sim_cost_x_per_gap_by_n": sim,
            "best_n": best_n,
            "best_n_cost_x_per_gap": sim[best_n],
        }
    return out


def summarize_cold_starts(gaps: list[Gap]) -> dict:
    cold = [g for g in gaps if g.is_cold_start]
    misses = sum(1 for g in cold if g.end.is_cache_miss)
    usd = sum(miss_premium_usd(g.end) for g in cold if g.end.is_cache_miss)
    return {"threads": len(cold), "cache_misses": misses, "miss_cost_usd_total": round(usd, 4)}


# ---------------------------------------------------------------------------
# Mislabel check and failure stats (needs the DB)
# ---------------------------------------------------------------------------


MISLABEL_MATCH_WINDOW_SEC = 60.0


def mislabel_check(all_gaps: list[Gap], db_rows: list[DBRow]) -> dict:
    """Count DB prefix_change rows that are really per-thread TTL expiry / cold start.

    The DB has no thread id (that is #423's job), so a row is matched to a
    transcript gap/cold-start event by: same session_id, same cache_read token
    count (both sides of a genuine miss agree on this), and the nearest
    timestamp within MISLABEL_MATCH_WINDOW_SEC. The DB's ts is the request's
    START; the transcript only gives the response's ARRIVAL time, so some
    skew is expected — on the validation session (fix-summarizer) matched
    deltas cluster under 65s and then jump to 74s+, which is where the window
    is cut. This is a best-effort match, not an exact one; see README.
    """
    session_events: dict[str, list[tuple[float, str, int]]] = defaultdict(list)
    for g in all_gaps:
        session_id = g.thread_id.split("/", 1)[0]
        if g.is_cold_start:
            label = "cold_start"
        elif g.gap_min >= 5.0:
            label = "ttl_expiry"
        else:
            continue
        session_events[session_id].append((g.end.ts, label, g.end.cache_read))

    mislabeled_count = 0
    mislabeled_usd = 0.0
    checked = 0
    for row in db_rows:
        if row.cache_miss_reason != "prefix_change":
            continue
        events = session_events.get(row.session_id)
        if not events:
            continue
        checked += 1
        row_ts = row.ts_ms / 1000.0
        best_label, best_dt = None, MISLABEL_MATCH_WINDOW_SEC
        for ts, label, cache_read in events:
            if cache_read != row.cache_read:
                continue
            dt = abs(ts - row_ts)
            if dt <= best_dt:
                best_dt, best_label = dt, label
        if best_label in ("ttl_expiry", "cold_start"):
            mislabeled_count += 1
            mislabeled_usd += row.cost_usd
    return {
        "db_prefix_change_rows_checked": checked,
        "mislabeled_count": mislabeled_count,
        "mislabeled_usd": round(mislabeled_usd, 4),
    }


def failure_stats(db_rows: list[DBRow]) -> dict:
    """Non-200 upstream rows, their hang time, and misses that follow them.

    A single real-world "hang" often shows up as several consecutive 5xx rows
    (the client/proxy retries fast before Claude Code's own retry succeeds), so
    misses-following-a-failure is counted once per consecutive run of failures,
    against the row that follows the run — not once per failed row. Status 0
    ("no upstream") is excluded; it is not an upstream failure.
    """
    failures = [r for r in db_rows if r.status != 0 and not (200 <= r.status < 300)]
    hang_ms = [r.upstream_ms for r in failures if r.upstream_ms]
    by_status: dict[int, list[float]] = defaultdict(list)
    for r in failures:
        if r.upstream_ms:
            by_status[r.status].append(r.upstream_ms)

    by_session: dict[str, list[DBRow]] = defaultdict(list)
    for r in db_rows:
        by_session[r.session_id].append(r)

    misses_following_failure = 0
    for session_id, rows in by_session.items():
        rows_sorted = sorted(rows, key=lambda r: r.ts_ms)
        i, n = 0, len(rows_sorted)
        while i < n:
            r = rows_sorted[i]
            if r.status != 0 and not (200 <= r.status < 300):
                j = i
                while j < n and rows_sorted[j].status != 0 and not (200 <= rows_sorted[j].status < 300):
                    j += 1
                if j < n and rows_sorted[j].cache_miss_reason not in ("", "hit"):
                    misses_following_failure += 1
                i = j
            else:
                i += 1

    return {
        "failure_count": len(failures),
        "hang_ms_median": round(median(hang_ms), 1) if hang_ms else 0.0,
        "hang_ms_max": round(max(hang_ms), 1) if hang_ms else 0.0,
        "hang_ms_median_by_status": {k: round(median(v), 1) for k, v in sorted(by_status.items())},
        "cache_misses_following_a_failure_run": misses_following_failure,
    }


# ---------------------------------------------------------------------------
# DB-only (production) mode
# ---------------------------------------------------------------------------


def db_only_report(db_rows: list[DBRow]) -> dict:
    """Approximate, thread-less report: group by (session, model) as a thread proxy."""
    by_key: dict[tuple[str, str], list[DBRow]] = defaultdict(list)
    for r in db_rows:
        by_key[(r.session_id, r.model)].append(r)

    gaps_min: list[float] = []
    miss_x_total = 0.0
    miss_usd_total = 0.0
    sim_totals = {n: [] for n in PING_BUDGETS}

    for key, rows in by_key.items():
        rows_sorted = sorted(rows, key=lambda r: r.ts_ms)
        for i in range(1, len(rows_sorted)):
            prev, cur = rows_sorted[i - 1], rows_sorted[i]
            gap_min = (cur.ts_ms - prev.ts_ms) / 1000.0 / 60.0
            if gap_min < GAP_THRESHOLD_MIN:
                continue
            gaps_min.append(gap_min)
            price = base_price_for_model(cur.model)
            is_miss = cur.cache_miss_reason not in ("", "hit")
            if is_miss:
                miss_x_total += MISS_PREMIUM_X
                miss_usd_total += cur.cache_write * price * MISS_PREMIUM_X
            for n in PING_BUDGETS:
                sim_totals[n].append(sim_cost_x(gap_min, n))

    sim = {n: round(sum(v) / len(v), 3) if v else 0.0 for n, v in sim_totals.items()}
    best_n = min(sim, key=lambda n: sim[n]) if sim else 0

    return {
        "mode": "db_only_approximate",
        "note": (
            "No transcripts: grouped by (session_id, model) as a best-effort thread "
            "proxy. Wake cause is unknown everywhere until issue #424's wake_cause "
            "field ships; the gap counts and ping simulation below do not depend on it."
        ),
        "gap_distribution": gap_dist(gaps_min),
        "miss_cost_no_pings_x_per_gap": round(miss_x_total / len(gaps_min), 3) if gaps_min else 0.0,
        "miss_cost_no_pings_usd_total": round(miss_usd_total, 4),
        "ping_sim_x_per_gap_by_n": sim,
        "best_n": best_n,
        "failures": failure_stats(db_rows),
    }


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------


def run_transcript_mode(projects_dirs: list[str], db_paths: list[str]) -> dict:
    threads = find_threads(projects_dirs)
    report: dict = {"mode": "transcript", "projects": {}}

    all_gaps_global: list[Gap] = []
    db_rows: list[DBRow] = []
    for db_path in db_paths:
        try:
            db_rows.extend(read_db_rows(db_path))
        except Exception as exc:  # fail open: a bad DB must not kill the report
            report.setdefault("db_errors", []).append(f"{db_path}: {exc}")

    per_project_gaps: dict[str, list[Gap]] = defaultdict(list)
    per_project_calls: dict[str, dict[str, list[Call]]] = defaultdict(lambda: defaultdict(list))

    for project, thread_id, thread_kind, path in threads:
        calls, gaps = iter_calls_and_gaps(path, thread_id, thread_kind, project)
        per_project_gaps[project].extend(gaps)
        per_project_calls[project][thread_id] = calls
        all_gaps_global.extend(gaps)

    db_index = DBIndex(db_rows) if db_rows else None

    for project, gaps in sorted(per_project_gaps.items()):
        report["projects"][project] = {"threads": len(per_project_calls[project])}
        for kind in ("main", "subagent", "team_thread"):
            kind_gaps = [g for g in gaps if g.thread_kind == kind]
            report["projects"][project][kind] = {
                "cold_starts": summarize_cold_starts(kind_gaps),
                "by_cause": summarize_gaps_by_cause(kind_gaps, db_index),
            }

    if db_rows:
        report["mislabel_check"] = mislabel_check(all_gaps_global, db_rows)
        report["failures"] = failure_stats(db_rows)
    else:
        report["mislabel_check"] = None
        report["failures"] = None

    return report


def human_summary(report: dict) -> str:
    lines = []
    if report["mode"] == "db_only_approximate":
        lines.append("DB-only mode (no transcripts) — approximate, no wake causes.")
        lines.append(report["note"])
        d = report["gap_distribution"]
        lines.append(
            f"Gaps >= {GAP_THRESHOLD_MIN} min: {d['count']}, median {d['median_min']} min, "
            f"p90 {d['p90_min']} min, max {d['max_min']} min"
        )
        lines.append(
            f"Observed miss cost (no transcripts, so pings unknown -> treated as 0): "
            f"{report['miss_cost_no_pings_x_per_gap']}x/gap (${report['miss_cost_no_pings_usd_total']} total)"
        )
        lines.append(f"Simulated best N (0-12): {report['best_n']}, cost {report['ping_sim_x_per_gap_by_n'][report['best_n']]}x/gap")
        for n in (0, 2, 4, 6, 10):
            lines.append(f"  simulated N={n}: {report['ping_sim_x_per_gap_by_n'].get(n)}x/gap")
        f = report["failures"]
        lines.append(
            f"Failures: {f['failure_count']}, hang median {f['hang_ms_median']}ms, "
            f"misses following a failure: {f['cache_misses_following_a_failure_run']}"
        )
        return "\n".join(lines)

    for project, data in report["projects"].items():
        lines.append(f"\n== {project} ({data['threads']} threads) ==")
        for kind in ("main", "subagent", "team_thread"):
            kd = data.get(kind)
            if not kd:
                continue
            cs = kd["cold_starts"]
            if cs["threads"] == 0 and not kd["by_cause"]:
                continue
            label = "team_thread (in-process agent-team teammate)" if kind == "team_thread" else kind
            lines.append(
                f"  {label}: cold starts {cs['threads']} ({cs['cache_misses']} misses, "
                f"${cs['miss_cost_usd_total']})"
            )
            for cause, stats in kd["by_cause"].items():
                dist = stats["gap_distribution"]
                lines.append(
                    f"    {cause}: {dist['count']} gaps (hits={stats['hits']} misses={stats['misses']}), "
                    f"median {dist['median_min']}min, p90 {dist['p90_min']}min"
                )
                lines.append(
                    f"      observed: {stats['observed_cost_x_per_gap']}x/gap "
                    f"(${stats['observed_cost_usd_total']} total, {stats['observed_pings_total']} real pings seen)"
                )
                lines.append(
                    f"      simulated: N=0 -> {stats['sim_cost_x_per_gap_by_n'][0]}x/gap, "
                    f"best N={stats['best_n']} -> {stats['best_n_cost_x_per_gap']}x/gap"
                )
    if report.get("mislabel_check"):
        mc = report["mislabel_check"]
        lines.append(
            f"\nMislabel check: {mc['mislabeled_count']} / {mc['db_prefix_change_rows_checked']} "
            f"DB prefix_change rows are really TTL expiry/cold start (${mc['mislabeled_usd']})"
        )
    if report.get("failures"):
        f = report["failures"]
        lines.append(
            f"Failures: {f['failure_count']}, hang median {f['hang_ms_median']}ms, "
            f"misses following a failure: {f['cache_misses_following_a_failure_run']}"
        )
    if report.get("db_errors"):
        lines.append(f"\nDB errors (ignored, fail-open): {report['db_errors']}")
    return "\n".join(lines)


# ---------------------------------------------------------------------------
# Self-test: a tiny synthetic transcript with known gaps, known hits/misses.
# ---------------------------------------------------------------------------


def _write_call_line(fh, ts_iso: str, message_id: str, model: str, cache_read: int, cache_creation: int, is_error: bool = False) -> None:
    entry = {
        "type": "assistant",
        "timestamp": ts_iso,
        "message": {
            "id": message_id,
            "model": model,
            "usage": {
                "input_tokens": 10,
                "cache_read_input_tokens": cache_read,
                "cache_creation_input_tokens": cache_creation,
                "output_tokens": 50,
            },
        },
    }
    if is_error:
        entry["isApiErrorMessage"] = True
    fh.write(json.dumps(entry) + "\n")


def _write_user_text_line(fh, ts_iso: str, text: str) -> None:
    entry = {"type": "user", "timestamp": ts_iso, "message": {"content": [{"type": "text", "text": text}]}}
    fh.write(json.dumps(entry) + "\n")


def _write_user_tool_result_line(fh, ts_iso: str) -> None:
    entry = {"type": "user", "timestamp": ts_iso, "message": {"content": [{"type": "tool_result", "content": "ok"}]}}
    fh.write(json.dumps(entry) + "\n")


def run_self_test() -> bool:
    """Build a tiny synthetic transcript (one main thread, one plain subagent
    thread) with known gaps, known hits/misses, and known causes; assert the
    exact observed and simulated numbers. Exits non-zero on failure.
    """
    import math
    import tempfile
    from datetime import datetime, timedelta, timezone

    ok = True

    def check(label: str, got, want, tol: float = 1e-6) -> None:
        nonlocal ok
        same = math.isclose(got, want, abs_tol=tol) if isinstance(want, float) else got == want
        status = "OK" if same else "FAIL"
        if not same:
            ok = False
        print(f"  [{status}] {label}: got {got!r}, want {want!r}")

    t0 = datetime(2026, 1, 1, tzinfo=timezone.utc)

    def iso(offset_sec: float) -> str:
        return (t0 + timedelta(seconds=offset_sec)).isoformat().replace("+00:00", "Z")

    with tempfile.TemporaryDirectory() as tmp:
        session_id = "self-test-session"
        main_path = os.path.join(tmp, f"{session_id}.jsonl")
        with open(main_path, "w") as fh:
            # Call 1: cold start. cache_read=0, cache_creation=2000 -> a miss.
            _write_call_line(fh, iso(0), "m1", "claude-opus-5-5", cache_read=0, cache_creation=2000)
            _write_user_text_line(fh, iso(1), "do the next thing")  # human
            # Gap 1: 10 min, cause=human, cache_read=0 cache_creation=5000 -> miss.
            _write_call_line(fh, iso(600), "m2", "claude-opus-5-5", cache_read=0, cache_creation=5000)
            _write_user_tool_result_line(fh, iso(601))  # tool_result
            # Gap 2: 400s (6.667 min), cause=tool_result, cache_read=50000
            # cache_creation=1000 (< half of cache_read) -> hit.
            _write_call_line(fh, iso(1000), "m3", "claude-opus-5-5", cache_read=50000, cache_creation=1000)

        subdir = os.path.join(tmp, session_id, "subagents")
        os.makedirs(subdir)
        sub_path = os.path.join(subdir, "agent-x.jsonl")
        with open(sub_path, "w") as fh:
            # Cold start: cache_read=0, cache_creation=1000 -> miss.
            _write_call_line(fh, iso(0), "s1", "claude-opus-5-5", cache_read=0, cache_creation=1000)
            fh.write(json.dumps({"type": "user", "timestamp": iso(1), "message": {"content": "<task-notification>\n<task-id>x</task-id>\n</task-notification>"}}) + "\n")
            # Gap: 300s (5.0 min), cause=background, cache_read=800 cache_creation=100 -> hit.
            _write_call_line(fh, iso(300), "s2", "claude-opus-5-5", cache_read=800, cache_creation=100)

        threads = find_threads([tmp])
        kinds = {t[2] for t in threads}
        check("thread kinds found", kinds, {"main", "subagent"})

        all_gaps = []
        for project, thread_id, thread_kind, path in threads:
            _, gaps = iter_calls_and_gaps(path, thread_id, thread_kind, project)
            all_gaps.extend(gaps)

        main_gaps = [g for g in all_gaps if g.thread_kind == "main"]
        sub_gaps = [g for g in all_gaps if g.thread_kind == "subagent"]

        main_cold = summarize_cold_starts(main_gaps)
        check("main cold starts", main_cold["threads"], 1)
        check("main cold start misses", main_cold["cache_misses"], 1)
        check("main cold start USD", main_cold["miss_cost_usd_total"], round(2000 * 15e-6 * 1.15, 4))

        main_by_cause = summarize_gaps_by_cause(main_gaps, db_index=None)
        check("main causes", set(main_by_cause), {"human", "tool_result"})

        human = main_by_cause["human"]
        check("human gap count", human["gap_distribution"]["count"], 1)
        check("human gap minutes", human["gap_distribution"]["median_min"], round(600 / 60, 2))
        check("human hits", human["hits"], 0)
        check("human misses", human["misses"], 1)
        check("human observed x/gap (no DB -> 0 pings known)", human["observed_cost_x_per_gap"], 1.15)
        check("human observed USD", human["observed_cost_usd_total"], round(5000 * 15e-6 * 1.15, 4))
        check("human sim N=0", human["sim_cost_x_per_gap_by_n"][0], 1.15)
        # window(12) = 4.67*12+5 = 61.04 min >> 10 min, so it resolves inside the
        # window: cost = 0.1 * floor(10 / 4.67) = 0.1 * 2 = 0.2.
        check("human sim N=12", human["sim_cost_x_per_gap_by_n"][12], 0.2)

        tool_result = main_by_cause["tool_result"]
        check("tool_result gap count", tool_result["gap_distribution"]["count"], 1)
        check("tool_result hits", tool_result["hits"], 1)
        check("tool_result misses", tool_result["misses"], 0)
        check("tool_result observed x/gap (a hit, no pings)", tool_result["observed_cost_x_per_gap"], 0.0)
        check("tool_result observed USD", tool_result["observed_cost_usd_total"], 0.0)
        check("tool_result sim N=0 (forced miss baseline)", tool_result["sim_cost_x_per_gap_by_n"][0], 1.15)

        sub_cold = summarize_cold_starts(sub_gaps)
        check("subagent cold starts", sub_cold["threads"], 1)
        check("subagent cold start misses", sub_cold["cache_misses"], 1)

        sub_by_cause = summarize_gaps_by_cause(sub_gaps, db_index=None)
        check("subagent causes", set(sub_by_cause), {"background"})
        background = sub_by_cause["background"]
        check("background gap count", background["gap_distribution"]["count"], 1)
        check("background hits", background["hits"], 1)
        check("background misses", background["misses"], 0)
        check("background observed x/gap", background["observed_cost_x_per_gap"], 0.0)

    return ok


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--projects-dir", action="append", default=[], help="A ~/.claude/projects/<project> dir. Repeatable.")
    ap.add_argument("--db", action="append", default=[], help="A dashboard-*.db path. Repeatable.")
    ap.add_argument("--json", action="store_true", help="Print machine-readable JSON instead of a summary.")
    ap.add_argument("--self-test", action="store_true", help="Run the built-in self-test on a synthetic transcript and exit.")
    args = ap.parse_args()

    if args.self_test:
        ok = run_self_test()
        print("\nself-test " + ("PASSED" if ok else "FAILED"))
        return 0 if ok else 1

    if not args.projects_dir and not args.db:
        ap.error("pass at least one of --projects-dir or --db")

    if args.projects_dir:
        report = run_transcript_mode(args.projects_dir, args.db)
    else:
        db_rows: list[DBRow] = []
        for db_path in args.db:
            db_rows.extend(read_db_rows(db_path))
        report = db_only_report(db_rows)

    if args.json:
        print(json.dumps(report, indent=2, default=str))
    else:
        print(human_summary(report))
    return 0


if __name__ == "__main__":
    sys.exit(main())
