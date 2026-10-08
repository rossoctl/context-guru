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

# waiting_for: known the moment the gap STARTS (the response that ends the
# turn before it), unlike wake_cause (known only once the gap ends). "own_tool"
# carries a tool name separately (Call.tool_use_names / the own_tool breakdown);
# AskUserQuestion and ExitPlanMode are the one exception — they are a tool_use
# that waits on the human, so they classify as "human", not "own_tool".
WAITING_FOR_VALUES = ("own_tool", "background", "teammate", "human", "retry")
ASK_HUMAN_TOOLS = frozenset({"AskUserQuestion", "ExitPlanMode"})

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

# --- waiting_for / waiting_on: open background tasks and open teammate waits.
#
# Found by reading real tool_result and user-entry text in both local
# projects (see README's "Open background tasks" section) rather than
# guessing at the shapes:
#
#   start                                          | finish
#   ------------------------------------------------|---------------------------------------
#   "running in background with ID: X."            | <task-notification><task-id>X</task-id>
#   "Monitor started (task X, expires in <dur>"    | <task-notification><task-id>X</task-id>,
#                                                   |   OR the stated expiry elapsing
#   "Async agent launched successfully... agentId: X" | <agent-message from="X"> (NOT a
#                                                   |   task-notification — this is the one
#                                                   |   place a background task's finish
#                                                   |   signal differs by start kind)
#
# A TaskStop(task_id=X) tool call also finishes X, whichever way it started.
# PR #425 review leak 2 follow-up: a <task-notification> is NOT always a
# finish signal. A Monitor fires ONE of these on every matching event while
# it keeps running — found by checking real data: 49 of 70 Monitor-event
# notifications in both local projects are mid-watch events, not the final
# one, and treating every one as a finish closed the task on its FIRST
# event instead of its last. Only the terminal notice says so, in a fixed
# bracketed form: "[Monitor expired after <dur> ...]". A task-notification
# with no "Monitor event:" summary at all (a one-shot Bash/Agent/Skill
# background task) always finishes, as before.
TASK_NOTIFICATION_BLOCK_RE = re.compile(r"<task-notification>(.*?)</task-notification>", re.DOTALL)
TASK_ID_IN_BLOCK_RE = re.compile(r"<task-id>([^<]+)</task-id>")
MONITOR_EVENT_SUMMARY_RE = re.compile(r"<summary>Monitor event:")
MONITOR_EXPIRED_RE = re.compile(r"Monitor expired")


def task_notification_finishes(block: str) -> bool:
    if MONITOR_EVENT_SUMMARY_RE.search(block):
        return bool(MONITOR_EXPIRED_RE.search(block))
    return True
AGENT_MESSAGE_FINISH_RE = re.compile(r'<agent-message\s+from="([^"]+)"')
BG_BASH_START_RE = re.compile(r"running in background with ID:\s*(\S+?)\.")
# PR #425 review leak 2/3 follow-up: "Monitor started (task X, ...)" has (at
# least) 3 real detail clauses, found by grepping every instance in both
# local projects rather than assuming one shape:
#   "expires in 1m 30s unless the source ends first; ..."  (a timed watch)
#   "timeout 1800000ms"                                    (also timed, in ms)
#   "persistent — runs until TaskStop or session end"      (no expiry at all)
# The old regex matched only the first, so a Monitor opened with either of
# the other two never registered as open at all.
MONITOR_START_RE = re.compile(r"Monitor started \(task\s+(\S+?),\s*([^)]*)\)")
MONITOR_EXPIRES_IN_RE = re.compile(r"expires in\s+([^;]+?);")
MONITOR_TIMEOUT_MS_RE = re.compile(r"timeout\s+(\d+)ms")
ASYNC_AGENT_START_RE = re.compile(r"Async agent launched successfully.*?agentId:\s*(\S+)", re.DOTALL)
# A Skill run in the background (forked execution) — e.g. "/code-review" as
# a background task. Its id is NOT in this text at all; it is a structured
# field, toolUseResult.agentId, on the SAME transcript entry (see
# ThreadWaitState.apply_user_entry) — found by inspecting a real instance's
# raw JSON, since the text alone gives no id to capture.
BACKGROUND_SKILL_START_RE = re.compile(r"launched \(forked execution, running in the background\)")

# Open teammate waits: a SendMessage tool_use (matched structurally, on the
# tool name and its JSON `to` field — not by regex) with no later reply.
TEAMMATE_ID_FINISH_RE = re.compile(r'<teammate-message\b[^>]*\bteammate_id="([^"]+)"')
CROSS_SESSION_NAME_FINISH_RE = re.compile(r'<cross-session-message\b[^>]*\bfrom-name="([^"]+)"')

_DURATION_RE = re.compile(r"(?:(\d+)\s*h)?\s*(?:(\d+)\s*m)?\s*(?:(\d+)\s*s)?")


def parse_duration_seconds(text: str) -> float:
    """Parse "1m 30s", "5m", "15s", "1h" (as seen in real Monitor-started
    text) into seconds. Unparseable text yields 0 (no expiry benefit, fails
    open rather than crashing)."""
    m = _DURATION_RE.match(text.strip())
    if not m:
        return 0.0
    h, mi, s = (int(g) if g else 0 for g in m.groups())
    return float(h * 3600 + mi * 60 + s)


def parse_monitor_expiry_seconds(detail: str) -> float | None:
    """From a Monitor's detail clause (the text between the task id and the
    closing paren), the seconds until it expires on its own — or None for a
    "persistent" Monitor, which only closes via TaskStop or a finish signal.
    """
    m = MONITOR_EXPIRES_IN_RE.search(detail)
    if m:
        return parse_duration_seconds(m.group(1))
    m = MONITOR_TIMEOUT_MS_RE.search(detail)
    if m:
        return int(m.group(1)) / 1000.0
    return None  # "persistent", or an unrecognized detail — fail open, stay open


def entry_tool_result_texts(entry: dict) -> list[str]:
    """Every tool_result block's own text in a 'user' entry. This is where
    background-task START markers live (the START ones above) — unlike the
    FINISH markers, which arrive as plain user/isMeta text, not a tool result.
    """
    message = entry.get("message") or {}
    content = message.get("content")
    if not isinstance(content, list):
        return []
    out = []
    for block in content:
        if not isinstance(block, dict) or block.get("type") != "tool_result":
            continue
        inner = block.get("content")
        if isinstance(inner, str):
            out.append(inner)
        elif isinstance(inner, list):
            for b in inner:
                if isinstance(b, dict) and b.get("type") == "text":
                    out.append(b.get("text", ""))
    return out


class ThreadWaitState:
    """Tracks, as a thread's entries are scanned in order, which background
    tasks and teammate waits are currently open. Queried at the moment a gap
    STARTS, to classify `waiting_for`.
    """

    def __init__(self) -> None:
        self.open_tasks: dict[str, float | None] = {}  # task_id -> expiry epoch secs, or None
        self.open_teammates: dict[str, float] = {}  # name -> ts the SendMessage opened it
        # (name, open_ts, close_ts) for every teammate wait that CLOSED (a
        # reply arrived) — see "report how long open teammate waits stay
        # open" (PR #425 review). A wait still open when the thread's own
        # transcript ends is read from `open_teammates` by the caller, which
        # knows the thread's last call ts and this class does not.
        self.teammate_wait_closed: list[tuple[str, float, float]] = []

    def apply_tool_use(self, name: str, tool_input: dict, ts: float | None) -> None:
        if name == "SendMessage":
            to = str(tool_input.get("to", "")).split(" [")[0].strip()
            if to and ts is not None:
                self.open_teammates[to] = ts
        elif name == "TaskStop":
            task_id = tool_input.get("task_id") or tool_input.get("shell_id")
            if task_id:
                self.open_tasks.pop(task_id, None)

    def _close_teammate(self, name: str, ts: float | None) -> None:
        opened_ts = self.open_teammates.pop(name, None)
        if opened_ts is not None and ts is not None:
            self.teammate_wait_closed.append((name, opened_ts, ts))

    def apply_user_entry(self, entry: dict) -> None:
        text, _ = entry_user_text(entry)
        ts = iso_to_epoch(entry.get("timestamp", ""))
        for block_m in TASK_NOTIFICATION_BLOCK_RE.finditer(text):
            block = block_m.group(1)
            id_m = TASK_ID_IN_BLOCK_RE.search(block)
            if id_m and task_notification_finishes(block):
                self.open_tasks.pop(id_m.group(1), None)
        for m in AGENT_MESSAGE_FINISH_RE.finditer(text):
            self.open_tasks.pop(m.group(1), None)
        for m in TEAMMATE_ID_FINISH_RE.finditer(text):
            self._close_teammate(m.group(1), ts)
        for m in CROSS_SESSION_NAME_FINISH_RE.finditer(text):
            self._close_teammate(m.group(1), ts)

        for result_text in entry_tool_result_texts(entry):
            for m in ASYNC_AGENT_START_RE.finditer(result_text):
                self.open_tasks[m.group(1)] = None
            for m in BG_BASH_START_RE.finditer(result_text):
                self.open_tasks[m.group(1)] = None
            for m in MONITOR_START_RE.finditer(result_text):
                task_id, detail = m.group(1), m.group(2)
                seconds = parse_monitor_expiry_seconds(detail)
                self.open_tasks[task_id] = (ts + seconds) if (ts is not None and seconds is not None) else None
            if BACKGROUND_SKILL_START_RE.search(result_text):
                # The id lives on the entry's own toolUseResult.agentId, not
                # in this text — see BACKGROUND_SKILL_START_RE's docstring.
                agent_id = (entry.get("toolUseResult") or {}).get("agentId")
                if agent_id:
                    self.open_tasks[agent_id] = None

    def prune_expired(self, now_ts: float) -> None:
        expired = [k for k, v in self.open_tasks.items() if v is not None and v <= now_ts]
        for k in expired:
            del self.open_tasks[k]

    def classify_waiting_for(self, call: "Call") -> str:
        if call.is_error:
            return "retry"
        if call.tool_use_names:
            if any(n in ASK_HUMAN_TOOLS for n in call.tool_use_names):
                return "human"
            return "own_tool"
        self.prune_expired(call.ts)
        if self.open_tasks:
            return "background"
        if self.open_teammates:
            return "teammate"
        return "human"


