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

WAKE_CAUSES = ("tool_result", "background", "teammate", "human", "system", "retry", "unknown")

# Hardcoded base INPUT price per token, in USD, by model family — used ONLY as a
# fallback when no --db is given, or the DB has no priced rows for a model. This
# is the "x" unit the issue's cost tables use. Cache write (5m) is priced at
# 1.25x this, cache write (1h) at 2x, cache read at 0.1x — which is exactly
# where PING_COST_X and MISS_PREMIUM_X below come from (1.25 - 0.1 = 1.15).
#
# PR #425 review finding 1: the first version of this table used Anthropic's
# published LIST prices, which overstated the actual billed rate by up to 3.8x
# (opus-5-5: $15/MTok listed vs $3.91/MTok actually billed on this gateway).
# These corrected numbers are fit_model_prices() run on both local dashboard
# DBs — the same fit a --db run does fresh, kept here as the best available
# fallback for a model neither local DB has billed. See PriceTable below: when
# --db is given, its DB-fitted price always wins over this table.
MODEL_BASE_INPUT_PRICE_PER_TOKEN = {
    "claude-opus-5-5": 3.91e-6,
    "claude-opus-5": 5.00e-6,
    "claude-sonnet-5-5": 2.00e-6,  # no local data for this exact id; same tier as sonnet-5
    "claude-sonnet-5": 2.00e-6,
    "claude-haiku-5-5": 1.00e-6,  # no local data for this exact id; same tier as haiku-4-5
    "claude-haiku-4-5": 1.00e-6,
}
DEFAULT_BASE_INPUT_PRICE = 2e-6  # fall back to sonnet-5-ish pricing for a wholly unknown model


def fit_model_prices(db_rows: "list[DBRow]") -> dict[str, float]:
    """Fit a per-model base input price (USD/token) from what the DB actually billed.

    cost_usd = b * (fresh_input + 0.1*cache_read + 1.25*cache_write + 5*output),
    solved per model by least squares through the origin: b = sum(x*y)/sum(x*x).
    This tracks the gateway's real billed rate, unlike a list price — see PR
    #425 review finding 1 (list price overstated opus-5-5 by 3.8x).
    """
    sums: dict[str, list[float]] = defaultdict(lambda: [0.0, 0.0])
    for r in db_rows:
        if r.cost_usd <= 0:
            continue
        x = r.fresh_input + 0.1 * r.cache_read + 1.25 * r.cache_write + 5 * r.output_tokens
        if x <= 0:
            continue
        s = sums[r.model]
        s[0] += x * r.cost_usd
        s[1] += x * x
    return {model: sxy / sxx for model, (sxy, sxx) in sums.items() if sxx > 0}


class PriceTable:
    """Per-model base input price: DB-fitted where available, else hardcoded.

    Matching is by substring, since a transcript model id can be a full
    Bedrock ARN like "anthropic.claude-haiku-4-5-20251001-v1:0" rather than
    the DB's short id. Keys are checked longest-first so "claude-opus-5-5" is
    never mistaken for a substring match on the shorter "claude-opus-5".
    """

    def __init__(self, fitted: dict[str, float] | None = None):
        self.fitted = fitted or {}

    def price_and_source(self, model: str) -> tuple[float, str]:
        if not model:
            return DEFAULT_BASE_INPUT_PRICE, "default_fallback"
        for key in sorted(self.fitted, key=len, reverse=True):
            if key in model:
                return self.fitted[key], "db_fit"
        for key in sorted(MODEL_BASE_INPUT_PRICE_PER_TOKEN, key=len, reverse=True):
            if key in model:
                return MODEL_BASE_INPUT_PRICE_PER_TOKEN[key], "hardcoded_fallback"
        return DEFAULT_BASE_INPUT_PRICE, "default_fallback"

    def price(self, model: str) -> float:
        return self.price_and_source(model)[0]

    def sources_used(self, models: Iterable[str]) -> dict[str, dict]:
        """{model: {"price_per_mtok":, "source":}} for every distinct model
        string seen, for the report — so a reader can see which number priced
        which traffic, per review finding 1's "print the price source"."""
        out = {}
        for m in sorted(set(models)):
            if not m:
                continue
            price, source = self.price_and_source(m)
            out[m] = {"price_per_mtok": round(price * 1e6, 4), "source": source}
        return out


