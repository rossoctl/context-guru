#!/usr/bin/env python3
"""wait_causes.py — why a thread waits, and what a keep-alive ping budget would cost.

Reusable analysis for https://github.com/rossoctl/context-guru/issues/424 (and #423).

Two modes:

  1. Transcript mode (--projects-dir given). Reads Claude Code session transcripts
     (one JSONL file per thread: the main session file, plus one file per subagent
     under <session>/subagents/*.jsonl). Each thread gives an exact sequence of API
     calls, exact gaps between them, and the exact signal that ended each gap
     (teammate message, background task notification, human text, or the agent's
     own tool result). Optionally also reads a dashboard DB (--db) to cross-check
     cache-miss labels and upstream failures.

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
    def is_cache_miss(self) -> bool:
        return (not self.is_error) and self.cache_read == 0 and self.cache_creation > 0

    @property
    def prompt_tokens_total(self) -> int:
        return self.input_tokens + self.cache_read + self.cache_creation


@dataclass
class Gap:
    thread_id: str
    thread_kind: str  # "main" or "subagent"
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
                    threads.append((project, thread_id, "subagent", sub_path))
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


def miss_cost_x_usd(gap: Gap) -> tuple[float, float]:
    """Unmitigated (0-ping) miss cost for this gap, in x units and in USD."""
    if gap.is_cold_start or not gap.end.is_cache_miss:
        return 0.0, 0.0
    price = base_price_for_model(gap.end.model)
    x_usd = gap.end.prompt_tokens_total * price
    premium_usd = gap.end.cache_creation * price * MISS_PREMIUM_X
    return MISS_PREMIUM_X, premium_usd


def gap_x_usd_unit(gap: Gap) -> float:
    """The dollar value of one 'x' unit for this gap's ending call."""
    price = base_price_for_model(gap.end.model)
    return gap.end.prompt_tokens_total * price


def summarize_gaps_by_cause(gaps: list[Gap]) -> dict:
    by_cause: dict[str, list[Gap]] = defaultdict(list)
    for g in gaps:
        if g.is_cold_start:
            continue
        by_cause[g.cause].append(g)

    out = {}
    for cause, gs in sorted(by_cause.items()):
        gap_minutes = [g.gap_min for g in gs]
        dist = gap_dist(gap_minutes)

        # Unmitigated (today, no pings) miss cost average, in x units and USD.
        x_costs = []
        usd_costs = []
        for g in gs:
            x, usd = miss_cost_x_usd(g)
            x_costs.append(x if g.end.is_cache_miss else 0.0)
            usd_costs.append(usd)
        avg_x = sum(x_costs) / len(gs) if gs else 0.0
        total_usd = sum(usd_costs)

        # Ping-budget simulation: average cost per gap, in x units, for each N.
        sim = {}
        for n in PING_BUDGETS:
            costs = [sim_cost_x(g.gap_min, n) for g in gs]
            sim[n] = round(sum(costs) / len(costs), 3) if costs else 0.0
        best_n = min(sim, key=lambda n: sim[n])

        out[cause] = {
            "gap_distribution": dist,
            "miss_cost_no_pings_x_per_gap": round(avg_x, 3),
            "miss_cost_no_pings_usd_total": round(total_usd, 4),
            "ping_sim_x_per_gap_by_n": sim,
            "best_n": best_n,
            "best_n_cost_x_per_gap": sim[best_n],
        }
    return out


def summarize_cold_starts(gaps: list[Gap]) -> dict:
    cold = [g for g in gaps if g.is_cold_start]
    misses = sum(1 for g in cold if g.end.is_cache_miss)
    usd = 0.0
    for g in cold:
        if g.end.is_cache_miss:
            price = base_price_for_model(g.end.model)
            usd += g.end.cache_creation * price * MISS_PREMIUM_X
    return {"threads": len(cold), "cache_misses": misses, "miss_cost_usd_total": round(usd, 4)}


# ---------------------------------------------------------------------------
# Mislabel check and failure stats (needs the DB)
# ---------------------------------------------------------------------------


MISLABEL_MATCH_WINDOW_SEC = 60.0


def mislabel_check(all_gaps: list[Gap], all_calls_by_session: dict[str, list[Call]], db_rows: list[DBRow]) -> dict:
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

    for project, gaps in sorted(per_project_gaps.items()):
        main_gaps = [g for g in gaps if g.thread_kind == "main"]
        sub_gaps = [g for g in gaps if g.thread_kind == "subagent"]
        report["projects"][project] = {
            "threads": len(per_project_calls[project]),
            "main": {
                "cold_starts": summarize_cold_starts(main_gaps),
                "by_cause": summarize_gaps_by_cause(main_gaps),
            },
            "subagent": {
                "cold_starts": summarize_cold_starts(sub_gaps),
                "by_cause": summarize_gaps_by_cause(sub_gaps),
            },
        }

    if db_rows:
        all_calls_by_session: dict[str, list[Call]] = {}
        report["mislabel_check"] = mislabel_check(all_gaps_global, all_calls_by_session, db_rows)
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
            f"Miss cost, no pings: {report['miss_cost_no_pings_x_per_gap']}x/gap "
            f"(${report['miss_cost_no_pings_usd_total']} total)"
        )
        lines.append(f"Best N (0-12): {report['best_n']}, cost {report['ping_sim_x_per_gap_by_n'][report['best_n']]}x/gap")
        for n in (0, 2, 4, 6, 10):
            lines.append(f"  N={n}: {report['ping_sim_x_per_gap_by_n'].get(n)}x/gap")
        f = report["failures"]
        lines.append(
            f"Failures: {f['failure_count']}, hang median {f['hang_ms_median']}ms, "
            f"misses following a failure: {f['cache_misses_following_a_failure_run']}"
        )
        return "\n".join(lines)

    for project, data in report["projects"].items():
        lines.append(f"\n== {project} ({data['threads']} threads) ==")
        for kind in ("main", "subagent"):
            kd = data[kind]
            cs = kd["cold_starts"]
            lines.append(
                f"  {kind}: cold starts {cs['threads']} ({cs['cache_misses']} misses, "
                f"${cs['miss_cost_usd_total']})"
            )
            for cause, stats in kd["by_cause"].items():
                dist = stats["gap_distribution"]
                lines.append(
                    f"    {cause}: {dist['count']} gaps, median {dist['median_min']}min, "
                    f"p90 {dist['p90_min']}min | miss cost {stats['miss_cost_no_pings_x_per_gap']}x/gap "
                    f"(${stats['miss_cost_no_pings_usd_total']}) | best N={stats['best_n']} "
                    f"-> {stats['best_n_cost_x_per_gap']}x/gap"
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


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--projects-dir", action="append", default=[], help="A ~/.claude/projects/<project> dir. Repeatable.")
    ap.add_argument("--db", action="append", default=[], help="A dashboard-*.db path. Repeatable.")
    ap.add_argument("--json", action="store_true", help="Print machine-readable JSON instead of a summary.")
    args = ap.parse_args()

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