@dataclass
class Call:
    """One API call on a thread, as recorded in the transcript.

    An API response can be split across several transcript entries sharing
    the same message_id (a thinking block in one entry, a tool_use block in
    the next). `tool_use_names` and `stop_reason` are merged across all of
    them — this matters for `waiting_for` (see classify_waiting_for), which
    needs to know whether the response that STARTS a gap called a tool.
    """

    ts: float  # epoch seconds (response-arrival time; see README caveat)
    message_id: str
    model: str
    is_error: bool
    input_tokens: int = 0
    cache_read: int = 0
    cache_creation: int = 0
    output_tokens: int = 0
    stop_reason: str = ""
    tool_use_names: tuple[str, ...] = ()

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
    cause: str  # wake_cause: one of WAKE_CAUSES, known once the gap ENDS
    is_cold_start: bool = False  # end.start is the thread's first call
    waiting_for: str = ""  # one of WAITING_FOR_VALUES, known once the gap STARTS


@dataclass
class ToolWait:
    """The wait after ONE call whose response's LAST tool_use block named
    `tool` — issue #424 section 4's per-tool learning. Unlike Gap, this is
    recorded for EVERY consecutive call pair, with no GAP_THRESHOLD_MIN floor
    ("every gap including the short ones"), because the distribution's own
    shape (how many waits are short vs. long) is part of what is learned.

    `fallback_group` is the waiting_for bucket this call would fall into if
    the tool itself turns out to have too little data: "human" for
    AskUserQuestion/ExitPlanMode (issue #424's named exception), "own_tool"
    for every other tool — this needs no thread-state lookup, since a
    tool_use always wins classify_waiting_for's own_tool/human check before
    it ever looks at open tasks/teammates.
    """

    project: str
    thread_kind: str
    thread_id: str
    tool: str
    gap_min: float
    start: Call
    end: Call
    fallback_group: str  # "human" or "own_tool"


@dataclass
class TeammateWait:
    """How long one open-teammate wait (a SendMessage to `name`) stayed open.
    PR #425 review: "check that the open-teammate rule does not keep a wait
    open forever ... report how long open teammate waits stay open."

    `closed` is False when no reply ever arrived within this thread's own
    visible transcript — `duration_min` is then measured only up to the
    thread's LAST call, not to any real close, so it is a LOWER bound on how
    long that wait was actually open, not its true duration.
    """

    project: str
    thread_id: str
    name: str
    open_ts: float
    duration_min: float
    closed: bool


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


class _CallBuffer:
    """Accumulates the transcript entries that share one message_id (an API
    response's content can be split across several entries — a thinking
    block in one, a tool_use block in the next) into one merged Call.
    """

    def __init__(self, mid: str) -> None:
        self.mid = mid
        self.ts: float | None = None
        self.model = ""
        self.is_error = False
        self.usage: dict | None = None
        self.stop_reason = ""
        self.tool_use_names: list[str] = []

    def add(self, entry: dict, wait_state: "ThreadWaitState") -> None:
        message = entry.get("message") or {}
        if self.ts is None:
            self.ts = iso_to_epoch(entry.get("timestamp", ""))
        if entry.get("isApiErrorMessage"):
            self.is_error = True
        model = message.get("model")
        if model and not self.model:
            self.model = model
        stop_reason = message.get("stop_reason")
        if stop_reason:
            self.stop_reason = stop_reason
        usage = message.get("usage")
        if usage is not None and self.usage is None:
            self.usage = usage
        content = message.get("content")
        if isinstance(content, list):
            for block in content:
                if isinstance(block, dict) and block.get("type") == "tool_use":
                    name = block.get("name", "")
                    self.tool_use_names.append(name)
                    # Side effects (open a teammate wait / close a background
                    # task) happen the moment the tool is CALLED, same as the
                    # transcript order — not deferred to when this Call is
                    # finalized, so a later entry's finish signal in the same
                    # scan sees accurate state.
                    wait_state.apply_tool_use(name, block.get("input") or {}, self.ts)

    def finalize(self) -> Call | None:
        if self.ts is None:
            return None
        if not self.is_error and self.usage is None:
            # A content-block-only response (tool_use, thinking) with no
            # usage field ever seen: not a call boundary by itself.
            return None
        usage = self.usage or {}
        return Call(
            ts=self.ts,
            message_id=self.mid,
            model=self.model,
            is_error=self.is_error,
            input_tokens=int(usage.get("input_tokens", 0) or 0),
            cache_read=int(usage.get("cache_read_input_tokens", 0) or 0),
            cache_creation=int(usage.get("cache_creation_input_tokens", 0) or 0),
            output_tokens=int(usage.get("output_tokens", 0) or 0),
            stop_reason=self.stop_reason,
            tool_use_names=tuple(self.tool_use_names),
        )