def ping_window_minutes(n_pings: int) -> float:
    """How long N pings keep the cache warm for, in minutes."""
    return PING_INTERVAL_MIN * n_pings + 5.0


def sim_cost_x(gap_min: float, n_pings: int) -> float:
    """Cost of one gap, in units of x, under a budget of N pings.

    Per issue #424's technical notes: a gap g costs 0.1*floor(g/4.67) when it
    ends inside the N-ping warm window, and 0.1*N + 1.15 otherwise (N pings
    spent, then still a miss).

    PR #425 review finding 3: that first term must never exceed the pings the
    budget actually sends. At N=0, floor(g/4.67) can be 1 for a gap just past
    one ping interval (e.g. 4.8 min), which charged 0.1x for a ping N=0 never
    fires. Capped at min(n_pings, floor(g/4.67)).
    """
    window = ping_window_minutes(n_pings)
    if gap_min <= window:
        pings_fired = min(n_pings, gap_min // PING_INTERVAL_MIN)
        return PING_COST_X * pings_fired
    return PING_COST_X * n_pings + MISS_PREMIUM_X


# ---------------------------------------------------------------------------
# Transcript parsing
# ---------------------------------------------------------------------------

# PR #425 review finding 2: a message from another Claude session also arrives
# as <cross-session-message ...>, not only <teammate-message ...>; both are a
# "teammate" wake. #424 needs this same marker list for its own wake_cause field.
TEAMMATE_RE = re.compile(r"<teammate-message|<cross-session-message")
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

    PR #425 review finding 2: an `isMeta` entry (a skill load, a system nudge,
    a subagent hand-back notice) must never be classified as `human` — it was
    not typed by a person. If it carries no recognized marker of its own, it
    gets its own `system` cause rather than falling through to `human`.
    """
    last_cause = None
    for entry in between_entries:
        if entry.get("type") != "user":
            continue
        text, only_tool_result = entry_user_text(entry)
        is_meta = bool(entry.get("isMeta"))
        if TEAMMATE_RE.search(text):
            last_cause = "teammate"
        elif TASK_NOTIF_RE.search(text):
            last_cause = "background"
        elif is_meta:
            last_cause = "system"
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


MATCH_WINDOW_SEC = 15.0  # see approx_response_ts() for why this can be tight.


def approx_response_ts(row: "DBRow") -> float:
    """A DB row's `ts` is the request's START. The transcript only has the
    response's ARRIVAL time. Approximate the row's arrival as ts + upstream_ms.

    PR #425 review finding 5: matching on raw `ts` alone (request start) needs
    a loose +-60s window and still misses ~4% of calls on the validation
    session. Adding upstream_ms tightens the match to +-15s and raises the
    match rate from 96.2% to 99.8% on the same data.
    """
    return row.ts_ms / 1000.0 + (row.upstream_ms or 0.0) / 1000.0


class DBIndex:
    """Matches a transcript Gap's ending call to the DB row it produced.

    The DB has no thread id (issue #423), so matching is by session_id +
    exact cache_read token count + nearest approximate response time (see
    approx_response_ts) within MATCH_WINDOW_SEC. Used to pull the REAL
    keepalive_pings already spent on a gap, for the OBSERVED cost column, and
    by mislabel_check. Best-effort, not exact — see README.
    """

    def __init__(self, db_rows: list[DBRow]):
        self._by_key: dict[tuple[str, int], list[tuple[float, DBRow]]] = defaultdict(list)
        for r in db_rows:
            self._by_key[(r.session_id, r.cache_read)].append((approx_response_ts(r), r))
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


def miss_premium_usd(call: Call, prices: PriceTable) -> float:
    """What this call's cache_creation cost over what a hit would have, in USD."""
    return call.cache_creation * prices.price(call.model) * MISS_PREMIUM_X


def observed_cost_x_usd(gap: Gap, pings: int, prices: PriceTable) -> tuple[float, float]:
    """What this gap ACTUALLY cost: `pings` real keep-alive pings (0 if unknown —
    the DB does not cover every session) plus a miss premium if it was still a
    miss. This is the "OBSERVED" column: real traffic, real (partial) mitigation,
    using the corrected hit/miss rule (see Call.is_cache_hit). It is deliberately
    NOT the same thing as the N=0 simulation, which assumes zero real pinging
    ever happened; where the DB shows pings already in flight, observed can come
    in below the N=0 simulated cost for the same gap.

    PR #425 review finding 4: an errored call (is_error) is neither a hit nor a
    miss — Call.is_cache_miss already excludes it, so it gets no miss premium
    here, only whatever real ping cost preceded it (usually 0).
    """
    is_miss = gap.end.is_cache_miss
    x = PING_COST_X * pings + (MISS_PREMIUM_X if is_miss else 0.0)
    price = prices.price(gap.end.model)
    ping_usd = pings * PING_COST_X * price * gap.end.prompt_tokens_total
    usd = ping_usd + (miss_premium_usd(gap.end, prices) if is_miss else 0.0)
    return x, usd


def summarize_gaps_by_cause(gaps: list[Gap], db_index: "DBIndex | None" = None, prices: PriceTable | None = None) -> dict:
    prices = prices or PriceTable()
    by_cause: dict[str, list[Gap]] = defaultdict(list)
    for g in gaps:
        if g.is_cold_start:
            continue
        by_cause[g.cause].append(g)

    out = {}
    for cause, gs in sorted(by_cause.items()):
        gap_minutes = [g.gap_min for g in gs]
        dist = gap_dist(gap_minutes)

        # PR #425 review finding 4: hits + misses + errors must equal len(gs).
        # An errored call is neither a hit nor a miss (both properties exclude
        # is_error) — count it separately instead of folding it into "misses"
        # via len(gs) - hits, which charged it a miss it never paid.
        hits = sum(1 for g in gs if g.end.is_cache_hit)
        errors = sum(1 for g in gs if g.end.is_error)
        misses = sum(1 for g in gs if g.end.is_cache_miss)

        # OBSERVED: what really happened on this traffic, including real pings
        # already sent (from the DB, where it covers the session) before this
        # call landed.
        x_costs, usd_costs, pings_used = [], [], []
        for g in gs:
            pings = db_index.pings_for(g) if db_index else 0
            x, usd = observed_cost_x_usd(g, pings, prices)
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
            "errors": errors,
            "observed_cost_x_per_gap": round(avg_observed_x, 3),
            "observed_cost_usd_total": round(total_observed_usd, 4),
            "observed_pings_total": sum(pings_used),
            "sim_cost_x_per_gap_by_n": sim,
            "best_n": best_n,
            "best_n_cost_x_per_gap": sim[best_n],
        }
    return out


