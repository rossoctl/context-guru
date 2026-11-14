#!/usr/bin/env python3
"""Real, sanitized, per-feature examples + temporal drift + subgroup stability.

For every feature Phase 2 of A4's audit (reports/A4-keepalive.md) classed **observed
live** or **reconstructible offline**, this pulls one genuine request row and shows: the
feature's value as it was known at the decision instant, what a keep-alive decision would
therefore have been, an estimated cost/value of that decision, and what actually happened
next in the same conversation. For every feature classed **impossible at proxy layer** or
**requires instrumentation**, it records why no real example can exist instead of building
a fake one.

Nothing here is a second opinion on cost arithmetic: `PriceBook`/`Semantics`/the write and
read rates are imported from `kv_ttl_cost_model.py` and used exactly as written. This file
only adds the per-request feature bookkeeping (turn index, prior gap history, prefix
identity via tool_declarations.digest) that the cost model itself has no reason to carry.

PRIVACY: reads only `requests` (every column is a count/flag/short categorical/hash — no
free text) and `tool_declarations.digest`/`.tokens`/`.ts` (a hash and two numbers, never
`.text_gz`/`.text_hash`) from the request DB, and only `keepalive_strategies` from the
control DB. Never touches `request_content`, `declaration_text`, `request_components.err`/
`.events`, or `tenants` (email/label/config_yaml). Tenant ids are pseudonymized T01..T18
(assigned by SORTED RAW tenant_id, never the raw id itself) before anything is written to
disk. Session ids are truncated to an 8-hex-char hash. See privacy_scan() at the bottom,
run automatically by --out and reported in its exit summary.

Usage:
    kv_ttl_examples.py --db snap/cg.db --control-db snap/cg-control.db \
        --prices /tmp/a4-kvpred/prices.yaml --out-dir exp3/examples
"""
from __future__ import annotations

import argparse
import bisect
import hashlib
import json
import math
import os
import re
import sqlite3
import sys
from collections import defaultdict
from dataclasses import dataclass
from pathlib import Path
from urllib.parse import quote

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from kv_ttl_cost_model import PriceBook, Semantics, TTL_5M, TTL_1H, TTL_NONE  # noqa: E402

# ── constants ────────────────────────────────────────────────────────────────

FIVE_MIN_MS = 5 * 60 * 1000
ONE_HOUR_MS = 60 * 60 * 1000
# The 2026-08-17 -> 08-19 bootstrap window: a claude-sonnet-5 price step (implied 2.11849
# that week vs exactly 2.28000 after) plus 1,696 rows of an unrelated attribution defect.
# Per the assignment, excluded from the headline drift series and dual-reported.
BOOTSTRAP_WINDOW_START_MS = 1786953600000  # 2026-08-17T00:00:00Z
BOOTSTRAP_WINDOW_END_MS = 1787126400000    # 2026-08-19T00:00:00Z

# stop_reason clusters, from deploy/harbor/kv_ttl_predictor_arms.py (reused, not
# re-derived): the "still working" majority is the reason historical-probability's naive
# keying fails; "actually done" is what stop-reason-gated pings on.
STILL_WORKING = frozenset({"tool_use", "stop_sequence", "tool_calls", "length", "content_filter"})
LOOKS_DONE_ISNT = frozenset({"stop", ""})
ACTUALLY_DONE = frozenset({"end_turn", "max_tokens", "refusal"})


def stop_cluster(stop_reason: str | None) -> str:
    sr = stop_reason or ""
    if sr in ACTUALLY_DONE:
        return "actually_done"
    if sr in STILL_WORKING:
        return "still_working"
    return "looks_done_isnt"


def band_of(idle_ms: int | None) -> str:
    """The 3 bands this whole line of work reasons about: still within the free 5-minute
    hold, would need the 1-hour tier, or would need a keep-alive/re-create either way."""
    if idle_ms is None:
        return "unobserved"
    if idle_ms <= FIVE_MIN_MS:
        return "le_5m"
    if idle_ms <= ONE_HOUR_MS:
        return "5m_1h"
    return "gt_1h"


def in_band(idle_ms: int | None) -> bool | None:
    """The outcome variable drift/subgroup tables are built on: did the NEXT request in
    this conversation arrive within the free 5-minute hold. None means censored (no next
    request observed) -- reported as such, never coerced to False."""
    if idle_ms is None:
        return None
    return idle_ms <= FIVE_MIN_MS


def sha_hash(*parts: str) -> str:
    return hashlib.sha256("|".join(parts).encode()).hexdigest()


def short_session(tenant_pseudo: str, session_id: str) -> str:
    """Pseudonymized tenant + an 8-hex-char truncated hash of the real session id. Never
    the raw session_id, per the assignment's privacy rule."""
    return f"{tenant_pseudo}:{sha_hash(session_id)[:8]}"


# ── loading (safe columns only -- see module docstring) ─────────────────────

def _ro_uri(path: str) -> str:
    return f"file:{quote(Path(path).resolve().as_posix())}?mode=ro"


def load_requests(db_path: str) -> list[dict]:
    """Every column of `requests`. Checked against PRAGMA table_info (see the report):
    every one is an INTEGER/REAL count-or-flag or a short categorical TEXT (model name,
    stop_reason, agent, ...) -- there is no free-text or content column in this table."""
    con = sqlite3.connect(_ro_uri(db_path), uri=True)
    try:
        con.row_factory = sqlite3.Row
        rows = [dict(r) for r in con.execute(
            "SELECT * FROM requests WHERE session_id <> '' "
            "AND token_accounting <> 'missing' ORDER BY tenant_id, session_id, model, ts, id"
        )]
    finally:
        con.close()
    return rows


def load_tool_digests(db_path: str) -> dict[tuple[str, str], tuple[list[int], list[str]]]:
    """(tenant_id, session_id) -> (sorted ts list, parallel digest list), deduplicated on
    (session, ts) -- the table logs ONE ROW PER DECLARED TOOL, so a session with a large
    tool count logs the same (ts, digest) many times over; DISTINCT collapses that back to
    one entry per declaration EVENT, which is what a per-request lookup needs. digest is a
    hash; ts is a timestamp. text_gz/text_hash (content-derived) are never selected."""
    con = sqlite3.connect(_ro_uri(db_path), uri=True)
    try:
        tmp: dict[tuple[str, str], list[tuple[int, str]]] = defaultdict(list)
        for tenant_id, session_id, ts, digest in con.execute(
            "SELECT DISTINCT tenant_id, session_id, ts, digest FROM tool_declarations "
            "ORDER BY ts"
        ):
            tmp[(tenant_id, session_id)].append((ts, digest))
        out: dict[tuple[str, str], tuple[list[int], list[str]]] = {}
        for key, pairs in tmp.items():
            pairs.sort()
            out[key] = ([p[0] for p in pairs], [p[1] for p in pairs])
        return out
    finally:
        con.close()


def load_keepalive_strategies(control_db_path: str) -> list[dict]:
    con = sqlite3.connect(_ro_uri(control_db_path), uri=True)
    try:
        con.row_factory = sqlite3.Row
        return [dict(r) for r in con.execute(
            "SELECT id, idle_seconds, max_pings, min_prefix_tokens, active, predictor_id, "
            "predictor_threshold, head_ttl_1h FROM keepalive_strategies"
        )]
    finally:
        con.close()


# ── conversation/session derivation (backward-only) ─────────────────────────

def digest_at_or_before(digests: tuple[list[int], list[str]], ts: int) -> str | None:
    """Latest tool_declarations digest with ts <= the given time, by binary search --
    called once per request, so linear scan over a 25k-entry session would be O(n^2)."""
    ts_list, digest_list = digests
    if not ts_list:
        return None
    idx = bisect.bisect_right(ts_list, ts) - 1
    return digest_list[idx] if idx >= 0 else None