def iter_calls_and_gaps(
    path: str, thread_id: str, thread_kind: str, project: str
) -> tuple[list[Call], list[Gap], list[ToolWait], list[TeammateWait]]:
    """Parse one thread's JSONL file into its calls, its gaps > threshold,
    its per-tool waits (every consecutive call pair, no threshold), and its
    teammate-wait durations (every SendMessage, open to close or to the
    thread's last call if no reply ever arrived)."""
    calls: list[Call] = []
    pending_between: list[dict] = []
    gaps: list[Gap] = []
    tool_waits: list[ToolWait] = []
    prev_call: Call | None = None
    prev_call_waiting_for: str = ""
    wait_state = ThreadWaitState()
    buffer: _CallBuffer | None = None

    def flush_buffer() -> None:
        nonlocal buffer, prev_call, prev_call_waiting_for, pending_between
        if buffer is None:
            return
        call = buffer.finalize()
        buffer = None
        if call is None:
            return
        # Snapshot THIS call's waiting_for right now, before any LATER entry
        # (a teammate reply, a task-notification — exactly the things that
        # END the next gap) gets a chance to mutate wait_state. waiting_for
        # must reflect what was open the moment the wait STARTS; computing it
        # lazily at the next flush (the previous shape of this function) read
        # it only after every in-between entry's side effects had already
        # landed, so a background task or teammate wait that was genuinely
        # open — and whose closing is what ends the gap — looked closed
        # before it was ever checked. See PR #425 review for 3 real examples
        # of exactly this leak.
        call_waiting_for = wait_state.classify_waiting_for(call)
        calls.append(call)

        if prev_call is not None:
            gap_min = (call.ts - prev_call.ts) / 60.0
            if prev_call.tool_use_names:
                tool = prev_call.tool_use_names[-1]
                fallback_group = "human" if tool in ASK_HUMAN_TOOLS else "own_tool"
                tool_waits.append(
                    ToolWait(
                        project=project,
                        thread_kind=thread_kind,
                        thread_id=thread_id,
                        tool=tool,
                        gap_min=gap_min,
                        start=prev_call,
                        end=call,
                        fallback_group=fallback_group,
                    )
                )
            if gap_min >= GAP_THRESHOLD_MIN:
                cause = "retry" if prev_call.is_error else classify_wake_cause(pending_between)
                # PR #425 review leak 1: AskUserQuestion/ExitPlanMode's own
                # answer MUST travel back as a tool_result block — that is
                # how the Messages API returns a tool's result, regardless
                # of who (a human) produced it. classify_wake_cause sees a
                # bare tool_result and says "tool_result" (the agent's own
                # next tool call, no new external input), which is right for
                # every OTHER tool but wrong here: a human answered. Prefer
                # the exception the SAME way waiting_for does.
                if cause == "tool_result" and any(n in ASK_HUMAN_TOOLS for n in prev_call.tool_use_names):
                    cause = "human"
                gaps.append(
                    Gap(
                        thread_id=thread_id,
                        thread_kind=thread_kind,
                        project=project,
                        start=prev_call,
                        end=call,
                        gap_min=gap_min,
                        cause=cause,
                        waiting_for=prev_call_waiting_for,
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
        prev_call_waiting_for = call_waiting_for

    try:
        fh = open(path, "r", encoding="utf-8", errors="replace")
    except OSError:
        return [], [], [], []

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
                mid = message.get("id") or entry.get("uuid")
                if buffer is not None and buffer.mid != mid:
                    flush_buffer()
                if buffer is None:
                    buffer = _CallBuffer(mid or "")
                buffer.add(entry, wait_state)
            elif etype == "user":
                flush_buffer()  # the assistant's turn is over once a user entry arrives
                wait_state.apply_user_entry(entry)
                pending_between.append(entry)
            else:
                # attachment / queue-operation / other entry types carry no
                # wake-cause or waiting_for signal, but still end any pending
                # assistant turn (defensive — none seen in practice to need it).
                flush_buffer()

        flush_buffer()

    teammate_waits: list[TeammateWait] = [
        TeammateWait(project=project, thread_id=thread_id, name=name, open_ts=open_ts, duration_min=(close_ts - open_ts) / 60.0, closed=True)
        for name, open_ts, close_ts in wait_state.teammate_wait_closed
    ]
    last_ts = calls[-1].ts if calls else None
    if last_ts is not None:
        for name, open_ts in wait_state.open_teammates.items():
            teammate_waits.append(
                TeammateWait(project=project, thread_id=thread_id, name=name, open_ts=open_ts, duration_min=(last_ts - open_ts) / 60.0, closed=False)
            )

    return calls, gaps, tool_waits, teammate_waits


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


def summarize_gaps_by_cause(
    gaps: list[Gap],
    db_index: "DBIndex | None" = None,
    prices: PriceTable | None = None,
    key_fn=lambda g: g.cause,
) -> dict:
    """Group gaps by `key_fn` (wake_cause by default; pass `lambda g: g.waiting_for`
    for the waiting_for cost table the keep-alive's budget needs — the keep-alive
    only ever knows `waiting_for`, since that's set when the wait STARTS, not
    `wake_cause`, which is only known once it ends).
    """
    prices = prices or PriceTable()
    by_cause: dict[str, list[Gap]] = defaultdict(list)
    for g in gaps:
        if g.is_cold_start:
            continue
        by_cause[key_fn(g)].append(g)

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


def waiting_for_x_wake_cause(gaps: list[Gap]) -> dict:
    """Cross-tab: rows = waiting_for (known at the START of the wait), columns
    = wake_cause (known at the END) — this is how well waiting_for predicts
    wake_cause, per issue #424's own table. {waiting_for: {wake_cause: count}},
    plus {waiting_for: {"total":, "median_gap_min":}}.
    """
    real = [g for g in gaps if not g.is_cold_start]
    table: dict[str, dict[str, int]] = defaultdict(lambda: defaultdict(int))
    gap_minutes_by_wf: dict[str, list[float]] = defaultdict(list)
    for g in real:
        table[g.waiting_for][g.cause] += 1
        gap_minutes_by_wf[g.waiting_for].append(g.gap_min)

    out = {}
    for wf in sorted(table):
        row = dict(table[wf])
        out[wf] = {
            "by_wake_cause": row,
            "total": sum(row.values()),
            "median_gap_min": round(median(gap_minutes_by_wf[wf]), 2) if gap_minutes_by_wf[wf] else 0.0,
        }
    return out


def own_tool_breakdown(gaps: list[Gap]) -> dict[str, int]:
    """How many own_tool gaps started with each tool name, keyed on the
    response's LAST tool_use block — the same rule ToolWait uses (see its
    docstring), and deliberately so: this must reconcile exactly with
    summarize_tool_waits()'s `waits_ge_threshold` count for the same tool
    (both come from the same gaps, both key on the last tool_use, both use
    GAP_THRESHOLD_MIN — the earlier version counted EVERY tool_use block in
    a multi-tool response, which could double-count a response that called
    2 tools and never matched the per-tool table's own count).
    """
    counts: dict[str, int] = defaultdict(int)
    for g in gaps:
        if g.is_cold_start or g.waiting_for != "own_tool":
            continue
        if g.start.tool_use_names:
            counts[g.start.tool_use_names[-1]] += 1
    return dict(sorted(counts.items(), key=lambda kv: -kv[1]))


# ---------------------------------------------------------------------------
# Issue #424 section 4: learn the wait per tool, with a generic fallback.
# ---------------------------------------------------------------------------

MIN_TOOL_WAITS_FOR_OWN_BUDGET = 20  # issue #424's own suggested threshold
MAX_PING_BUDGET = 11  # issue #424: "never go above the break-even point"
DEFAULT_PING_BUDGET = 2  # today's fixed policy
# PR #425 review: a policy-comparison test set below this is too small to
# call any policy a win or a tie on — say so rather than letting a 14-gap
# test set (forever) read the same as a 445-gap one (context-guru).
MIN_MEANINGFUL_TEST_GAPS = 50


def best_n_for_gaps(gaps_min: list[float]) -> int:
    """The N (0..12) with the lowest average sim_cost_x over these gaps."""
    if not gaps_min:
        return DEFAULT_PING_BUDGET
    sim = {n: sum(sim_cost_x(g, n) for g in gaps_min) / len(gaps_min) for n in PING_BUDGETS}
    return min(sim, key=lambda n: sim[n])


def tool_call_counts(calls_by_thread: dict[str, list[Call]]) -> dict[str, int]:
    """Number of calls to each tool, counting EVERY tool_use block in EVERY
    response (not just the last one per response, unlike ToolWait) — issue
    #424's "number of calls" stat, a plain usage-frequency count.
    """
    counts: dict[str, int] = defaultdict(int)
    for calls in calls_by_thread.values():
        for call in calls:
            for name in call.tool_use_names:
                counts[name] += 1
    return dict(counts)


def summarize_tool_waits(tool_waits: list[ToolWait], call_counts: dict[str, int]) -> dict:
    """Per tool, sorted by call count.

    PR #425 review: the keep-alive only ever acts after GAP_THRESHOLD_MIN
    (4.67 min) of idle time, so the PRIMARY stats here are over waits of
    that length or more — count (`waits_ge_threshold`), median/p75/p90
    (the `_ge_threshold` fields), and hits/misses among them (the hit rule
    is the same `Call.is_cache_hit` every other table uses). The all-wait
    median and the share over 5 min (`median_wait_min_all_waits`,
    `share_over_5min_all_waits`) are kept as SECONDARY columns — almost
    every tool resolves in well under a minute, so they mostly just show
    how rare a long wait is for that tool. `own_tool_breakdown` (reported
    alongside this table) counts gaps the same way — the response's LAST
    tool_use, gaps at or above the threshold — specifically so the two
    reconcile: `own_tool_breakdown[tool]` must equal
    `waits_ge_threshold` for every tool except `AskUserQuestion`/
    `ExitPlanMode` (which `own_tool_breakdown` excludes, since those wait
    on the human, not the agent's own tool — see `waiting_for`).

    `best_n` is `"n/a"` when a tool has fewer than
    MIN_TOOL_WAITS_FOR_OWN_BUDGET training waits at or above the
    threshold — the same rule the hierarchical policy itself uses to
    decide whether a tool has "enough" data (see `policy_n`); a best N
    computed from 0 or 1 waits is not a real signal.
    """
    by_tool: dict[str, list[ToolWait]] = defaultdict(list)
    for tw in tool_waits:
        by_tool[tw.tool].append(tw)

    out = {}
    for tool, tws in by_tool.items():
        all_waits = [tw.gap_min for tw in tws]
        long = [tw for tw in tws if tw.gap_min >= GAP_THRESHOLD_MIN]
        long_waits = [tw.gap_min for tw in long]
        hits = sum(1 for tw in long if tw.end.is_cache_hit)
        misses = sum(1 for tw in long if tw.end.is_cache_miss)
        out[tool] = {
            "call_count": call_counts.get(tool, len(tws)),
            "waits_ge_threshold": len(long),
            "median_wait_min": round(median(long_waits), 2) if long_waits else 0.0,
            "p75_wait_min": round(pct(long_waits, 0.75), 2) if long_waits else 0.0,
            "p90_wait_min": round(pct(long_waits, 0.90), 2) if long_waits else 0.0,
            "hits": hits,
            "misses": misses,
            "best_n": best_n_for_gaps(long_waits) if len(long) >= MIN_TOOL_WAITS_FOR_OWN_BUDGET else "n/a",
            "wait_count_all": len(tws),
            "median_wait_min_all_waits": round(median(all_waits), 2) if all_waits else 0.0,
            "share_over_5min_all_waits": round(sum(1 for g in all_waits if g > 5.0) / len(all_waits), 3) if all_waits else 0.0,
        }
    return dict(sorted(out.items(), key=lambda kv: -kv[1]["call_count"]))


def split_sessions_by_time(calls_by_thread: dict[str, list[Call]]) -> tuple[set[str], set[str]]:
    """Order a project's SESSIONS (not threads) by their first call's
    timestamp, and split into the first half (train) and second half
    (test) — issue #424's own instruction, so the policy comparison
    measures generalization rather than fit.
    """
    session_first_ts: dict[str, float] = {}
    for thread_id, calls in calls_by_thread.items():
        if not calls:
            continue
        session_id = thread_id.split("/", 1)[0]
        ts = calls[0].ts
        if session_id not in session_first_ts or ts < session_first_ts[session_id]:
            session_first_ts[session_id] = ts
    ordered = sorted(session_first_ts, key=lambda sid: session_first_ts[sid])
    cutoff = (len(ordered) + 1) // 2  # train gets the extra session on an odd split
    return set(ordered[:cutoff]), set(ordered[cutoff:])


def policy_n(
    gap: Gap,
    tool_best_n: dict[str, tuple[int, int]],  # tool -> (best_n, train_wait_count)
    group_best_n: dict[str, int],
) -> int:
    """The hierarchical policy from issue #424 section 4: a known special
    case uses the human budget; else the tool's own budget if it has
    enough training data; else its waiting_for group's budget; else the
    default. Capped at MAX_PING_BUDGET throughout.
    """
    tool = gap.start.tool_use_names[-1] if gap.start.tool_use_names else None
    if tool in ASK_HUMAN_TOOLS:
        n = group_best_n.get("human", DEFAULT_PING_BUDGET)
    elif tool is not None:
        best_n, train_count = tool_best_n.get(tool, (DEFAULT_PING_BUDGET, 0))
        n = best_n if train_count >= MIN_TOOL_WAITS_FOR_OWN_BUDGET else group_best_n.get("own_tool", DEFAULT_PING_BUDGET)
    else:
        n = group_best_n.get(gap.waiting_for, DEFAULT_PING_BUDGET)
    return min(n, MAX_PING_BUDGET)


def simulate_policies(
    train_gaps: list[Gap], test_gaps: list[Gap], train_tool_waits: list[ToolWait]
) -> dict:
    """Learn on train_gaps/train_tool_waits, evaluate 4 policies' average
    cost per gap on test_gaps: the hierarchical policy, a single global
    best N, the waiting_for-only policy (no per-tool step), and today's
    fixed 2 pings. All capped at MAX_PING_BUDGET.
    """
    group_best_n = {
        wf: best_n_for_gaps([g.gap_min for g in train_gaps if g.waiting_for == wf])
        for wf in WAITING_FOR_VALUES
    }

    by_tool: dict[str, list[float]] = defaultdict(list)
    for tw in train_tool_waits:
        if tw.gap_min >= GAP_THRESHOLD_MIN:
            by_tool[tw.tool].append(tw.gap_min)
    tool_best_n = {tool: (best_n_for_gaps(gaps_min), len(gaps_min)) for tool, gaps_min in by_tool.items()}

    global_n = min(MAX_PING_BUDGET, best_n_for_gaps([g.gap_min for g in train_gaps]))

    def avg_cost(n_for_gap) -> float:
        if not test_gaps:
            return 0.0
        return sum(sim_cost_x(g.gap_min, n_for_gap(g)) for g in test_gaps) / len(test_gaps)

    return {
        "train_gap_count": len(train_gaps),
        "test_gap_count": len(test_gaps),
        "hierarchical_cost_x_per_gap": round(avg_cost(lambda g: policy_n(g, tool_best_n, group_best_n)), 3),
        "global_best_n": global_n,
        "global_best_n_cost_x_per_gap": round(avg_cost(lambda g: global_n), 3),
        "waiting_for_only_cost_x_per_gap": round(
            avg_cost(lambda g: min(MAX_PING_BUDGET, group_best_n.get(g.waiting_for, DEFAULT_PING_BUDGET))), 3
        ),
        "fixed_2_pings_cost_x_per_gap": round(avg_cost(lambda g: DEFAULT_PING_BUDGET), 3),
    }


def random_session_split(session_ids: list[str], seed: int) -> tuple[set[str], set[str]]:
    """A RANDOM (not time-ordered) half/half session split, for the repeated
    robustness check only — the headline train/test split stays time-ordered
    (issue #424's own instruction), but one split alone cannot show whether
    a result is a property of the method or an artifact of that one split's
    particular train/test boundary (see PR #425 review).
    """
    import random

    ids = sorted(session_ids)
    random.Random(seed).shuffle(ids)
    cutoff = (len(ids) + 1) // 2
    return set(ids[:cutoff]), set(ids[cutoff:])


def repeated_split_policy_comparison(
    all_gaps: list[Gap], all_tool_waits: list[ToolWait], calls_by_thread: dict[str, list[Call]], n_splits: int = 5
) -> dict:
    """Run simulate_policies on N random session-level splits and report
    each policy's mean cost and its spread (min/max across splits) — PR
    #425 review: "so the result is not judged on one split."
    """

    def session_of(thread_id: str) -> str:
        return thread_id.split("/", 1)[0]

    session_ids = sorted({session_of(tid) for tid, calls in calls_by_thread.items() if calls})
    real_gaps = [g for g in all_gaps if not g.is_cold_start]

    per_split = []
    for seed in range(n_splits):
        train_s, test_s = random_session_split(session_ids, seed)
        train_gaps = [g for g in real_gaps if session_of(g.thread_id) in train_s]
        test_gaps = [g for g in real_gaps if session_of(g.thread_id) in test_s]
        train_tw = [tw for tw in all_tool_waits if session_of(tw.thread_id) in train_s]
        per_split.append(simulate_policies(train_gaps, test_gaps, train_tw))

    def agg(key: str) -> dict:
        vals = [r[key] for r in per_split]
        return {"mean": round(sum(vals) / len(vals), 3), "min": round(min(vals), 3), "max": round(max(vals), 3), "values": vals}

    return {
        "n_splits": n_splits,
        "hierarchical": agg("hierarchical_cost_x_per_gap"),
        "global_best_n": agg("global_best_n_cost_x_per_gap"),
        "waiting_for_only": agg("waiting_for_only_cost_x_per_gap"),
        "fixed_2_pings": agg("fixed_2_pings_cost_x_per_gap"),
    }


# ---------------------------------------------------------------------------
# Issue #424 section 4: unused tools (declared but never called).
# ---------------------------------------------------------------------------


def read_tool_tables(db_path: str, session_ids: set[str]) -> tuple[set[str], set[str]]:
    """(declared built-in tool names, called tool names) for these sessions,
    from the DB's tool_declarations (kind='tool') and tool_uses tables —
    the DB stores exactly this, which a transcript does not (see
    unused_tools_report).
    """
    if not session_ids:
        return set(), set()
    uri = f"file:{db_path}?mode=ro"
    con = sqlite3.connect(uri, uri=True)
    try:
        placeholders = ",".join("?" for _ in session_ids)
        cur = con.cursor()
        cur.execute(
            f"select distinct name from tool_declarations where kind='tool' and session_id in ({placeholders})",
            tuple(session_ids),
        )
        declared = {r[0] for r in cur.fetchall()}
        cur.execute(
            f"select distinct name from tool_uses where calls > 0 and session_id in ({placeholders})",
            tuple(session_ids),
        )
        called = {r[0] for r in cur.fetchall()}
    finally:
        con.close()
    return declared, called


def summarize_teammate_wait_durations(teammate_waits: list[TeammateWait]) -> dict:
    """How long open-teammate waits stay open — PR #425 review: "check that
    the open-teammate rule does not keep a wait open forever after a
    SendMessage that never gets a reply."

    `never_closed` waits are a LOWER bound (duration_min is measured only to
    the thread's last call, not a real close — see TeammateWait), so they
    are reported separately from closed ones rather than pooled with them.
    """
    closed = [tw.duration_min for tw in teammate_waits if tw.closed]
    never_closed = [tw.duration_min for tw in teammate_waits if not tw.closed]
    return {
        "closed_count": len(closed),
        "closed_median_min": round(median(closed), 2) if closed else 0.0,
        "closed_p90_min": round(pct(closed, 0.90), 2) if closed else 0.0,
        "closed_max_min": round(max(closed), 2) if closed else 0.0,
        "never_closed_count": len(never_closed),
        "never_closed_median_min_lower_bound": round(median(never_closed), 2) if never_closed else 0.0,
        "never_closed_max_min_lower_bound": round(max(never_closed), 2) if never_closed else 0.0,
    }


def unused_tools_report(session_ids: set[str], db_paths: list[str], called_from_transcripts: set[str]) -> dict:
    """Declared-but-never-called built-in tools for a project's sessions.

    Needs the DB's tool_declarations table: a transcript carries no `tools`
    array of its own (checked: Claude Code session JSONL records the
    rendered conversation, not the raw request body a declared-tools list
    would live in), so without a DB this can only report which tools WERE
    called, not which were declared and skipped.
    """
    if not db_paths:
        return {
            "source": "transcripts_only",
            "note": "No --db given, and transcripts carry no declared-tools list, so unused tools cannot be found here — only the tools that were called.",
            "called": sorted(called_from_transcripts),
        }
    declared: set[str] = set()
    called: set[str] = set()
    for db_path in db_paths:
        try:
            d, c = read_tool_tables(db_path, session_ids)
        except Exception:
            continue  # fail open: a DB that doesn't cover this project just contributes nothing
        declared |= d
        called |= c
    if not declared:
        return {
            "source": "db_tool_declarations",
            "note": "The DB covers none of this project's sessions (or has no tool_declarations rows for them) — only the tools that were called, from the transcripts.",
            "called": sorted(called_from_transcripts),
        }
    # The DB's own tool_uses table can under-cover a project (it only has
    # rows for sessions it captured, which may be a small subset of this
    # project's transcripts) — a tool the transcripts show as CALLED must
    # never be reported "unused" just because this particular DB missed it.
    called |= called_from_transcripts
    return {
        "source": "db_tool_declarations",
        "declared_count": len(declared),
        "called_count": len(called),
        "unused": sorted(declared - called),
    }


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
    per_project_tool_waits: dict[str, list[ToolWait]] = defaultdict(list)
    per_project_teammate_waits: dict[str, list[TeammateWait]] = defaultdict(list)

    for project, thread_id, thread_kind, path in threads:
        calls, gaps, tool_waits, teammate_waits = iter_calls_and_gaps(path, thread_id, thread_kind, project)
        per_project_gaps[project].extend(gaps)
        per_project_calls[project][thread_id] = calls
        per_project_tool_waits[project].extend(tool_waits)
        per_project_teammate_waits[project].extend(teammate_waits)
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
                "by_waiting_for": summarize_gaps_by_cause(kind_gaps, db_index, prices, key_fn=lambda g: g.waiting_for),
                "waiting_for_x_wake_cause": waiting_for_x_wake_cause(kind_gaps),
                "own_tool_breakdown": own_tool_breakdown(kind_gaps),
            }

        # --- Issue #424 section 4: per-tool stats (main and team_thread only,
        # per spec), the hierarchical policy comparison, and unused tools. ---
        project_tool_waits = per_project_tool_waits.get(project, [])
        project_calls = per_project_calls.get(project, {})
        thread_kind_of: dict[str, str] = {tid: tk for p, tid, tk, _ in threads if p == project}

        report["projects"][project]["tool_waits"] = {}
        for kind in ("main", "team_thread"):
            kind_tool_waits = [tw for tw in project_tool_waits if tw.thread_kind == kind]
            kind_calls = {tid: c for tid, c in project_calls.items() if thread_kind_of.get(tid) == kind}
            report["projects"][project]["tool_waits"][kind] = summarize_tool_waits(
                kind_tool_waits, tool_call_counts(kind_calls)
            )

        train_sessions, test_sessions = split_sessions_by_time(project_calls)

        def _session_of(tid: str) -> str:
            return tid.split("/", 1)[0]

        train_gaps = [g for g in gaps if _session_of(g.thread_id) in train_sessions and not g.is_cold_start]
        test_gaps = [g for g in gaps if _session_of(g.thread_id) in test_sessions and not g.is_cold_start]
        train_tool_waits = [tw for tw in project_tool_waits if _session_of(tw.thread_id) in train_sessions]
        report["projects"][project]["policy_comparison"] = simulate_policies(train_gaps, test_gaps, train_tool_waits)
        report["projects"][project]["policy_comparison"]["train_sessions"] = len(train_sessions)
        report["projects"][project]["policy_comparison"]["test_sessions"] = len(test_sessions)
        report["projects"][project]["repeated_split_policy_comparison"] = repeated_split_policy_comparison(
            gaps, project_tool_waits, project_calls
        )

        called_from_transcripts = {name for calls in project_calls.values() for c in calls for name in c.tool_use_names}
        all_session_ids = train_sessions | test_sessions
        report["projects"][project]["unused_tools"] = unused_tools_report(all_session_ids, db_paths, called_from_transcripts)
        report["projects"][project]["teammate_wait_durations"] = summarize_teammate_wait_durations(
            per_project_teammate_waits.get(project, [])
        )

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

            cross = kd.get("waiting_for_x_wake_cause") or {}
            if cross:
                lines.append(f"\n  {label}: waiting_for x wake_cause (what the keep-alive would know vs. what actually ended it)")
                wake_cols = sorted({wc for row in cross.values() for wc in row["by_wake_cause"]})
                lines.append("    waiting_for".ljust(22) + "".join(c.ljust(14) for c in wake_cols) + "total   median_gap")
                for wf, row in cross.items():
                    counts = "".join(str(row["by_wake_cause"].get(c, 0)).ljust(14) for c in wake_cols)
                    lines.append(f"    {wf}".ljust(22) + counts + f"{row['total']}".ljust(8) + f"{row['median_gap_min']}min")

            wf_stats = kd.get("by_waiting_for") or {}
            if wf_stats:
                lines.append(f"\n  {label}: cost by waiting_for (this is what the keep-alive's ping budget would be set from)")
                for wf, stats in wf_stats.items():
                    dist = stats["gap_distribution"]
                    lines.append(
                        f"    {wf}: {dist['count']} gaps (hits={stats['hits']} misses={stats['misses']} "
                        f"errors={stats['errors']}), median {dist['median_min']}min | "
                        f"observed {stats['observed_cost_x_per_gap']}x/gap (${stats['observed_cost_usd_total']}) | "
                        f"best N={stats['best_n']} -> {stats['best_n_cost_x_per_gap']}x/gap"
                    )

            tool_breakdown = kd.get("own_tool_breakdown") or {}
            if tool_breakdown:
                lines.append(f"  {label}: own_tool by tool name: " + ", ".join(f"{name}={n}" for name, n in tool_breakdown.items()))

        tool_waits = data.get("tool_waits") or {}
        for kind in ("main", "team_thread"):
            tool_stats = tool_waits.get(kind) or {}
            if not tool_stats:
                continue
            label = "team_thread" if kind == "team_thread" else kind
            lines.append(
                f"\n  {label}: per-tool wait stats (sorted by call count). PRIMARY columns are "
                f"waits >= {GAP_THRESHOLD_MIN}min (the keep-alive only ever acts after that much idle "
                f"time); 'all waits' columns are secondary, over every wait of any length."
            )
            for tool, stats in tool_stats.items():
                lines.append(
                    f"    {tool}: calls={stats['call_count']} | "
                    f"waits>=thresh={stats['waits_ge_threshold']} (hits={stats['hits']} misses={stats['misses']}) "
                    f"median={stats['median_wait_min']}min p75={stats['p75_wait_min']}min p90={stats['p90_wait_min']}min "
                    f"best N={stats['best_n']} | "
                    f"all waits: {stats['wait_count_all']}, median={stats['median_wait_min_all_waits']}min, "
                    f">5min={stats['share_over_5min_all_waits']*100:.0f}%"
                )

        pc = data.get("policy_comparison")
        if pc:
            lines.append(
                f"\n  Policy comparison (train {pc['train_sessions']} sessions / {pc['train_gap_count']} gaps, "
                f"test {pc['test_sessions']} sessions / {pc['test_gap_count']} gaps):"
            )
            if pc["test_gap_count"] < MIN_MEANINGFUL_TEST_GAPS:
                lines.append(
                    f"    CAVEAT: only {pc['test_gap_count']} test gaps — too few to call any policy a win or a "
                    f"tie here; see the repeated-split check below instead."
                )
            lines.append(f"    hierarchical policy: {pc['hierarchical_cost_x_per_gap']}x/gap")
            lines.append(f"    single global best N ({pc['global_best_n']}): {pc['global_best_n_cost_x_per_gap']}x/gap")
            lines.append(f"    waiting_for-only policy: {pc['waiting_for_only_cost_x_per_gap']}x/gap")
            lines.append(f"    today's fixed 2 pings: {pc['fixed_2_pings_cost_x_per_gap']}x/gap")

        rsc = data.get("repeated_split_policy_comparison")
        if rsc:
            lines.append(f"\n  Repeated random session-split check ({rsc['n_splits']} splits, mean [min, max] x/gap):")
            for label, key in (("hierarchical", "hierarchical"), ("global best N", "global_best_n"), ("waiting_for-only", "waiting_for_only"), ("fixed 2 pings", "fixed_2_pings")):
                r = rsc[key]
                lines.append(f"    {label}: {r['mean']} [{r['min']}, {r['max']}] (values: {r['values']})")

        ut = data.get("unused_tools")
        if ut:
            if ut["source"] == "db_tool_declarations" and "unused" in ut:
                lines.append(
                    f"\n  Unused tools ({ut['declared_count']} declared, {ut['called_count']} called): "
                    + (", ".join(ut["unused"]) if ut["unused"] else "(none)")
                )
            else:
                lines.append(f"\n  Unused tools: {ut['note']}")
                lines.append(f"    called: " + ", ".join(ut.get("called", [])))

        tw = data.get("teammate_wait_durations")
        if tw:
            lines.append(
                f"\n  Teammate wait durations: {tw['closed_count']} closed "
                f"(median {tw['closed_median_min']}min, p90 {tw['closed_p90_min']}min, max {tw['closed_max_min']}min); "
                f"{tw['never_closed_count']} never closed within the visible transcript "
                f"(median {tw['never_closed_median_min_lower_bound']}min, max {tw['never_closed_max_min_lower_bound']}min, "
                f"lower bound — measured only to the thread's last call)"
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


def _write_call_line(
    fh,
    ts_iso: str,
    message_id: str,
    model: str,
    cache_read: int,
    cache_creation: int,
    is_error: bool = False,
    tool_uses: list[tuple[str, dict]] | None = None,
) -> None:
    content = [{"type": "tool_use", "name": name, "input": inp, "id": f"toolu_{message_id}_{i}"} for i, (name, inp) in enumerate(tool_uses or [])]
    message: dict = {
        "id": message_id,
        "model": model,
        "stop_reason": "tool_use" if tool_uses else "end_turn",
        "usage": {
            "input_tokens": 10,
            "cache_read_input_tokens": cache_read,
            "cache_creation_input_tokens": cache_creation,
            "output_tokens": 50,
        },
    }
    if content:
        message["content"] = content
    entry = {"type": "assistant", "timestamp": ts_iso, "message": message}
    if is_error:
        entry["isApiErrorMessage"] = True
    fh.write(json.dumps(entry) + "\n")


def _write_tool_result_line(fh, ts_iso: str, text: str, tool_use_result: dict | None = None) -> None:
    """A 'user' entry carrying one tool_result whose own text is `text` — this
    is where background-task START markers live (Monitor started, async agent
    launched, running in background with ID). `tool_use_result`, when given,
    is the entry's top-level `toolUseResult` field — a background Skill
    launch's own id lives there, not in the text (see BACKGROUND_SKILL_START_RE)."""
    entry = {
        "type": "user",
        "timestamp": ts_iso,
        "message": {"content": [{"type": "tool_result", "content": [{"type": "text", "text": text}]}]},
    }
    if tool_use_result is not None:
        entry["toolUseResult"] = tool_use_result
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
            _, gaps, _, _ = iter_calls_and_gaps(path, thread_id, thread_kind, project)
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

        # --- waiting_for: one gap per value (own_tool, background, teammate,
        # human-via-AskUserQuestion), in a dedicated session. retry is already
        # covered above (main_gaps' "retry" gap doubles as a waiting_for=retry
        # case, since a failed call means retry for both fields identically).
        wf_session_id = "waiting-for-session"
        wf_path = os.path.join(tmp, f"{wf_session_id}.jsonl")
        with open(wf_path, "w") as fh:
            # W1: tool_use=Bash. The gap W1->W2 is classified by W1 -> own_tool.
            _write_call_line(fh, iso(0), "w1", "claude-opus-5-5", cache_read=0, cache_creation=500, tool_uses=[("Bash", {"command": "ls"})])
            _write_tool_result_line(fh, iso(1), "total 0")
            # W2: tool_use=Monitor. Its tool_result OPENS task "wftask1", expiring
            # 15 min after it opens (absolute expiry ~= iso(301) + 900 = iso(1201)):
            # open for W3's check below (iso(600)) but expired well before W6's
            # (iso(1500)), so it does not mask the teammate check at W6. The gap
            # W1->W2 is classified by W1 (own_tool, above); the gap W2->W3 is
            # classified by W2 (own_tool too, Monitor is a tool).
            _write_call_line(fh, iso(300), "w2", "claude-opus-5-5", cache_read=400, cache_creation=50, tool_uses=[("Monitor", {"command": "tail -f x"})])
            _write_tool_result_line(fh, iso(301), 'Monitor started (task wftask1, expires in 15m unless the source ends first; you get one notice at expiry). You will be notified on each event.')
            # W3: no tool_use (end_turn). wftask1 is still open (under its 15-min
            # expiry) -> the gap W3->W4 is classified by W3 -> background.
            _write_call_line(fh, iso(600), "w3", "claude-opus-5-5", cache_read=400, cache_creation=50)
            # W4: tool_use=AskUserQuestion -> the exception: waits on the human,
            # not "own_tool". The gap W4->W5 is classified by W4 -> human.
            _write_call_line(fh, iso(900), "w4", "claude-opus-5-5", cache_read=400, cache_creation=50, tool_uses=[("AskUserQuestion", {"questions": []})])
            _write_tool_result_line(fh, iso(901), "answer: yes")
            # W5: tool_use=SendMessage to "bob". OPENS a teammate wait for "bob"
            # (no reply ever arrives in this fixture). The gap W4->W5 is
            # classified by W4 (human, above); the gap W5->W6 is classified by
            # W5 (own_tool, SendMessage is a tool).
            _write_call_line(fh, iso(1200), "w5", "claude-opus-5-5", cache_read=400, cache_creation=50, tool_uses=[("SendMessage", {"to": "bob", "message": "hi"})])
            _write_tool_result_line(fh, iso(1201), "Message sent to bob's inbox")
            # W6: no tool_use (end_turn). "bob" is still an open teammate wait
            # (no <teammate-message teammate_id="bob"> ever arrived) -> the gap
            # W6->W7 is classified by W6 -> teammate.
            _write_call_line(fh, iso(1500), "w6", "claude-opus-5-5", cache_read=400, cache_creation=50)
            _write_call_line(fh, iso(1800), "w7", "claude-opus-5-5", cache_read=400, cache_creation=50)

        wf_threads = find_threads([tmp])
        wf_path_found = [p for proj, tid, kind, p in wf_threads if tid == wf_session_id]
        check("waiting-for-session found as its own thread", len(wf_path_found), 1)
        wf_calls, wf_gaps, wf_tool_waits, wf_teammate_waits = iter_calls_and_gaps(wf_path, wf_session_id, "main", "wf-project")
        wf_gaps = [g for g in wf_gaps if not g.is_cold_start]
        # There is one gap per consecutive call pair (6 calls -> 5 gaps + 1 cold
        # start taken separately below); index them by which call starts them.
        gaps_by_start_mid = {g.start.message_id: g for g in wf_gaps}
        check("waiting-for-session gap count", len(wf_gaps), 6)
        check("W1 (tool_use=Bash) -> own_tool", gaps_by_start_mid["w1"].waiting_for, "own_tool")
        check("W3 (end_turn, Monitor task open) -> background", gaps_by_start_mid["w3"].waiting_for, "background")
        check("W4 (tool_use=AskUserQuestion) -> human (the exception)", gaps_by_start_mid["w4"].waiting_for, "human")
        check("W6 (end_turn, SendMessage reply never arrived) -> teammate", gaps_by_start_mid["w6"].waiting_for, "teammate")

        wf_breakdown = own_tool_breakdown(wf_gaps)
        check("own_tool breakdown counts Bash", wf_breakdown.get("Bash"), 1)
        check("own_tool breakdown counts Monitor", wf_breakdown.get("Monitor"), 1)
        check("own_tool breakdown counts SendMessage", wf_breakdown.get("SendMessage"), 1)
        check("own_tool breakdown excludes AskUserQuestion (it's a human wait, not own_tool)", "AskUserQuestion" in wf_breakdown, False)

        cross = waiting_for_x_wake_cause(wf_gaps)
        check("cross-tab has a background row", "background" in cross, True)
        check("cross-tab has a teammate row", "teammate" in cross, True)

        # --- PR #425 review: 3 real leaks found in the context-guru main
        # cross-tab, each confirmed against a real session/timestamp/entry
        # before fixing, now regression-tested here. ---
        reg_session_id = "regression-session"
        reg_path = os.path.join(tmp, f"{reg_session_id}.jsonl")
        with open(reg_path, "w") as fh:
            # Leak 1: AskUserQuestion/ExitPlanMode's answer travels back as a
            # bare tool_result (the API's own protocol for returning a
            # tool's result) -- classify_wake_cause must not call that
            # "tool_result" (own next tool call, no human involved); it must
            # defer to the SAME exception waiting_for already applies.
            _write_call_line(fh, iso(0), "r1", "claude-opus-5-5", cache_read=0, cache_creation=500, tool_uses=[("AskUserQuestion", {"questions": []})])
            # Gap r1->r2: 5 min, ending in a BARE tool_result (the human's
            # answer, delivered the only way the API allows).
            _write_user_tool_result_line(fh, iso(1))
            _write_call_line(fh, iso(300), "r2", "claude-opus-5-5", cache_read=400, cache_creation=50)

            # Leak 2a: ordering. r2 ends with NO tool_use while task
            # "ordertest1" (opened earlier, "persistent") is open. The FINISH
            # signal for "ordertest1" arrives DURING the gap r2->r3 (i.e.
            # it is one of the "in-between" entries for THIS gap) -- before
            # the fix, applying it before classifying r2 made the task look
            # already closed, so r2's wait looked like "nothing open".
            _write_call_line(fh, iso(301), "r2b", "claude-opus-5-5", cache_read=400, cache_creation=50, tool_uses=[("Monitor", {"command": "watch"})])
            _write_tool_result_line(fh, iso(302), 'Monitor started (task ordertest1, persistent — runs until TaskStop or session end). You will be notified on each event.')
            _write_call_line(fh, iso(303), "r2c", "claude-opus-5-5", cache_read=400, cache_creation=50)
            # Gap r2c->r3: 5 min, classified by r2c (no tool_use, ordertest1
            # open) -> must be "background", not "human".
            fh.write(json.dumps({"type": "user", "timestamp": iso(600), "message": {"content": "<task-notification>\n<task-id>ordertest1</task-id>\n<summary>completed</summary>\n</task-notification>"}}) + "\n")
            _write_call_line(fh, iso(603), "r3", "claude-opus-5-5", cache_read=400, cache_creation=50)

            # Leak 2b: a Monitor's INTERMEDIATE event notification must NOT
            # close it -- only its own "[Monitor expired ...]" terminal
            # notice does. Open "ordertest2" with the "timeout <ms>" detail
            # format (the 2nd missing Monitor-start variant found).
            _write_call_line(fh, iso(604), "r3b", "claude-opus-5-5", cache_read=400, cache_creation=50, tool_uses=[("Monitor", {"command": "watch2"})])
            _write_tool_result_line(fh, iso(605), 'Monitor started (task ordertest2, timeout 1800000ms). You will be notified on each event.')
            _write_call_line(fh, iso(606), "r3c", "claude-opus-5-5", cache_read=400, cache_creation=50)
            # An ONGOING event (not the terminal notice) -- must not close it.
            fh.write(json.dumps({"type": "user", "timestamp": iso(607), "message": {"content": '<task-notification>\n<task-id>ordertest2</task-id>\n<summary>Monitor event: "watch2"</summary>\n<event>checkpoint 1/10</event>\n</task-notification>'}}) + "\n")
            _write_call_line(fh, iso(608), "r3d", "claude-opus-5-5", cache_read=400, cache_creation=50)
            # Gap r3d->r4: 5 min, classified by r3d -> "background" (ordertest2
            # is still open; only its intermediate event arrived, not its
            # terminal notice).
            _write_call_line(fh, iso(908), "r4", "claude-opus-5-5", cache_read=400, cache_creation=50)

            # Leak 2c: a background Skill launch (forked execution). Its id
            # is NOT in the text -- it is the entry's own toolUseResult.agentId.
            _write_call_line(fh, iso(909), "r4b", "claude-opus-5-5", cache_read=400, cache_creation=50, tool_uses=[("Skill", {"name": "code-review"})])
            _write_tool_result_line(
                fh, iso(910),
                'Skill "code-review" launched (forked execution, running in the background).\n\nRunning in the background as @code-review',
                tool_use_result={"agentId": "askill1", "background": True, "commandName": "code-review", "status": "forked", "success": True},
            )
            _write_call_line(fh, iso(911), "r4c", "claude-opus-5-5", cache_read=400, cache_creation=50)
            # Gap r4c->r5: 5 min, classified by r4c -> "background" (askill1
            # open, no finish signal yet).
            _write_call_line(fh, iso(1211), "r5", "claude-opus-5-5", cache_read=400, cache_creation=50)

            # A teammate wait that DOES close, 7 minutes (420s) after it
            # opened — for summarize_teammate_wait_durations' "closed" path.
            _write_call_line(fh, iso(1212), "r5b", "claude-opus-5-5", cache_read=400, cache_creation=50, tool_uses=[("SendMessage", {"to": "carol", "message": "status?"})])
            _write_user_text_line(fh, iso(1632), 'Another Claude session sent a message:\n<teammate-message teammate_id="carol">all good</teammate-message>')
            _write_call_line(fh, iso(1633), "r6", "claude-opus-5-5", cache_read=400, cache_creation=50)

        reg_calls, reg_gaps, _, reg_teammate_waits = iter_calls_and_gaps(reg_path, reg_session_id, "main", "regression-project")
        reg_gaps = [g for g in reg_gaps if not g.is_cold_start]
        reg_by_start_mid = {g.start.message_id: g for g in reg_gaps}

        check(
            "leak 1 fixed: AskUserQuestion's bare-tool_result answer is wake_cause=human, not tool_result",
            reg_by_start_mid["r1"].cause,
            "human",
        )
        check(
            "leak 2a fixed: a background task open at r2c, closed only DURING the gap, still classifies r2c as background",
            reg_by_start_mid["r2c"].waiting_for,
            "background",
        )
        check(
            "leak 2b fixed: an intermediate Monitor event does not close the task -- r3d still sees it open",
            reg_by_start_mid["r3d"].waiting_for,
            "background",
        )
        check(
            "leak 2c fixed: a background Skill launch (toolUseResult.agentId) registers as an open task",
            reg_by_start_mid["r4c"].waiting_for,
            "background",
        )

        # --- "report how long open teammate waits stay open" (PR #425
        # review): one closed wait (carol, 420s = 7 min) in reg_teammate_waits,
        # one never-closed wait (bob, from the waiting-for-session fixture:
        # opened at W5=iso(1200), the thread's last call is W7=iso(1800), so
        # its lower-bound duration is (1800-1200)/60 = 10 min). ---
        check("carol's teammate wait is recorded as closed", [tw.closed for tw in reg_teammate_waits if tw.name == "carol"], [True])
        check("carol's teammate wait duration is 7 min", [tw.duration_min for tw in reg_teammate_waits if tw.name == "carol"], [7.0])
        check("bob's teammate wait (never answered) is recorded as not closed", [tw.closed for tw in wf_teammate_waits if tw.name == "bob"], [False])
        check("bob's never-closed duration is a 10-min lower bound (to the thread's last call)", [tw.duration_min for tw in wf_teammate_waits if tw.name == "bob"], [10.0])

        tw_summary = summarize_teammate_wait_durations(reg_teammate_waits + wf_teammate_waits)
        check("teammate wait summary: 1 closed", tw_summary["closed_count"], 1)
        check("teammate wait summary: closed median is carol's 7 min", tw_summary["closed_median_min"], 7.0)
        check("teammate wait summary: 1 never closed", tw_summary["never_closed_count"], 1)
        check("teammate wait summary: never-closed median is bob's 10-min lower bound", tw_summary["never_closed_median_min_lower_bound"], 10.0)

        # --- Issue #424 section 4: per-tool stats on the waiting-for-session
        # fixture (W1=Bash, W2=Monitor, W4=AskUserQuestion, W5=SendMessage,
        # each followed by exactly one 5-min wait; W3/W6/W7 have no tool_use
        # so they contribute no ToolWait at all). ---
        wf_call_counts = tool_call_counts({wf_session_id: wf_calls})
        check("call_count: Bash", wf_call_counts.get("Bash"), 1)
        check("call_count: Monitor", wf_call_counts.get("Monitor"), 1)
        check("call_count: AskUserQuestion", wf_call_counts.get("AskUserQuestion"), 1)
        check("call_count: SendMessage", wf_call_counts.get("SendMessage"), 1)
        wf_tool_stats = summarize_tool_waits(wf_tool_waits, wf_call_counts)
        check("tool_waits count (one per tool-ending call)", len(wf_tool_waits), 4)
        check("Bash waits_ge_threshold (the 5-min gap qualifies)", wf_tool_stats["Bash"]["waits_ge_threshold"], 1)
        check("Bash median wait (>= threshold) is the 5-min gap", wf_tool_stats["Bash"]["median_wait_min"], 5.0)
        check("Bash best_n is n/a with only 1 training wait (< 20)", wf_tool_stats["Bash"]["best_n"], "n/a")
        check("AskUserQuestion waits_ge_threshold", wf_tool_stats["AskUserQuestion"]["waits_ge_threshold"], 1)
        # sorted by call_count; all tied at 1 here, so just check every tool present.
        check("summarize_tool_waits covers all 4 tools", set(wf_tool_stats), {"Bash", "Monitor", "AskUserQuestion", "SendMessage"})

        # PR #425 review: own_tool_breakdown and summarize_tool_waits must
        # reconcile exactly (same gaps, same "last tool_use" rule, same
        # threshold) for every tool except AskUserQuestion/ExitPlanMode,
        # which own_tool_breakdown excludes (they wait on the human).
        wf_breakdown_check = own_tool_breakdown(wf_gaps)
        for tool, stats in wf_tool_stats.items():
            if tool in ASK_HUMAN_TOOLS:
                continue
            check(
                f"own_tool_breakdown[{tool}] reconciles with waits_ge_threshold",
                wf_breakdown_check.get(tool, 0),
                stats["waits_ge_threshold"],
            )

        # --- Issue #424 section 4: the hierarchical ping-budget policy, built
        # and evaluated directly on hand-computed Gap/ToolWait objects (no
        # need to round-trip through files for this part — split_sessions_by_time
        # and simulate_policies only look at .ts / .gap_min / .waiting_for /
        # .tool_use_names, not at any transcript-specific field).
        def mk_call(ts: float, tools: tuple[str, ...] = ()) -> Call:
            return Call(ts=ts, message_id="x", model="claude-opus-5-5", is_error=False, tool_use_names=tools)

        def mk_gap(thread_id: str, gap_min: float, waiting_for: str, tools: tuple[str, ...] = ()) -> Gap:
            start = mk_call(0.0, tools)
            end = mk_call(gap_min * 60.0)
            return Gap(thread_id=thread_id, thread_kind="main", project="policy-test", start=start, end=end, gap_min=gap_min, cause="unknown", waiting_for=waiting_for)

        def mk_tool_wait(thread_id: str, tool: str, gap_min: float) -> ToolWait:
            start = mk_call(0.0, (tool,))
            end = mk_call(gap_min * 60.0)
            return ToolWait(project="policy-test", thread_kind="main", thread_id=thread_id, tool=tool, gap_min=gap_min, start=start, end=end, fallback_group="own_tool")

        # 25 Bash waits @ 6 min (own_tool best N: cost is 0.1 for any N>=1,
        # so best_n_for_gaps picks the first tied N, 1) and 25 human waits @
        # 20 min (best N: cost is 1.15+0.1N below N=4, then flat at 0.4 for
        # N>=4 — picks 4) -- see the comment math in the PR/README.
        train_gaps = [mk_gap("train/t", 6.0, "own_tool", ("Bash",)) for _ in range(25)]
        train_gaps += [mk_gap("train/t", 20.0, "human") for _ in range(25)]
        train_tool_waits = [mk_tool_wait("train/t", "Bash", 6.0) for _ in range(25)]
        test_gaps = [mk_gap("test/t", 6.0, "own_tool", ("Bash",)), mk_gap("test/t", 20.0, "human")]

        policy = simulate_policies(train_gaps, test_gaps, train_tool_waits)
        check("policy: Bash qualifies for its own budget (25 >= 20 train waits)", policy["hierarchical_cost_x_per_gap"], 0.25)
        check("policy: global best N learned on train", policy["global_best_n"], 4)
        check("policy: global-N cost on test", policy["global_best_n_cost_x_per_gap"], 0.25)
        check("policy: waiting_for-only cost on test", policy["waiting_for_only_cost_x_per_gap"], 0.25)
        check("policy: fixed 2 pings costs clearly more here", policy["fixed_2_pings_cost_x_per_gap"], 0.725)

        # repeated_split_policy_comparison: 4 sessions (2 train-labeled, 2
        # test-labeled, by thread_id prefix) is enough to exercise the
        # mechanics -- random_session_split is deterministic per seed, so a
        # fixed n_splits must come back with exactly that many values per
        # policy, each a valid x/gap average.
        rs_calls_by_thread = {
            "rs1/main": [mk_call(0.0)],
            "rs2/main": [mk_call(0.0)],
            "rs3/main": [mk_call(0.0)],
            "rs4/main": [mk_call(0.0)],
        }
        rs_gaps = (
            [mk_gap("rs1/main", 6.0, "own_tool", ("Bash",)) for _ in range(5)]
            + [mk_gap("rs2/main", 20.0, "human") for _ in range(5)]
            + [mk_gap("rs3/main", 6.0, "own_tool", ("Bash",)) for _ in range(5)]
            + [mk_gap("rs4/main", 20.0, "human") for _ in range(5)]
        )
        rs_tool_waits = [mk_tool_wait("rs1/main", "Bash", 6.0) for _ in range(5)] + [mk_tool_wait("rs3/main", "Bash", 6.0) for _ in range(5)]
        rs_result = repeated_split_policy_comparison(rs_gaps, rs_tool_waits, rs_calls_by_thread, n_splits=5)
        check("repeated_split: n_splits", rs_result["n_splits"], 5)
        check("repeated_split: 5 values per policy", len(rs_result["hierarchical"]["values"]), 5)
        check("repeated_split: hierarchical mean is a plausible x/gap value", 0.0 <= rs_result["hierarchical"]["mean"] <= 1.15, True)
        check("repeated_split: min <= mean <= max", rs_result["hierarchical"]["min"] <= rs_result["hierarchical"]["mean"] <= rs_result["hierarchical"]["max"], True)

        # split_sessions_by_time: 3 sessions at increasing times -> train gets
        # the first 2 (the extra one on an odd split), test gets the last 1.
        calls_by_thread = {
            "s1/main": [mk_call(100.0)],
            "s2/main": [mk_call(200.0)],
            "s3/main": [mk_call(300.0)],
        }
        train_s, test_s = split_sessions_by_time(calls_by_thread)
        check("split_sessions_by_time: train gets the earlier majority", train_s, {"s1", "s2"})
        check("split_sessions_by_time: test gets the rest", test_s, {"s3"})

        # --- Issue #424 section 4: unused tools, and the DB-undercoverage fix
        # (a tool the TRANSCRIPT shows as called must never show as unused
        # just because the DB's own tool_uses table missed that session). ---
        unused_db_path = os.path.join(tmp, "unused-tools.db")
        ucon = sqlite3.connect(unused_db_path)
        ucon.execute("create table tool_declarations (session_id text, kind text, name text)")
        ucon.execute("create table tool_uses (session_id text, name text, calls integer)")
        for name in ("Bash", "Read", "Write"):
            ucon.execute("insert into tool_declarations values (?,?,?)", ("ut-session", "tool", name))
        ucon.execute("insert into tool_uses values (?,?,?)", ("ut-session", "Bash", 5))
        ucon.commit()
        ucon.close()

        ut = unused_tools_report({"ut-session"}, [unused_db_path], called_from_transcripts={"Write"})
        check("unused tools: declared count", ut["declared_count"], 3)
        # Bash: called per the DB. Write: called per the TRANSCRIPT, even
        # though the DB's tool_uses has no row for it (the undercoverage
        # case) -- it must NOT show up as unused. Read: never called by
        # either source -- it is genuinely unused.
        check("unused tools: Read is unused, Bash and Write are not", ut["unused"], ["Read"])

        ut_no_db = unused_tools_report({"ut-session"}, [], called_from_transcripts={"Bash", "Write"})
        check("unused tools, no DB: falls back to transcript-called list", ut_no_db["source"], "transcripts_only")
        check("unused tools, no DB: reports what was called, not what's unused", ut_no_db["called"], ["Bash", "Write"])

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