def summarize_cold_starts(gaps: list[Gap], prices: PriceTable | None = None) -> dict:
    prices = prices or PriceTable()
    cold = [g for g in gaps if g.is_cold_start]
    misses = sum(1 for g in cold if g.end.is_cache_miss)
    usd = sum(miss_premium_usd(g.end, prices) for g in cold if g.end.is_cache_miss)
    return {"threads": len(cold), "cache_misses": misses, "miss_cost_usd_total": round(usd, 4)}


# ---------------------------------------------------------------------------
# Mislabel check and failure stats (needs the DB)
# ---------------------------------------------------------------------------


def mislabel_check(all_gaps: list[Gap], db_rows: list[DBRow]) -> dict:
    """Count DB prefix_change rows that are really per-thread TTL expiry / cold start.

    The DB has no thread id (that is #423's job), so a row is matched to a
    transcript gap/cold-start event by: same session_id, same cache_read token
    count (both sides of a genuine miss agree on this), and the nearest
    approximate response time (see approx_response_ts) within MATCH_WINDOW_SEC.
    This is a best-effort match, not an exact one; see README.
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
    mislabeled_full_cost_usd = 0.0
    checked = 0
    for row in db_rows:
        if row.cache_miss_reason != "prefix_change":
            continue
        events = session_events.get(row.session_id)
        if not events:
            continue
        checked += 1
        row_ts = approx_response_ts(row)
        best_label, best_dt = None, MATCH_WINDOW_SEC
        for ts, label, cache_read in events:
            if cache_read != row.cache_read:
                continue
            dt = abs(ts - row_ts)
            if dt <= best_dt:
                best_dt, best_label = dt, label
        if best_label in ("ttl_expiry", "cold_start"):
            mislabeled_count += 1
            mislabeled_full_cost_usd += row.cost_usd
    return {
        "db_prefix_change_rows_checked": checked,
        "mislabeled_count": mislabeled_count,
        # PR #425 review finding 7: this is the mislabeled rows' FULL recorded
        # cost_usd (what the dashboard calls prefix_change_cost_usd), not just
        # their miss premium over a hit — name it that way, not "mislabeled_usd".
        "mislabeled_full_cost_usd": round(mislabeled_full_cost_usd, 4),
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
    prices = PriceTable(fit_model_prices(db_rows))
    by_key: dict[tuple[str, str], list[DBRow]] = defaultdict(list)
    for r in db_rows:
        by_key[(r.session_id, r.model)].append(r)

    gaps_min: list[float] = []
    hits = misses = 0
    observed_x_total = 0.0
    observed_usd_total = 0.0
    observed_pings_total = 0
    sim_totals = {n: [] for n in PING_BUDGETS}

    for key, rows in by_key.items():
        rows_sorted = sorted(rows, key=lambda r: r.ts_ms)
        for i in range(1, len(rows_sorted)):
            prev, cur = rows_sorted[i - 1], rows_sorted[i]
            gap_min = (cur.ts_ms - prev.ts_ms) / 1000.0 / 60.0
            if gap_min < GAP_THRESHOLD_MIN:
                continue
            gaps_min.append(gap_min)
            price = prices.price(cur.model)
            # PR #425 review finding 6: use the SAME hit rule as transcript mode
            # (cache_read / cache_write), not cache_miss_reason — that label
            # depends on the ttl_expiry/prefix_change distinction issue #423 is
            # still fixing, so a production number built on it inherits that bug.
            is_hit = cur.cache_read > 0 and cur.cache_write < 0.5 * cur.cache_read
            is_miss = not is_hit
            hits += 1 if is_hit else 0
            misses += 1 if is_miss else 0
            # PR #425 review finding 6: also use the row's own real keepalive_pings
            # (the production DB has this column) instead of assuming 0.
            pings = cur.keepalive_pings or 0
            observed_pings_total += pings
            observed_x_total += PING_COST_X * pings + (MISS_PREMIUM_X if is_miss else 0.0)
            observed_usd_total += pings * PING_COST_X * price * (cur.fresh_input + cur.cache_read + cur.cache_write)
            if is_miss:
                observed_usd_total += cur.cache_write * price * MISS_PREMIUM_X
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
        "price_sources": prices.sources_used(r.model for r in db_rows),
        "gap_distribution": gap_dist(gaps_min),
        "hits": hits,
        "misses": misses,
        "observed_cost_x_per_gap": round(observed_x_total / len(gaps_min), 3) if gaps_min else 0.0,
        "observed_cost_usd_total": round(observed_usd_total, 4),
        "observed_pings_total": observed_pings_total,
        "sim_cost_x_per_gap_by_n": sim,
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
    prices = PriceTable(fit_model_prices(db_rows) if db_rows else None)
    report["price_sources"] = prices.sources_used(g.end.model for g in all_gaps_global)

    for project, gaps in sorted(per_project_gaps.items()):
        report["projects"][project] = {"threads": len(per_project_calls[project])}
        for kind in ("main", "subagent", "team_thread"):
            kind_gaps = [g for g in gaps if g.thread_kind == kind]
            report["projects"][project][kind] = {
                "cold_starts": summarize_cold_starts(kind_gaps, prices),
                "by_cause": summarize_gaps_by_cause(kind_gaps, db_index, prices),
            }

    if db_rows:
        report["mislabel_check"] = mislabel_check(all_gaps_global, db_rows)
        report["failures"] = failure_stats(db_rows)
    else:
        report["mislabel_check"] = None
        report["failures"] = None

    return report


def _price_sources_lines(price_sources: dict) -> list[str]:
    if not price_sources:
        return []
    lines = ["Prices used (USD/MTok, source):"]
    for model, info in price_sources.items():
        lines.append(f"  {model}: ${info['price_per_mtok']}/MTok ({info['source']})")
    return lines


def human_summary(report: dict) -> str:
    lines = []
    if report["mode"] == "db_only_approximate":
        lines.append("DB-only mode (no transcripts) — approximate, no wake causes.")
        lines.append(report["note"])
        lines.extend(_price_sources_lines(report.get("price_sources")))
        d = report["gap_distribution"]
        lines.append(
            f"Gaps >= {GAP_THRESHOLD_MIN} min: {d['count']}, median {d['median_min']} min, "
            f"p90 {d['p90_min']} min, max {d['max_min']} min "
            f"(hits={report['hits']} misses={report['misses']})"
        )
        lines.append(
            f"Observed: {report['observed_cost_x_per_gap']}x/gap "
            f"(${report['observed_cost_usd_total']} total, {report['observed_pings_total']} real pings seen)"
        )
        lines.append(f"Simulated best N (0-12): {report['best_n']}, cost {report['sim_cost_x_per_gap_by_n'][report['best_n']]}x/gap")
        for n in (0, 2, 4, 6, 10):
            lines.append(f"  simulated N={n}: {report['sim_cost_x_per_gap_by_n'].get(n)}x/gap")
        f = report["failures"]
        lines.append(
            f"Failures: {f['failure_count']}, hang median {f['hang_ms_median']}ms, "
            f"misses following a failure: {f['cache_misses_following_a_failure_run']}"
        )
        return "\n".join(lines)

    lines.extend(_price_sources_lines(report.get("price_sources")))

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
                    f"    {cause}: {dist['count']} gaps (hits={stats['hits']} misses={stats['misses']} "
                    f"errors={stats['errors']}), median {dist['median_min']}min, p90 {dist['p90_min']}min"
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
            f"DB prefix_change rows are really TTL expiry/cold start "
            f"(full recorded cost ${mc['mislabeled_full_cost_usd']})"
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


def _write_user_meta_line(fh, ts_iso: str, text: str) -> None:
    """An isMeta entry with no recognized marker — e.g. a skill load notice.
    Per PR #425 review finding 2, this must classify as "system", not "human".
    """
    entry = {"type": "user", "timestamp": ts_iso, "isMeta": True, "message": {"content": [{"type": "text", "text": text}]}}
    fh.write(json.dumps(entry) + "\n")


# PRICE used by this self-test's assertions: the hardcoded fallback for
# claude-opus-5-5 (no --db in these scenarios, so PriceTable() falls back to
# MODEL_BASE_INPUT_PRICE_PER_TOKEN). Keep this in sync with that table.
_SELF_TEST_PRICE = MODEL_BASE_INPUT_PRICE_PER_TOKEN["claude-opus-5-5"]


def run_self_test() -> bool:
    """Build tiny synthetic transcripts and a tiny synthetic DB with known
    gaps, hits/misses, causes, and DB rows; assert the exact observed and
    simulated numbers. Exits non-zero on failure.
    """
    import math
    import sqlite3
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
            # Call 4: an upstream failure (isApiErrorMessage). Neither hit nor miss.
            _write_call_line(fh, iso(1010), "m4", "<synthetic>", cache_read=0, cache_creation=0, is_error=True)
            # Gap 3: 400s (6.667 min) after a failed call -> cause=retry, regardless
            # of what's in between (there is nothing here), cache_read=0
            # cache_creation=3000 -> miss.
            _write_call_line(fh, iso(1410), "m5", "claude-opus-5-5", cache_read=0, cache_creation=3000)

        subdir = os.path.join(tmp, session_id, "subagents")
        os.makedirs(subdir)
        sub_path = os.path.join(subdir, "agent-x.jsonl")
        with open(sub_path, "w") as fh:
            # Cold start: cache_read=0, cache_creation=1000 -> miss.
            _write_call_line(fh, iso(0), "s1", "claude-opus-5-5", cache_read=0, cache_creation=1000)
            fh.write(json.dumps({"type": "user", "timestamp": iso(1), "message": {"content": "<task-notification>\n<task-id>x</task-id>\n</task-notification>"}}) + "\n")
            # Gap: 300s (5.0 min), cause=background, cache_read=800 cache_creation=100 -> hit.
            _write_call_line(fh, iso(300), "s2", "claude-opus-5-5", cache_read=800, cache_creation=100)

        # A team_thread (in-process agent-team teammate): one gap per teammate
        # wake format, plus an isMeta entry with no marker (-> "system").
        team_name = "agent-team-reviewer"
        team_path = os.path.join(subdir, f"{team_name}.jsonl")
        with open(team_path, "w") as fh:
            _write_call_line(fh, iso(0), "t1", "claude-opus-5-5", cache_read=0, cache_creation=500)
            _write_user_text_line(fh, iso(1), 'Another Claude session sent a message:\n<teammate-message teammate_id="x">hi</teammate-message>')
            # Gap: 5 min, cause=teammate (via <teammate-message).
            _write_call_line(fh, iso(300), "t2", "claude-opus-5-5", cache_read=400, cache_creation=50)
            _write_user_text_line(fh, iso(301), 'Another Claude session sent a message:\n<cross-session-message from="x">hi</cross-session-message>')
            # Gap: 5 min, cause=teammate (via <cross-session-message).
            _write_call_line(fh, iso(600), "t3", "claude-opus-5-5", cache_read=400, cache_creation=50)
            _write_user_meta_line(fh, iso(601), "Base directory for this skill: /tmp/x")
            # Gap: 5 min, cause=system (isMeta, no recognized marker).
            _write_call_line(fh, iso(900), "t4", "claude-opus-5-5", cache_read=400, cache_creation=50)
        with open(os.path.join(subdir, f"{team_name}.meta.json"), "w") as fh:
            json.dump({"taskKind": "in_process_teammate"}, fh)

        threads = find_threads([tmp])
        kinds = {t[2] for t in threads}
        check("thread kinds found", kinds, {"main", "subagent", "team_thread"})

        all_gaps = []
        for project, thread_id, thread_kind, path in threads:
            _, gaps = iter_calls_and_gaps(path, thread_id, thread_kind, project)
            all_gaps.extend(gaps)

        main_gaps = [g for g in all_gaps if g.thread_kind == "main"]
        sub_gaps = [g for g in all_gaps if g.thread_kind == "subagent"]
        team_gaps = [g for g in all_gaps if g.thread_kind == "team_thread"]

        main_cold = summarize_cold_starts(main_gaps)
        check("main cold starts", main_cold["threads"], 1)
        check("main cold start misses", main_cold["cache_misses"], 1)
        check("main cold start USD", main_cold["miss_cost_usd_total"], round(2000 * _SELF_TEST_PRICE * 1.15, 4))

        main_by_cause = summarize_gaps_by_cause(main_gaps, db_index=None)
        check("main causes", set(main_by_cause), {"human", "tool_result", "retry"})

        human = main_by_cause["human"]
        check("human gap count", human["gap_distribution"]["count"], 1)
        check("human gap minutes", human["gap_distribution"]["median_min"], round(600 / 60, 2))
        check("human hits", human["hits"], 0)
        check("human misses", human["misses"], 1)
        check("human errors", human["errors"], 0)
        check("human observed x/gap (no DB -> 0 pings known)", human["observed_cost_x_per_gap"], 1.15)
        check("human observed USD", human["observed_cost_usd_total"], round(5000 * _SELF_TEST_PRICE * 1.15, 4))
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

        # Finding 4: a gap ENDING on an error (m3 -> m4, 10s) is below threshold
        # so it never becomes a Gap at all. The gap that matters here is
        # m4(error) -> m5, which must be "retry" and must NOT count m5 as an
        # error (m5 itself succeeded) and must NOT give m5 a miss premium of
        # zero just because the PRECEDING call errored.
        retry = main_by_cause["retry"]
        check("retry gap count", retry["gap_distribution"]["count"], 1)
        check("retry hits", retry["hits"], 0)
        check("retry misses", retry["misses"], 1)
        check("retry errors", retry["errors"], 0)
        check("retry observed USD (m5's own miss premium)", retry["observed_cost_usd_total"], round(3000 * _SELF_TEST_PRICE * 1.15, 4))

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

        # Finding 2: both teammate-message formats land in the SAME "teammate"
        # cause, and the isMeta/no-marker entry lands in "system", not "human".
        team_by_cause = summarize_gaps_by_cause(team_gaps, db_index=None)
        check("team_thread causes", set(team_by_cause), {"teammate", "system"})
        check("team_thread teammate gap count (both formats)", team_by_cause["teammate"]["gap_distribution"]["count"], 2)
        check("team_thread system gap count", team_by_cause["system"]["gap_distribution"]["count"], 1)

        # --- Finding 1 & 5: a tiny synthetic DB, DB-fitted prices, and the
        # response-time-approximate match (ts + upstream_ms). ---
        db_path = os.path.join(tmp, "self-test.db")
        con = sqlite3.connect(db_path)
        con.execute(
            """
            create table requests (
                id integer primary key, ts integer, session_id text, model text,
                status integer, cache_read integer, cache_write integer,
                fresh_input integer, output_tokens integer, cost_usd real,
                cache_miss_reason text, upstream_ms real, keepalive integer,
                keepalive_pings integer, saved_unique integer, stop_reason text
            )
            """
        )
        # Row for m2 (the human-cause miss): request started 2.5s before the
        # transcript's response-arrival timestamp, upstream took 2.0s -- so
        # ts + upstream_ms lands 0.5s from the transcript's ts, well inside
        # MATCH_WINDOW_SEC, while raw ts alone is 2.5s off (still inside the
        # old 60s window, so this alone doesn't distinguish the two -- the
        # mislabel/ping-match assertions below just need SOME match to land).
        m2_ts_ms = int((iso_to_epoch(iso(600)) - 2.5) * 1000)
        # cost_usd fit to a known price: cost = price * (fresh + 0.1*cr + 1.25*cw + 5*out)
        # fresh=10 (Call's hardcoded input_tokens), cache_read=0, cache_write=5000, output=50
        fit_price = 2.5e-6
        m2_cost = fit_price * (10 + 0.1 * 0 + 1.25 * 5000 + 5 * 50)
        con.execute(
            "insert into requests (ts, session_id, model, status, cache_read, cache_write, fresh_input, "
            "output_tokens, cost_usd, cache_miss_reason, upstream_ms, keepalive, keepalive_pings, "
            "saved_unique, stop_reason) values (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
            (m2_ts_ms, session_id, "claude-opus-5-5", 200, 0, 5000, 10, 50, m2_cost, "prefix_change", 2000.0, 0, 2, 0, "end_turn"),
        )
        con.commit()
        con.close()

        db_rows = read_db_rows(db_path)
        check("db rows read", len(db_rows), 1)

        fitted = fit_model_prices(db_rows)
        check("fitted price for claude-opus-5-5", fitted.get("claude-opus-5-5", 0.0), fit_price)

        prices = PriceTable(fitted)
        check("PriceTable uses the DB fit, not the hardcoded fallback", prices.price_and_source("claude-opus-5-5")[1], "db_fit")

        db_index = DBIndex(db_rows)
        human_db = summarize_gaps_by_cause(main_gaps, db_index, prices)["human"]
        check("human observed_pings_total pulls the DB's real 2 pings", human_db["observed_pings_total"], 2)
        # Finding 1: USD now uses the fitted price, not the hardcoded fallback.
        check(
            "human observed USD uses the DB-fitted price",
            human_db["observed_cost_usd_total"],
            round(2 * PING_COST_X * fit_price * (10 + 0 + 5000) + 5000 * fit_price * MISS_PREMIUM_X, 4),
        )

        mc = mislabel_check(main_gaps, db_rows)
        check("mislabel_check finds the DB's prefix_change row", mc["db_prefix_change_rows_checked"], 1)
        check("mislabel_check flags it as a TTL expiry (it's a 10-min main-thread gap)", mc["mislabeled_count"], 1)
        check("mislabel_check reports the row's FULL cost, not just its premium", mc["mislabeled_full_cost_usd"], round(m2_cost, 4))

        # --- Finding 6: DB-only mode uses the real hit rule and real pings. ---
        # Pings/hit-or-miss are attributed to whichever row ENDS a gap (same as
        # DBIndex.pings_for in transcript mode) — row 1 is only the gap's start.
        db_only_rows = [
            DBRow(
                id=1, ts_ms=0, session_id="s", model="claude-opus-5-5", status=200,
                cache_read=0, cache_write=1000, fresh_input=10, output_tokens=50,
                cost_usd=0.01, cache_miss_reason="unknown", upstream_ms=100.0,
                keepalive=0, keepalive_pings=0, saved_unique=0, stop_reason="end_turn",
            ),
            DBRow(
                id=2, ts_ms=10 * 60 * 1000, session_id="s", model="claude-opus-5-5", status=200,
                cache_read=9000, cache_write=100, fresh_input=10, output_tokens=50,
                # A hit: cache_write (100) < half of cache_read (9000);
                # cache_miss_reason deliberately mislabeled "prefix_change" (the
                # #423 bug) to prove db_only_report ignores that column now.
                cost_usd=0.001, cache_miss_reason="prefix_change", upstream_ms=100.0,
                keepalive=0, keepalive_pings=0, saved_unique=0, stop_reason="end_turn",
            ),
            DBRow(
                id=3, ts_ms=20 * 60 * 1000, session_id="s", model="claude-opus-5-5", status=200,
                cache_read=0, cache_write=5000, fresh_input=10, output_tokens=50,
                # A miss, mislabeled the other way ("hit") to prove the same thing,
                # with 1 real keepalive ping already spent on this gap.
                cost_usd=0.02, cache_miss_reason="hit", upstream_ms=100.0,
                keepalive=0, keepalive_pings=1, saved_unique=0, stop_reason="end_turn",
            ),
        ]
        db_only = db_only_report(db_only_rows)
        check("db_only hits (ignores the wrong cache_miss_reason label)", db_only["hits"], 1)
        check("db_only misses (ignores the wrong cache_miss_reason label)", db_only["misses"], 1)
        check("db_only observed_pings_total (real keepalive_pings)", db_only["observed_pings_total"], 1)

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