def annotate(rows: list[dict], digests_by_session: dict) -> None:
    """Fills in, per row, every backward-only feature this file's examples need. Mutates
    `rows` in place. Two grouping keys, deliberately different:

      - conv_key = (tenant_id, session_id, model): a cache entry does not survive a model
        change, so idle gaps / turn index / prefix identity are all keyed on this (matches
        kv_ttl_cost_model.Request.key).
      - sess_key = (tenant_id, session_id): a client session spans models; session age and
        tool-declaration history are keyed on this instead.
    """
    by_conv: dict[tuple, list[dict]] = defaultdict(list)
    by_sess: dict[tuple, list[dict]] = defaultdict(list)
    for r in rows:
        by_conv[(r["tenant_id"], r["session_id"], r["model"])].append(r)
        by_sess[(r["tenant_id"], r["session_id"])].append(r)

    for sess_rows in by_sess.values():
        sess_rows.sort(key=lambda r: (r["ts"], r["id"]))
        first_ts = sess_rows[0]["ts"]
        for r in sess_rows:
            r["_sess_age_ms"] = r["ts"] - first_ts

    for conv_rows in by_conv.values():
        conv_rows.sort(key=lambda r: (r["ts"], r["id"]))
        idles: list[int | None] = []
        for i, r in enumerate(conv_rows):
            r["_turn_index"] = i
            r["_prev"] = conv_rows[i - 1] if i > 0 else None
            r["_next"] = conv_rows[i + 1] if i + 1 < len(conv_rows) else None
            idles.append(conv_rows[i + 1]["ts"] - r["ts"] if r["_next"] else None)
        for i, r in enumerate(conv_rows):
            r["idle_ms"] = idles[i]
            # prior_gap_bands: bands of gaps strictly completed BEFORE this row arrived,
            # i.e. idles[0 .. i-2] -- idles[i-1] is the gap that produced THIS row and is
            # simultaneous with it, not prior knowledge (see report's methodology note).
            counts = {"le_5m": 0, "5m_1h": 0, "gt_1h": 0}
            for g in idles[: max(0, i - 1)]:
                if g is not None:
                    counts[band_of(g)] += 1
            r["_prior_gap_bands"] = counts
            r["_prior_n_gaps"] = sum(counts.values())
            r["_cached_context"] = (r["_prev"]["cache_read"] + r["_prev"]["cache_write"]
                                     if r["_prev"] else 0)
            r["_prior_stop_reason"] = r["_prev"]["stop_reason"] if r["_prev"] else None
            r["_prior_tokens_after"] = r["_prev"]["tokens_after"] if r["_prev"] else None
            r["_prior_cache_ttl"] = r["_prev"]["cache_ttl"] if r["_prev"] else None
            key = (r["tenant_id"], r["session_id"])
            digests = digests_by_session.get(key, ([], []))
            r["_digest_now"] = digest_at_or_before(digests, r["ts"])
            r["_digest_prior"] = (digest_at_or_before(digests, r["_prev"]["ts"])
                                   if r["_prev"] else None)
            # Component-level breakdown, kept separate rather than folded straight into one
            # boolean, because they are not equally trustworthy: tools/system_blocks/model/
            # thinking_mode are coarse counts/labels that change on ~22%/0.5%/-/17% of ALL
            # consecutive real-request pairs in this snapshot (measured, see
            # calibration_stats()) for reasons that have nothing to do with cache
            # compatibility (e.g. a thinking-mode toggle a user made deliberately); digest
            # is a hash of the actual declared-tool set and is the one signal that directly
            # answers "would this request's prefix still match the entry". _prefix_changed
            # (the coarse OR) is kept only for the "changed tools/system/model/thinking"
            # feature example, which is explicitly about those observed columns; anything
            # deciding root cause (diagnose_ping_writes) uses _digest_changed when available
            # and says so when it is not.
            r["_tools_changed"] = r["_prev"] is not None and r["tools"] != r["_prev"]["tools"]
            r["_system_blocks_changed"] = (r["_prev"] is not None
                                            and r["system_blocks"] != r["_prev"]["system_blocks"])
            r["_model_changed"] = r["_prev"] is not None and r["model"] != r["_prev"]["model"]
            r["_thinking_changed"] = (r["_prev"] is not None
                                       and r["thinking_mode"] != r["_prev"]["thinking_mode"])
            r["_digest_changed"] = (
                (r["_digest_now"] != r["_digest_prior"])
                if (r["_digest_now"] is not None and r["_digest_prior"] is not None)
                else None)
            r["_prefix_changed"] = (r["_prev"] is not None
                                     and (r["_tools_changed"] or r["_system_blocks_changed"]
                                          or r["_model_changed"] or r["_thinking_changed"]
                                          or bool(r["_digest_changed"])))

    # cold-start / hierarchical-pooling counter: number of PRIOR requests for this
    # (tenant_id, model), across all that tenant's sessions, strictly before this row --
    # exactly the population kvcache.History's fallback chain pools over before a session
    # has enough of its own history.
    by_user_model: dict[tuple[str, str], int] = defaultdict(int)
    for r in sorted(rows, key=lambda r: (r["ts"], r["id"])):
        key = (r["tenant_id"], r["model"])
        r["_prior_user_model_n"] = by_user_model[key]
        by_user_model[key] += 1


# ── tenant pseudonymization ──────────────────────────────────────────────────

def pseudonym_map(rows: list[dict]) -> dict[str, str]:
    """Sorted RAW tenant_id -> T01..T18, per the assignment's exact convention (uppercase
    T, distinct from other reports' lowercase t-prefixed pseudonyms -- stated once here so
    a reader cross-referencing those reports isn't confused by the case difference)."""
    tenants = sorted({r["tenant_id"] for r in rows})
    return {t: f"T{i+1:02d}" for i, t in enumerate(tenants)}


# ── feature registry ─────────────────────────────────────────────────────────

@dataclass
class Feature:
    key: str
    family: str
    label: str
    kind: str  # "categorical" | "continuous" | "boolean"
    status: str  # A4 matrix label
    note: str
    # decision-time value, as known right after r["_prev"] finished (None if not
    # applicable -- e.g. turn_index 0 has no prior history for a history-based feature)
    decision_value: object  # callable(row) -> value | None
    # the realized value of the SAME quantity at/after the row that follows the decision
    outcome_value: object  # callable(row) -> value | None (None means censored/n-a)
    applicable: object  # callable(row) -> bool


def _turn0_safe(fn):
    def wrapped(row):
        if row["_prev"] is None:
            return None
        return fn(row)
    return wrapped


MEASURABLE_FEATURES: list[Feature] = [
    Feature("inter_request_gap", "user_behaviour",
            "inter-request gap distribution & survival", "continuous", "observed live",
            "requests.ts per (tenant,session,model); every arm in kv_ttl_cost_model.py "
            "already computes this.",
            decision_value=lambda r: r["_prior_gap_bands"],
            outcome_value=lambda r: r["idle_ms"],
            applicable=lambda r: r["_prior_n_gaps"] >= 1),
    Feature("return_probability_band", "user_behaviour",
            "return probability (band/hazard)", "continuous", "reconstructible offline",
            "derived from the gap distribution via kvcache.History's bucketed counts.",
            decision_value=lambda r: (r["_prior_gap_bands"]["le_5m"] / r["_prior_n_gaps"]
                                       if r["_prior_n_gaps"] else None),
            outcome_value=lambda r: r["idle_ms"],
            applicable=lambda r: r["_prior_n_gaps"] >= 3),
    Feature("weekday", "user_behaviour", "weekday", "categorical", "observed live",
            "requests.ts -> weekday, exactly as kv_ttl_predictor_arms.py derives it.",
            decision_value=lambda r: WEEKDAY_NAMES[(r["ts"] // 86_400_000 + 4) % 7],
            outcome_value=lambda r: r["idle_ms"],
            applicable=lambda r: True),
    Feature("recency", "user_behaviour", "recency (SinceLastMs)", "continuous",
            "observed live", "already on kvcache.Observation.",
            decision_value=_turn0_safe(lambda r: r["ts"] - r["_prev"]["ts"]),
            outcome_value=lambda r: r["idle_ms"],
            applicable=lambda r: r["_prev"] is not None),
    Feature("session_length", "user_behaviour", "session length (turns so far)",
            "continuous", "observed live",
            "COUNT(*) per session up to this point, or Turn ordinal on Observation.",
            decision_value=lambda r: r["_turn_index"],
            outcome_value=lambda r: r["idle_ms"],
            applicable=lambda r: True),
    Feature("cold_start_pooling", "user_behaviour",
            "cold-start / hierarchical pooling", "continuous", "reconstructible offline",
            "kvcache.History's own fallback chain (LevelUserBucket -> ... -> LevelGlobal, "
            "minCell=6) -- reused, not reinvented.",
            decision_value=lambda r: r["_prior_user_model_n"],
            outcome_value=lambda r: r["idle_ms"],
            applicable=lambda r: True),

    Feature("agent", "agent_workflow", "agent (client dialect)", "categorical",
            "observed live", "requests.agent.",
            decision_value=lambda r: r["agent"],
            outcome_value=lambda r: r["idle_ms"],
            applicable=lambda r: True),
    Feature("model", "agent_workflow", "model", "categorical", "observed live",
            "requests.model.",
            decision_value=lambda r: r["model"],
            outcome_value=lambda r: r["idle_ms"],
            applicable=lambda r: True),
    Feature("turn_index", "agent_workflow", "turn index", "continuous", "observed live",
            "ordinal within (tenant,session,model), already on Observation.Turn.",
            decision_value=lambda r: r["_turn_index"],
            outcome_value=lambda r: r["idle_ms"],
            applicable=lambda r: True),
    Feature("stop_reason", "agent_workflow", "stop_reason (of the last completed turn)",
            "categorical", "observed live", "requests.stop_reason.",
            decision_value=_turn0_safe(lambda r: r["_prior_stop_reason"]),
            outcome_value=lambda r: r["idle_ms"],
            applicable=lambda r: r["_prev"] is not None),
    Feature("context_headroom", "agent_workflow",
            "remaining budget (context-window headroom)", "continuous",
            "reconstructible offline, partially",
            "tokens_before/tokens_after give context-window headroom (observed live); a "
            "tenant's DOLLAR budget-remaining is not stored anywhere as a running counter "
            "-- tenant_spend is a monthly rollup, not a live gauge. Only the token half of "
            "this feature has an example; the dollar half is one of the absent features.",
            decision_value=_turn0_safe(lambda r: r["_prior_tokens_after"]),
            outcome_value=lambda r: r["idle_ms"],
            applicable=lambda r: r["_prev"] is not None),
    Feature("session_age", "agent_workflow",
            "session age (wall-clock since first request)", "continuous",
            "observed live", "MIN(ts) per (tenant,session).",
            decision_value=lambda r: r["_sess_age_ms"],
            outcome_value=lambda r: r["idle_ms"],
            applicable=lambda r: True),
    Feature("active_vs_idle", "agent_workflow", "active-vs-idle (right now)", "boolean",
            "reconstructible offline for replay",
            "the running proxy/keepalive.go tracks this in-memory (kaEntry); a replay "
            "reconstructs it exactly by comparing SinceLastMs to idle_seconds.",
            decision_value=_turn0_safe(
                lambda r: (r["ts"] - r["_prev"]["ts"]) > 280_000),
            outcome_value=lambda r: r["idle_ms"],
            applicable=lambda r: r["_prev"] is not None),

    Feature("tool_names", "pending_tools", "tool name/type (session-level)",
            "categorical", "observed live",
            "tool_uses(tenant_id, session_id, name, ...) -- session-level, not "
            "per-request; this example shows the tenant's declared-tool digest count as "
            "the request-level proxy for 'a tool is in play', since tool_uses itself has "
            "no per-request cut point (see the absent-feature list for what this can't do).",
            decision_value=_turn0_safe(lambda r: r["_prev"]["tools"]),
            outcome_value=lambda r: r["idle_ms"],
            applicable=lambda r: r["_prev"] is not None),

    Feature("prefix_identity", "prefix_economics",
            "per-prefix stable identity / overlap", "boolean",
            "reconstructible offline",
            "tool_declarations.digest changing between two requests in the same session is "
            "a directly observable prefix-break signal.",
            decision_value=_turn0_safe(lambda r: r["_digest_prior"]),
            outcome_value=lambda r: r["_digest_now"],
            applicable=lambda r: r["_digest_prior"] is not None and r["_digest_now"] is not None),
    Feature("changed_tools_system", "prefix_economics",
            "changed tools/system/model/thinking", "boolean", "observed live",
            "requests.tools/system_blocks/model/thinking_mode are populated columns; a "
            "delta between consecutive requests is a one-line diff.",
            decision_value=_turn0_safe(lambda r: r["_prefix_changed"]),
            outcome_value=lambda r: r["idle_ms"],
            applicable=lambda r: r["_prev"] is not None),
    Feature("compaction_marker", "prefix_economics",
            "branch/compaction marker", "boolean", "reconstructible offline, imperfectly",
            "no explicit compaction flag exists; tokens_before dropping sharply relative "
            "to the immediately preceding request's tokens_after is the observable "
            "footprint, not cleanly separable from a genuine topic change.",
            decision_value=_turn0_safe(
                lambda r: (r["tokens_before"] < 0.5 * r["_prev"]["tokens_after"]
                           if r["_prev"]["tokens_after"] else None)),
            outcome_value=lambda r: r["idle_ms"],
            applicable=lambda r: (r["_prev"] is not None and r["_prev"]["tokens_after"])),
    Feature("cache_read_write", "prefix_economics", "observed cache read/write",
            "categorical", "observed live",
            "requests.cache_read, cache_write, cache_write_1h, cache_miss_reason.",
            decision_value=_turn0_safe(lambda r: r["_prev"]["cache_miss_reason"]),
            outcome_value=lambda r: r["cache_miss_reason"],
            applicable=lambda r: r["_prev"] is not None),
    Feature("expected_reusable_tokens", "prefix_economics", "expected reusable tokens",
            "continuous", "reconstructible offline",
            "attempted_tokens/frozen_tokens/saved_unique + the cache columns -- exactly "
            "what kv_ttl_cost_model.derive()/Request.cached_context already compute.",
            decision_value=lambda r: r["_cached_context"],
            outcome_value=lambda r: r["cache_read"] + r["cache_write"],
            applicable=lambda r: r["_prev"] is not None),
    Feature("subsequent_compatible_count", "prefix_economics",
            "count of subsequent compatible requests", "continuous",
            "reconstructible offline",
            "a forward scan per session once prefix-compatibility is defined -- mechanical, "
            "not new data. Necessarily 0 AT the decision instant (the future is unknown); "
            "the example shows the value once observed.",
            decision_value=lambda r: 0,
            outcome_value=lambda r: r.get("_subseq_compatible"),
            applicable=lambda r: r.get("_subseq_compatible") is not None),
]

WEEKDAY_NAMES = ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"]  # index = (days+4)%7

ABSENT_FEATURES = [
    ("active_hours_tz", "user_behaviour", "active hours, correct tenant-local timezone",
     "impossible at proxy layer, as currently stored",
     "requests.ts is UTC-only; cg-control.db.tenants carries no timezone column (checked "
     "PRAGMA table_info(tenants) directly). Each keep-alive strategy's own windows_json "
     "carries a tz, but that is the manager's admin-set schedule, not a verified per-tenant "
     "residency fact, and it is never joined onto requests."),
    ("project_switching", "user_behaviour", "project/workspace switching",
     "impossible at proxy layer",
     "no project/workspace identifier exists anywhere in requests; the proxy sees a "
     "session_id and a tenant, nothing about which repo or directory the client is in."),
    ("phase", "agent_workflow", "phase (planning/executing/reviewing)",
     "impossible at proxy layer",
     "no such field exists; it would require the client to declare it, which no observed "
     "client does."),
    ("error_retry_outcome", "agent_workflow", "error/retry/test outcome",
     "impossible at proxy layer",
     "requests.status is the HTTP status of the proxy's OWN response to the client, not a "
     "signal about whether the agent's task succeeded; nothing reflects task-level "
     "success/failure."),
    ("task_completion", "agent_workflow", "task completion", "impossible at proxy layer",
     "same root cause as error/retry outcome -- no task-level signal crosses the proxy "
     "boundary."),
    ("tool_call_timing", "pending_tools", "tool start/elapsed/remaining time, per call",
     "requires instrumentation",
     "tool_uses has only first_ts/last_ts bounding the WHOLE SESSION's calls to a tool by "
     "name, plus a calls count -- no per-invocation row, no duration."),
    ("tool_call_outcome", "pending_tools", "success/failure, per call",
     "requires instrumentation",
     "same limitation as tool_call_timing -- no per-call outcome field exists."),
    ("tool_next_request_link", "pending_tools",
     "tool-completion -> next-request probability (request-level linkage)",
     "requires instrumentation",
     "tool_uses is (tenant_id, session_id, name)-keyed, a per-session aggregate; there is "
     "no way to join a specific tool invocation to the specific requests row whose "
     "SinceLastMs it explains. Building this needs a new per-request-scoped tool-call log "
     "(e.g. a request_id foreign key at capture time), a schema/write-path change."),
    ("subagent_ids", "subagents", "parent/child session ids",
     "impossible at proxy layer, as currently stored",
     "checked requests.session_id for any nested/multi-colon pattern that might encode a "
     "parent -- none found. archived_sessions has no parent/child column either."),
    ("subagent_progress", "subagents", "child progress/completion -> parent continuation",
     "impossible at proxy layer",
     "same root cause as subagent_ids -- no hierarchy is recorded anywhere, so nothing "
     "downstream of it can be reconstructed offline."),
    ("subagent_instrumentation", "subagents",
     "non-sensitive subagent instrumentation, going forward", "requires instrumentation",
     "would need the client to declare a parent-session marker; Claude Code's own "
     "Task/subagent invocations show up in tool_uses.name by count only, never as a "
     "structured parent-session field."),
]


# ── forward scan for subsequent_compatible_count ────────────────────────────

def compute_subsequent_compatible(rows: list[dict]) -> None:
    """For every row, how many of the requests that follow it in the same conversation
    stay prefix-compatible (same model/tools/system_blocks/digest) before the first
    change. Purely descriptive -- see the feature note on why this is 0 at decision time."""
    by_conv: dict[tuple, list[dict]] = defaultdict(list)
    for r in rows:
        by_conv[(r["tenant_id"], r["session_id"], r["model"])].append(r)
    for conv_rows in by_conv.values():
        conv_rows.sort(key=lambda r: (r["ts"], r["id"]))
        n = len(conv_rows)
        for i in range(n):
            count = 0
            base = conv_rows[i]
            for j in range(i + 1, n):
                nxt = conv_rows[j]
                if (nxt["tools"] == base["tools"] and nxt["system_blocks"] == base["system_blocks"]
                        and nxt["model"] == base["model"]):
                    count += 1
                else:
                    break
            base["_subseq_compatible"] = count


# ── deterministic selection ──────────────────────────────────────────────────

def pick_nth(rows: list[dict], predicate, rank: int) -> dict | None:
    """The rank-th row (1-based) satisfying predicate, ordered by primary key `id` --
    matches reports/M-meas.md Section 5's "500th matching row by primary key" convention.
    Never the extremum, never hand-picked."""
    matches = sorted((r for r in rows if predicate(r)), key=lambda r: r["id"])
    if not matches:
        return None
    idx = min(rank, len(matches)) - 1
    return matches[idx]


def render_example(feature: Feature, row: dict, tpm: dict[str, str], prices: PriceBook,
                    rank: int, pool_size: int, misleading: bool) -> dict:
    tenant = tpm[row["tenant_id"]]
    decision = feature.decision_value(row)
    outcome_idle = row["idle_ms"]
    would_still_be_alive = (outcome_idle is not None and outcome_idle <= FIVE_MIN_MS)
    price = prices.for_model(row["model"])
    cached = row["_cached_context"]
    sem = Semantics()
    ping_cost = price.keep_alive_cost(cached, sem) if cached else 0.0
    recreate_cost = price.recreate_cost(cached, TTL_5M, sem) if cached else 0.0
    return {
        "feature": feature.key,
        "family": feature.family,
        "label": feature.label,
        "status": feature.status,
        "selection_rule": (
            f"the {rank}-th of {pool_size} rows matching this feature's applicability "
            f"filter, ordered by primary key id (never the extremum, never hand-picked)"),
        "misleading": misleading,
        "request_id": row["id"],
        "tenant": tenant,
        "session": short_session(tenant, row["session_id"]),
        "model": row["model"],
        "turn_index": row["_turn_index"],
        "decision_time_value": _jsonable(decision),
        "predicted_decision": ("hold/ping (return likely inside 5m)"
                                if _predicted_would_hit(feature, decision) else
                                "let it expire (return unlikely inside 5m)"),
        "estimated_ping_cost_usd": round(ping_cost, 6) if cached else None,
        "estimated_ping_cost_usd_label": "estimated" if cached else None,
        "estimated_avoided_recreate_usd": (round(recreate_cost - ping_cost, 6)
                                            if cached else None),
        "estimated_avoided_recreate_usd_label": "estimated" if cached else None,
        "actual_next_event": (
            "unobserved (censored: last request in this conversation)"
            if outcome_idle is None else {
                "gap_ms": outcome_idle,
                "band": band_of(outcome_idle),
                "would_have_hit_5m_cache": would_still_be_alive,
            }),
        "actual_next_event_label": "observed" if outcome_idle is not None else "n/a",
    }


def _predicted_would_hit(feature: Feature, decision) -> bool:
    """A deliberately simple stand-in decision rule per feature, JUST to make the worked
    examples concrete -- not a claim that this is the shipped or best rule. Booleans/high
    fractions/short gaps/actually_done -> predict a return inside 5m."""
    if decision is None:
        return False
    if isinstance(decision, bool):
        return decision
    if isinstance(decision, (int, float)):
        return decision < FIVE_MIN_MS if decision > 1000 else decision > 0.3
    if isinstance(decision, str):
        return decision in ("end_turn", "max_tokens", "refusal", "hit")
    if isinstance(decision, dict):  # prior_gap_bands
        total = sum(decision.values()) or 1
        return decision.get("le_5m", 0) / total > 0.3
    return False


def _jsonable(value):
    if isinstance(value, dict):
        return value
    if isinstance(value, (bool, int, float, str)) or value is None:
        return value
    return str(value)


def build_examples(rows: list[dict], tpm: dict[str, str], prices: PriceBook) -> dict:
    out: dict = {"measured_features": [], "absent_features": [], "misleading_examples": []}
    for feature in MEASURABLE_FEATURES:
        pool = [r for r in rows if feature.applicable(r)]
        if not pool:
            out["measured_features"].append({
                "feature": feature.key, "family": feature.family, "label": feature.label,
                "status": feature.status, "note": feature.note,
                "example": None,
                "reason_no_example": "no row in the snapshot satisfies this feature's "
                                      "applicability filter",
            })
            continue
        rank = min(250, max(1, len(pool) // 2))
        row = pick_nth(rows, feature.applicable, rank)
        ex = render_example(feature, row, tpm, prices, rank, len(pool), misleading=False)
        out["measured_features"].append({
            "feature": feature.key, "family": feature.family, "label": feature.label,
            "status": feature.status, "note": feature.note, "example": ex,
        })

        # misleading example: decision-time prediction disagreed with the observed outcome,
        # i.e. predicted a return inside 5m but it arrived late (or vice versa) -- a
        # deterministic filter (stated), not a hand-pick within it.
        def is_misleading(r, feature=feature):
            if r["idle_ms"] is None or not feature.applicable(r):
                return False
            d = feature.decision_value(r)
            predicted_hit = _predicted_would_hit(feature, d)
            actual_hit = r["idle_ms"] <= FIVE_MIN_MS
            return predicted_hit != actual_hit

        mpool = [r for r in rows if is_misleading(r)]
        if mpool:
            mrank = min(50, max(1, len(mpool) // 2))
            mrow = pick_nth(rows, is_misleading, mrank)
            mex = render_example(feature, mrow, tpm, prices, mrank, len(mpool),
                                  misleading=True)
            out["misleading_examples"].append({
                "feature": feature.key, "family": feature.family, "label": feature.label,
                "example": mex,
            })
        else:
            out["misleading_examples"].append({
                "feature": feature.key, "family": feature.family, "label": feature.label,
                "example": None,
                "reason": "no row in the snapshot has this feature's simple stand-in "
                          "prediction disagreeing with the observed outcome",
            })

    for key, family, label, status, reason in ABSENT_FEATURES:
        out["absent_features"].append({
            "feature": key, "family": family, "label": label, "status": status,
            "reason_no_example": reason,
        })
    return out


# ── worked ping decision, end to end (one that worked, one that didn't) ─────

def worked_ping_examples(behavior_rows: list[dict], all_rows: list[dict], tpm: dict[str, str],
                          prices: PriceBook) -> list[dict]:
    """"worked" is a REAL (non-ping) row, so it is picked from behavior_rows (the
    keepalive-excluded annotation -- its decision-time context is the real conversation
    chain, not a ping chain). "did not work" is a ping row itself, so it is picked from
    all_rows (the ping-inclusive annotation -- a ping's own _prev is the true previous
    request, ping or real)."""
    pings = [r for r in all_rows if r["keepalive"] == 1 and r["status"] == 200]
    out = []
    # "worked": a real row whose keepalive_saved_usd (the credited rescue) is > 0.
    worked_pool = [r for r in behavior_rows if r["keepalive_saved_usd"] and r["keepalive_saved_usd"] > 0]
    # "did not work": a ping that billed a cache WRITE (found nothing to refresh -- see the
    # late-arrival-vs-changed-prefix analysis for why).
    failed_pool = [r for r in pings if r["cache_write"] > 0]
    for label, pool in (("worked", worked_pool), ("did_not_work", failed_pool)):
        if not pool:
            out.append({"outcome": label, "example": None, "reason": "no matching row"})
            continue
        rank = min(20, max(1, len(pool) // 2))
        row = sorted(pool, key=lambda r: r["id"])[min(rank, len(pool)) - 1]
        tenant = tpm[row["tenant_id"]]
        price = prices.for_model(row["model"])
        cached = row["_cached_context"]
        sem = Semantics()
        ping_cost = price.keep_alive_cost(cached, sem) if cached else 0.0
        recreate_cost = price.recreate_cost(cached, TTL_5M, sem) if cached else 0.0
        out.append({
            "outcome": label,
            "selection_rule": f"the {min(rank, len(pool))}-th of {len(pool)} rows in this "
                               f"outcome's pool, ordered by primary key id",
            "request_id": row["id"],
            "tenant": tenant,
            "session": short_session(tenant, row["session_id"]),
            "model": row["model"],
            "observed_features_at_decision": {
                "prior_stop_reason": row["_prior_stop_reason"],
                "cached_context_tokens": cached,
                "prior_gap_bands": row["_prior_gap_bands"],
            },
            "estimated_ping_cost_usd": round(ping_cost, 6),
            "estimated_ping_cost_usd_label": "estimated",
            "estimated_recreate_cost_usd": round(recreate_cost, 6),
            "estimated_recreate_cost_usd_label": "estimated",
            "estimated_ping_value_usd": round(recreate_cost - ping_cost, 6),
            "estimated_ping_value_usd_label": "estimated",
            "actual_cost_usd": row["cost_usd"],
            "actual_cost_usd_label": "observed",
            "actual_keepalive_saved_usd": row["keepalive_saved_usd"],
            "actual_keepalive_saved_usd_label": "observed",
            "actual_cache_read": row["cache_read"],
            "actual_cache_write": row["cache_write"],
            "actual_cache_write_label": "observed",
        })
    return out


# ── the 32-ping-write phenomenon: late arrival vs changed prefix ────────────

def calibration_stats(behavior_rows: list[dict]) -> dict:
    """Base rates of each coarse prefix-change signal over EVERY consecutive real-request
    pair in the snapshot (not just the 32 ping-writes) -- so a reader can judge how much a
    'changed' flag on one of those 32 rows is actually telling them. A signal that fires on
    a fifth of all ordinary turns is weak corroborating evidence at best, not a diagnosis by
    itself; that is why diagnose_ping_writes() below treats digest (a real hash of the
    declared-tool set) as authoritative and the rest as secondary."""
    total = tools_c = sb_c = tm_c = model_c = digest_avail = digest_c = 0
    for r in behavior_rows:
        if r["_prev"] is None:
            continue
        total += 1
        tools_c += r["_tools_changed"]
        sb_c += r["_system_blocks_changed"]
        tm_c += r["_thinking_changed"]
        model_c += r["_model_changed"]
        if r["_digest_changed"] is not None:
            digest_avail += 1
            digest_c += r["_digest_changed"]
    def pct(n, d):
        return round(100 * n / d, 2) if d else None
    return {
        "n_consecutive_pairs": total,
        "tools_count_changed_pct": pct(tools_c, total),
        "system_blocks_changed_pct": pct(sb_c, total),
        "thinking_mode_changed_pct": pct(tm_c, total),
        "model_changed_pct": pct(model_c, total),
        "digest_available_pct": pct(digest_avail, total),
        "digest_changed_pct_of_available": pct(digest_c, digest_avail),
    }


def diagnose_ping_writes(rows: list[dict], strategies: list[dict]) -> dict:
    """rows must be the PING-INCLUSIVE annotation (each ping's _prev is the true previous
    request in the conversation, ping or real) -- the ping-EXCLUSIVE behavior_rows view
    would give a ping the wrong predecessor. `strategies` is cg-control.db's
    keepalive_strategies rows, used to look up the REAL configured idle_seconds trigger for
    each ping (270/275/280s in this snapshot, not a single constant) -- a tighter overshoot
    figure than assuming kv_ttl_cost_model's own PingSchedule default (280s) applies live."""
    strategy_idle_s = {s["id"]: s["idle_seconds"] for s in strategies}
    pings = [r for r in rows if r["keepalive"] == 1 and r["status"] == 200]
    writes = [r for r in pings if r["cache_write"] > 0]
    total_ping_cost = sum(r["cost_usd"] for r in pings)
    write_cost = sum(r["cost_usd"] for r in writes)
    late_arrival, changed_prefix, unexplained, ambiguous, partial, no_entry = [], [], [], [], [], []
    for r in writes:
        prev = r["_prev"]
        entry = {
            "request_id": r["id"], "cache_read": r["cache_read"], "cache_write": r["cache_write"],
        }
        if prev is None:
            entry["diagnosis"] = "no prior request in conversation (first row is a ping) -- unexplained"
            unexplained.append(entry)
            continue
        gap_ms = r["ts"] - prev["ts"]
        tier = TTL_1H if prev["cache_write_1h"] else (TTL_5M if (prev["cache_read"] or prev["cache_write"]) else TTL_NONE)
        if tier == TTL_NONE:
            # The immediately preceding request itself read and wrote nothing -- there was
            # no live cache entry for this ping to have kept alive or found stale, so
            # neither "late arrival" nor "changed prefix" describes it: nothing existed to
            # arrive late for or to have changed against. Kept out of both buckets rather
            # than forced into late_arrival by a vacuously-true "gap > 0" comparison.
            entry["diagnosis"] = "no_prior_entry"
            entry["gap_ms"] = gap_ms
            no_entry.append(entry)
            continue
        lifetime = {TTL_NONE: 0, TTL_5M: FIVE_MIN_MS, TTL_1H: ONE_HOUR_MS}[tier]
        expired_by_time = gap_ms > lifetime
        digest_known = r["_digest_changed"] is not None
        # Authoritative when the digest is known on both sides; otherwise fall back to the
        # coarse OR and say so explicitly rather than silently using a weaker signal.
        prefix_changed = r["_digest_changed"] if digest_known else r["_prefix_changed"]
        entry["gap_ms"] = gap_ms
        entry["prior_entry_tier"] = tier or "none"
        entry["lifetime_ms"] = lifetime
        entry["expired_by_time_alone"] = expired_by_time
        entry["prefix_changed_vs_prior"] = bool(prefix_changed)
        entry["prefix_signal_source"] = "digest" if digest_known else "coarse_fallback(tools/system_blocks/model/thinking)"
        entry["changed_components"] = [
            name for name, flag in (
                ("tools", r["_tools_changed"]), ("system_blocks", r["_system_blocks_changed"]),
                ("model", r["_model_changed"]), ("thinking_mode", r["_thinking_changed"]),
                ("digest", r["_digest_changed"]),
            ) if flag
        ]
        entry["partial"] = r["cache_read"] > 0
        idle_s = strategy_idle_s.get(r["keepalive_strategy_id"])
        entry["configured_idle_seconds"] = idle_s
        entry["overshoot_vs_configured_trigger_ms"] = (
            gap_ms - idle_s * 1000 if idle_s is not None and tier == TTL_5M else None)
        if r["cache_read"] > 0:
            partial.append(entry)
        if expired_by_time and not prefix_changed:
            entry["diagnosis"] = "late_arrival"
            late_arrival.append(entry)
        elif prefix_changed and not expired_by_time:
            entry["diagnosis"] = "changed_prefix"
            changed_prefix.append(entry)
        elif expired_by_time and prefix_changed:
            # Both signals fired -- the gap was long enough to expire the entry on its own,
            # AND the prefix looks different. Cannot cleanly attribute a single cause; the
            # digest-vs-coarse distinction above still tells you whether "changed" here
            # rests on strong or weak evidence, but even the strong version does not tell
            # you which of the two would have been the miss on its own.
            entry["diagnosis"] = "both (cannot separate)"
            ambiguous.append(entry)
        else:
            entry["diagnosis"] = "unexplained (entry should still have been live by time " \
                                  "alone, and the prefix looks unchanged by every field " \
                                  "this analysis can see)"
            unexplained.append(entry)
    overshoots = sorted(e["overshoot_vs_configured_trigger_ms"] for e in late_arrival
                         if e.get("overshoot_vs_configured_trigger_ms") is not None)
    return {
        "n_pings": len(pings),
        "n_writes": len(writes),
        "share_of_pings_pct": round(100 * len(writes) / len(pings), 2) if pings else None,
        "total_ping_cost_usd": round(total_ping_cost, 4),
        "write_ping_cost_usd": round(write_cost, 4),
        "share_of_ping_spend_pct": round(100 * write_cost / total_ping_cost, 2) if total_ping_cost else None,
        "n_partial_reads": len(partial),
        "n_pure_miss": len(writes) - len(partial),
        "late_arrival": {"n": len(late_arrival), "rows": late_arrival},
        "changed_prefix": {"n": len(changed_prefix), "rows": changed_prefix},
        "both_cannot_separate": {"n": len(ambiguous), "rows": ambiguous},
        "no_prior_entry": {"n": len(no_entry), "rows": no_entry},
        "unexplained": {"n": len(unexplained), "rows": unexplained},
        "late_arrival_overshoot_ms": {
            "n_with_known_trigger": len(overshoots),
            "min": overshoots[0] if overshoots else None,
            "median": overshoots[len(overshoots) // 2] if overshoots else None,
            "max": overshoots[-1] if overshoots else None,
            "note": "gap_ms minus the REAL configured idle_seconds*1000 for that ping's own "
                    "keepalive_strategy_id (270/275/280s in this snapshot, looked up from "
                    "cg-control.db, not assumed) -- only defined for 5-minute-tier pings "
                    "whose strategy_id resolves; positive means the ping fired that many ms "
                    "after its own configured trigger, before the entry's 300s lifetime.",
        },
        "caveat": (
            "cache_miss_reason is blank ('') on every one of these rows -- the store does "
            "not record a specific miss reason for keep-alive pings at all, so this "
            "diagnosis is a RECONSTRUCTION from timing (gap vs. the prior entry's own "
            "lifetime) and from prefix-change signals this file can see, not a read of a "
            "stored label. 'unexplained' and 'both (cannot separate)' rows are cases this "
            "reconstruction genuinely cannot resolve cleanly -- reported as such, not "
            "forced into late_arrival or changed_prefix for a tidier table."),
    }


# ── temporal drift ──────────────────────────────────────────────────────────

def week_index(ts_ms: int, origin_ms: int) -> int:
    return (ts_ms - origin_ms) // (7 * 86_400_000)


def bootstrap_ci(values: list[float], conv_ids: list, n_reps: int = 400, seed: int = 0):
    """Conversation-level bootstrap: resample CONVERSATIONS with replacement (not rows),
    per the methodology note. `values`/`conv_ids` are parallel per-row arrays."""
    import random
    if not values:
        return None, None, None
    by_conv: dict = defaultdict(list)
    for v, c in zip(values, conv_ids):
        by_conv[c].append(v)
    convs = list(by_conv.keys())
    point = sum(values) / len(values)
    if len(convs) < 2:
        return point, None, None
    rng = random.Random(seed)
    means = []
    for _ in range(n_reps):
        sample_vals: list[float] = []
        for _ in range(len(convs)):
            c = convs[rng.randrange(len(convs))]
            sample_vals.extend(by_conv[c])
        if sample_vals:
            means.append(sum(sample_vals) / len(sample_vals))
    means.sort()
    if not means:
        return point, None, None
    lo = means[int(0.025 * len(means))]
    hi = means[min(len(means) - 1, int(0.975 * len(means)))]
    return point, lo, hi


MIN_TENANT_N_PER_WEEK = 30  # matches tune_historical_probability's own min_signal convention


def feature_effect_series(feature: Feature, rows: list[dict], origin_ms: int,
                           tpm: dict[str, str]) -> dict:
    """One statistic per feature, reused across all of them (deliberately, to keep this
    file from growing 20 bespoke metrics): for a boolean/categorical decision value, the
    in-band rate among rows where the decision value is 'true-like' minus the pooled
    in-band rate that week (a lift). Every week's point + bootstrap CI is reported.

    Reports BOTH a pooled version and a per-tenant-unweighted version of the same lift,
    per the team-lead's measured finding (this file's own re-derivation, see
    band_rate_tenant_mix_verification, agrees with it): pooling every tenant's rows
    together before computing a week's baseline lets whichever tenant is largest THAT WEEK
    set the week's baseline, so a pure tenant-mix shift (one tenant's share rising or
    falling) can manufacture a trend with no behaviour change behind it. The unweighted
    version computes the SAME lift within each qualifying tenant (>=30 applicable rows
    that week) against that TENANT's own week rate, then averages unweighted across
    tenants -- immune to any one tenant's volume share. Where the two disagree, the
    unweighted one is the one that answers "did the feature's association change"."""
    applicable = [r for r in rows if feature.applicable(r) and r["idle_ms"] is not None]
    weeks: dict[int, list[dict]] = defaultdict(list)
    for r in applicable:
        weeks[week_index(r["ts"], origin_ms)].append(r)
    series = []
    for wk in sorted(weeks):
        wrows = weeks[wk]
        pooled_rate = sum(in_band(r["idle_ms"]) for r in wrows) / len(wrows)
        vals, convs = [], []
        by_tenant: dict[str, list[dict]] = defaultdict(list)
        for r in wrows:
            d = feature.decision_value(r)
            truthy = _predicted_would_hit(feature, d)
            lift = (1.0 if in_band(r["idle_ms"]) else 0.0) - pooled_rate if truthy else None
            if lift is not None:
                vals.append(lift)
                convs.append((r["tenant_id"], r["session_id"], r["model"]))
            by_tenant[tpm[r["tenant_id"]]].append(r)
        point, lo, hi = bootstrap_ci(vals, convs)

        tenant_lifts = []
        top_tenant_n = max((len(v) for v in by_tenant.values()), default=0)
        for trows in by_tenant.values():
            if len(trows) < MIN_TENANT_N_PER_WEEK:
                continue
            t_rate = sum(in_band(r["idle_ms"]) for r in trows) / len(trows)
            t_vals = []
            for r in trows:
                truthy = _predicted_would_hit(feature, feature.decision_value(r))
                if truthy:
                    t_vals.append((1.0 if in_band(r["idle_ms"]) else 0.0) - t_rate)
            if t_vals:
                tenant_lifts.append(sum(t_vals) / len(t_vals))
        unweighted_mean = (sum(tenant_lifts) / len(tenant_lifts)) if tenant_lifts else None

        week_start_ms = origin_ms + wk * 7 * 86_400_000
        series.append({
            "week": wk, "week_start_ts_ms": week_start_ms, "n": len(vals),
            "n_pooled": len(wrows), "pooled_in_band_rate": round(pooled_rate, 4),
            "tenant_top1_share": round(top_tenant_n / len(wrows), 4) if wrows else None,
            "n_qualifying_tenants": len(tenant_lifts),
            "lift_point": round(point, 4) if point is not None else None,
            "lift_ci_lo": round(lo, 4) if lo is not None else None,
            "lift_ci_hi": round(hi, 4) if hi is not None else None,
            "unweighted_tenant_mean_lift": (round(unweighted_mean, 4)
                                             if unweighted_mean is not None else None),
        })
    return {
        "feature": feature.key, "label": feature.label,
        "metric": "in-band-rate lift: (rate among rows where the simple stand-in "
                  "prediction is 'true-like') minus (that week's pooled in-band rate). "
                  "Positive means the feature's true-like value associates with returning "
                  "inside the 5-minute band more often than the week's baseline. "
                  "unweighted_tenant_mean_lift is the same lift computed within each "
                  "tenant (>=30 rows that week) against ITS OWN week rate, then averaged "
                  "unweighted across tenants -- compare to lift_point to see whether tenant "
                  "mix is doing the work (see feature_effect_series docstring).",
        "weekly": series,
    }


def verify_band_rate_drift(rows: list[dict], tpm: dict[str, str],
                            min_tenant_n: int = 200) -> dict:
    """Independent re-derivation of the team-lead's measured finding: pooled vs. unweighted-
    across-tenant 5m-1h band rate, by week, plus each week's largest tenant's share of
    decision points. Same population/partition as the rest of this file (behavior_rows =
    keepalive=0, token_accounting<>'missing', partitioned (tenant_id,session_id,model)
    ordered by (ts,id)); band = idle_ms in (5min, 1h]; only rows with an observed successor
    (idle_ms is not None) count as a decision point, exactly as specified."""
    origin_ms = min(r["ts"] for r in rows)
    applicable = [r for r in rows if r["idle_ms"] is not None]
    weeks: dict[int, list[dict]] = defaultdict(list)
    for r in applicable:
        weeks[week_index(r["ts"], origin_ms)].append(r)
    def is_band(r):
        return FIVE_MIN_MS < r["idle_ms"] <= ONE_HOUR_MS
    out = []
    for wk in sorted(weeks):
        wrows = weeks[wk]
        pooled_rate = sum(is_band(r) for r in wrows) / len(wrows)
        by_tenant: dict[str, list[dict]] = defaultdict(list)
        for r in wrows:
            by_tenant[tpm[r["tenant_id"]]].append(r)
        qualifying = {t: trows for t, trows in by_tenant.items() if len(trows) >= min_tenant_n}
        unweighted = ([sum(is_band(r) for r in trows) / len(trows) for trows in qualifying.values()])
        top1_n = max((len(v) for v in by_tenant.values()), default=0)
        out.append({
            "week": wk, "n": len(wrows), "n_tenants_qualifying": len(qualifying),
            "pooled_band_rate": round(pooled_rate, 4),
            "unweighted_mean_band_rate": (round(sum(unweighted) / len(unweighted), 4)
                                           if unweighted else None),
            "top1_tenant_share": round(top1_n / len(wrows), 4) if wrows else None,
        })
    return {
        "definition": "band = 5min < idle_ms <= 1h; population = behavior_rows (keepalive=0, "
                       "token_accounting<>'missing'); partition (tenant_id,session_id,model) "
                       "ordered by (ts,id); only rows with an observed successor.",
        "min_tenant_n_per_week": min_tenant_n,
        "weekly": out,
    }


def drift_report(rows: list[dict], tpm: dict[str, str]) -> dict:
    origin_ms = min(r["ts"] for r in rows)
    all_series = [feature_effect_series(f, rows, origin_ms, tpm) for f in MEASURABLE_FEATURES]
    excluded = [r for r in rows
                if BOOTSTRAP_WINDOW_START_MS <= r["ts"] < BOOTSTRAP_WINDOW_END_MS]
    included_rows = [r for r in rows
                      if not (BOOTSTRAP_WINDOW_START_MS <= r["ts"] < BOOTSTRAP_WINDOW_END_MS)]
    dual_series = [feature_effect_series(f, included_rows, origin_ms, tpm) for f in MEASURABLE_FEATURES]
    return {
        "with_bootstrap_window": all_series,
        "excluding_2026_08_17_to_19_bootstrap_window": dual_series,
        "bootstrap_window_rows_excluded": len(excluded),
        "note": "the 2026-08-17->08-19 window carries a claude-sonnet-5 price step and "
                "1,696 rows of an unrelated attribution defect (per the assignment); both "
                "series are reported, never only the one that looks cleaner.",
    }


# ── subgroup stability ───────────────────────────────────────────────────────

def turn_band(turn_index: int) -> str:
    if turn_index <= 2:
        return "1-3"
    if turn_index <= 5:
        return "4-6"
    if turn_index <= 10:
        return "7-11"
    return "12+"


def subgroup_report(rows: list[dict], tpm: dict[str, str]) -> list[dict]:
    out = []
    for feature in MEASURABLE_FEATURES:
        applicable = [r for r in rows if feature.applicable(r) and r["idle_ms"] is not None]
        if not applicable:
            out.append({"feature": feature.key, "label": feature.label, "subgroups": [],
                        "note": "no applicable rows"})
            continue
        pooled_rate = sum(in_band(r["idle_ms"]) for r in applicable) / len(applicable)
        pooled_vals, pooled_convs = [], []
        for r in applicable:
            truthy = _predicted_would_hit(feature, feature.decision_value(r))
            if truthy:
                pooled_vals.append((1.0 if in_band(r["idle_ms"]) else 0.0) - pooled_rate)
                pooled_convs.append((r["tenant_id"], r["session_id"], r["model"]))
        pooled_point, _, _ = bootstrap_ci(pooled_vals, pooled_convs, n_reps=200)

        def grouped(keyfn, label_prefix=""):
            groups: dict[str, list[dict]] = defaultdict(list)
            for r in applicable:
                groups[keyfn(r)].append(r)
            rows_out = []
            for gkey, grows in sorted(groups.items(), key=lambda kv: -len(kv[1])):
                if len(grows) < 20:
                    continue
                grate = sum(in_band(r["idle_ms"]) for r in grows) / len(grows)
                vals, convs = [], []
                for r in grows:
                    truthy = _predicted_would_hit(feature, feature.decision_value(r))
                    if truthy:
                        vals.append((1.0 if in_band(r["idle_ms"]) else 0.0) - grate)
                        convs.append((r["tenant_id"], r["session_id"], r["model"]))
                point, lo, hi = bootstrap_ci(vals, convs, n_reps=200)
                sign_reversed = (pooled_point is not None and point is not None
                                  and (pooled_point > 0) != (point > 0)
                                  and abs(point) > 1e-9 and abs(pooled_point) > 1e-9)
                rows_out.append({
                    "group": f"{label_prefix}{gkey}", "n": len(grows),
                    "lift_point": round(point, 4) if point is not None else None,
                    "lift_ci_lo": round(lo, 4) if lo is not None else None,
                    "lift_ci_hi": round(hi, 4) if hi is not None else None,
                    "sign_reversed_vs_pooled": sign_reversed,
                })
            return rows_out

        out.append({
            "feature": feature.key, "label": feature.label,
            "pooled_lift_point": round(pooled_point, 4) if pooled_point is not None else None,
            "by_tenant": grouped(lambda r: tpm[r["tenant_id"]]),
            "by_model": grouped(lambda r: r["model"]),
            "by_stop_cluster": grouped(lambda r: stop_cluster(r["_prior_stop_reason"])),
            "by_turn_band": grouped(lambda r: turn_band(r["_turn_index"])),
        })
    return out


# ── privacy scan ─────────────────────────────────────────────────────────────

UUID_RE = re.compile(
    r"[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}")


def privacy_scan(out_dir: Path, raw_tenant_ids: list[str]) -> dict:
    hits = []
    for path in sorted(out_dir.rglob("*")):
        if not path.is_file() or path.suffix not in (".json", ".txt", ".md", ".svg"):
            continue
        text = path.read_text(errors="replace")
        for tid in raw_tenant_ids:
            if tid in text:
                hits.append({"file": str(path), "kind": "raw_tenant_id", "value": tid})
        for m in UUID_RE.finditer(text):
            hits.append({"file": str(path), "kind": "uuid", "value": m.group(0)})
    return {"files_scanned": sum(1 for p in out_dir.rglob("*") if p.is_file()),
            "hits": hits, "clean": len(hits) == 0}


# ── plots ────────────────────────────────────────────────────────────────────

def make_plots(drift: dict, out_dir: Path) -> None:
    import matplotlib
    matplotlib.use("Agg")
    import matplotlib.pyplot as plt

    plots_dir = out_dir / "plots"
    plots_dir.mkdir(exist_ok=True)
    for series_key, series_label in (
        ("with_bootstrap_window", "all_weeks"),
        ("excluding_2026_08_17_to_19_bootstrap_window", "excl_bootstrap_window"),
    ):
        for fs in drift[series_key]:
            weekly = [w for w in fs["weekly"] if w["lift_point"] is not None]
            if len(weekly) < 2:
                continue
            fig, ax = plt.subplots(figsize=(6, 3.2))
            xs = [w["week"] for w in weekly]
            ys = [w["lift_point"] for w in weekly]
            los = [w["lift_ci_lo"] if w["lift_ci_lo"] is not None else w["lift_point"] for w in weekly]
            his = [w["lift_ci_hi"] if w["lift_ci_hi"] is not None else w["lift_point"] for w in weekly]
            uw = [w.get("unweighted_tenant_mean_lift") for w in weekly]
            ax.axhline(0, color="#999", linewidth=0.8)
            ax.fill_between(xs, los, his, alpha=0.25, color="#4472c4")
            ax.plot(xs, ys, marker="o", color="#4472c4", label="pooled lift")
            if any(v is not None for v in uw):
                uw_xs = [x for x, v in zip(xs, uw) if v is not None]
                uw_ys = [v for v in uw if v is not None]
                ax.plot(uw_xs, uw_ys, marker="s", color="#c0504d", linestyle="--",
                        label="unweighted across tenants")
            ax.legend(fontsize=7)
            ax.set_title(f"{fs['label']} -- weekly in-band lift ({series_label})", fontsize=9)
            ax.set_xlabel("week index (0 = snapshot start)")
            ax.set_ylabel("lift")
            fig.tight_layout()
            fname = f"{fs['feature']}_{series_label}"
            fig.savefig(plots_dir / f"{fname}.svg")
            plt.close(fig)
            (plots_dir / f"{fname}.txt").write_text(
                f"{fs['label']}, {series_label.replace('_', ' ')}. Blue circles: each "
                f"calendar week's in-band-rate lift, POOLING all tenants' rows before "
                f"comparing to that week's pooled baseline (metric defined in drift.json). "
                f"Shaded band is a 95% conversation-level bootstrap CI (400 reps) on the "
                f"pooled line. Red squares (dashed), where present: the same lift computed "
                f"WITHIN each qualifying tenant against its own week's rate, then averaged "
                f"unweighted across tenants -- immune to one tenant's volume share. If the "
                f"two lines track each other, the pooled trend is real; if they diverge, "
                f"the pooled trend is at least partly a tenant-mix artifact (see "
                f"band_rate_tenant_mix_verification in drift.json for the clearest case of "
                f"this in this dataset).\n"
            )

    verify = drift.get("band_rate_tenant_mix_verification")
    if verify and len(verify["weekly"]) >= 2:
        fig, ax = plt.subplots(figsize=(6, 3.2))
        weekly = verify["weekly"]
        xs = [w["week"] for w in weekly]
        pooled = [w["pooled_band_rate"] for w in weekly]
        unweighted = [w["unweighted_mean_band_rate"] for w in weekly]
        ax.plot(xs, pooled, marker="o", color="#4472c4", label="pooled band rate")
        uw_xs = [x for x, v in zip(xs, unweighted) if v is not None]
        uw_ys = [v for v in unweighted if v is not None]
        if uw_ys:
            ax.plot(uw_xs, uw_ys, marker="s", color="#c0504d", linestyle="--",
                    label="unweighted mean across tenants")
        ax2 = ax.twinx()
        ax2.plot(xs, [w["top1_tenant_share"] for w in weekly], color="#9bbb59",
                 linestyle=":", marker="^", label="largest tenant's share")
        ax2.set_ylabel("top-1 tenant share", color="#9bbb59")
        ax.legend(fontsize=7, loc="upper left")
        ax.set_title("5m-1h band rate: pooled vs. unweighted-across-tenant, by week", fontsize=9)
        ax.set_xlabel("week index (0 = snapshot start)")
        ax.set_ylabel("band rate")
        fig.tight_layout()
        fig.savefig(plots_dir / "band_rate_tenant_mix_verification.svg")
        plt.close(fig)
        (plots_dir / "band_rate_tenant_mix_verification.txt").write_text(
            "5-minute-to-1-hour return-band rate by calendar week. Blue circles: every "
            "tenant's rows pooled together before computing the week's rate. Red squares "
            "(dashed): each qualifying tenant's (>=200 decision points that week) own rate, "
            "averaged unweighted across tenants. Green triangles (dotted, right axis): the "
            "largest tenant's share of that week's decision points. If the blue line moves "
            "while the red line stays flat, and the green line falls over the same weeks, "
            "the blue line's movement is a tenant-mix artifact, not a behaviour change -- "
            "one high-volume, low-band-rate tenant's shrinking share pulling the pooled "
            "average toward the typical tenant's rate, not the typical tenant's rate itself "
            "changing. See drift.json's band_rate_tenant_mix_verification for the numbers.\n"
        )


# ── main ─────────────────────────────────────────────────────────────────────

def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--db", default="/home/vpcuser/cg-night-0928/snap/cg.db")
    ap.add_argument("--control-db", default="/home/vpcuser/cg-night-0928/snap/cg-control.db")
    ap.add_argument("--prices", default="/tmp/a4-kvpred/prices.yaml")
    ap.add_argument("--out-dir", default="/home/vpcuser/cg-night-0928/exp3/examples")
    args = ap.parse_args(argv)

    print(f"loading requests from {args.db} (mode=ro) ...", file=sys.stderr)
    all_rows = load_requests(args.db)
    print(f"  {len(all_rows)} rows ({sum(r['keepalive'] for r in all_rows)} are keep-alive "
          f"pings)", file=sys.stderr)
    digests = load_tool_digests(args.db)
    strategies = load_keepalive_strategies(args.control_db)

    print("annotating (backward-only features) ...", file=sys.stderr)
    # Two views, deliberately: a ping is traffic context-guru itself sent while nobody was
    # at the keyboard (kv_ttl_cost_model.py's own load_trajectories excludes it from every
    # arm for the same reason) -- counted as a turn it invents a short gap and shifts every
    # turn_index after it. `all_rows` (ping-inclusive) is annotated FIRST so every ping's
    # own _prev/_cached_context/_prefix_changed point at its true predecessor (ping or
    # real); `behavior_rows` (ping-EXCLUDED, matching kv_ttl_cost_model's `keepalive = 0`)
    # is annotated SECOND, overwriting the shared dict objects for the real rows with the
    # correct ping-free chain. Ping rows are never in behavior_rows, so their first-pass
    # annotation survives untouched.
    annotate(all_rows, digests)
    behavior_rows = [r for r in all_rows if r["keepalive"] == 0]
    annotate(behavior_rows, digests)
    compute_subsequent_compatible(behavior_rows)

    tpm = pseudonym_map(all_rows)
    raw_tenant_ids = sorted(tpm.keys())
    prices = PriceBook.from_operator_file(args.prices)

    out_dir = Path(args.out_dir)
    out_dir.mkdir(parents=True, exist_ok=True)

    print("building per-feature examples ...", file=sys.stderr)
    examples = build_examples(behavior_rows, tpm, prices)
    examples["ping_worked_and_failed"] = worked_ping_examples(behavior_rows, all_rows, tpm, prices)
    examples["ping_write_diagnosis"] = diagnose_ping_writes(all_rows, strategies)
    examples["ping_write_diagnosis"]["calibration"] = calibration_stats(behavior_rows)
    examples["tenant_pseudonym_count"] = len(tpm)
    examples["keepalive_strategies_configured"] = len(strategies)
    (out_dir / "examples.json").write_text(json.dumps(examples, indent=2, default=str))

    print("computing temporal drift ...", file=sys.stderr)
    drift = drift_report(behavior_rows, tpm)
    drift["subgroup_stability"] = subgroup_report(behavior_rows, tpm)
    drift["band_rate_tenant_mix_verification"] = verify_band_rate_drift(behavior_rows, tpm)
    (out_dir / "drift.json").write_text(json.dumps(drift, indent=2, default=str))

    print("plotting ...", file=sys.stderr)
    make_plots(drift, out_dir)

    print("privacy scan ...", file=sys.stderr)
    scan = privacy_scan(out_dir, raw_tenant_ids)
    (out_dir / "privacy_scan.json").write_text(json.dumps(scan, indent=2))
    print(f"privacy scan: {scan['files_scanned']} files scanned, "
          f"{'CLEAN' if scan['clean'] else 'HITS FOUND: ' + json.dumps(scan['hits'][:5])}",
          file=sys.stderr)

    print(f"wrote {out_dir}/examples.json, drift.json, plots/, privacy_scan.json",
          file=sys.stderr)
    return 0 if scan["clean"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
