#!/usr/bin/env python3
"""Correlate every catalogued kv-ttl feature against two things nobody in this line of
work has put in one table yet: (a) the TIME BUCKET of the next request, properly censored,
and (b) each live keep-alive STRATEGY's realized rescue outcome.

Extends, not repeats: reports3/feat-univariate.md already scored these 18 features against
a return-within-band hazard, prefix-compatibility, and a tuned dollar rule. This script asks
a different pair of questions -- which bucket does the wait land in, and does a feature's
apparent per-strategy effect survive once the strategy's own admin-configured time window is
accounted for (all 30 active strategies share idle_seconds=280/min_prefix_tokens=20000; only
their windows and tenant targets differ, so any BETWEEN-strategy contrast is a window/tenant
effect unless shown otherwise).

Reuses rather than reinvents: kv_ttl_cost_model (Request/derive/PriceBook/evaluate) and
kv_ttl_predictor_arms (History/bucket_of/stop_cluster/pseudonymize_map) -- the same tested
primitives every sibling script in this line of work shares. The richer per-request feature
frame (FRequest/attach_derived/CANON) is a fresh, compact implementation of the same 18
definitions reports3/feat-univariate.md already established (that script is WIP in a
sibling's un-merged worktree, so its definitions are followed rather than imported).

READ-ONLY: opens cg.db/cg-control.db with mode=ro only, never touches
/etc/context-guru/prices.yaml or the live service.

Usage:
    python3 kv_ttl_featcorr.py --db .../snap/cg.db --control-db .../snap/cg-control.db \
        --prices .../prices.local.yaml --out-json .../correlations.json \
        --out-plots .../plots --seed 0
"""
from __future__ import annotations

import argparse
import json
import math
import os
import sqlite3
import sys
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path
from urllib.parse import quote
from zoneinfo import ZoneInfo

import numpy as np

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from kv_ttl_cost_model import (  # noqa: E402
    Request, derive, UNRESCUABLE, DEFAULT_CACHE_READ_MULTIPLE, DEFAULT_WRITE_5M_MULTIPLE,
)
from kv_ttl_predictor_arms import (  # noqa: E402
    History, bucket_of, stop_cluster, pseudonymize_map,
)

DEFAULT_DB = "/home/vpcuser/cg-night-0928/snap/cg.db"
DEFAULT_CONTROL_DB = "/home/vpcuser/cg-night-0928/snap/cg-control.db"
IDLE_TRIGGER_MS = 280_000        # production ping trigger's own delay (kvcache default)
RESCUE_HORIZON_MS = 880_000      # max_pings=2 schedule's reach past the trigger (280+2*300)
BAND_LO_MS, BAND_HI_MS = 300_000, 3_600_000     # the "rescue band" (5m, 1h]
BREAK_EVEN_PCT = 100 * DEFAULT_CACHE_READ_MULTIPLE / (DEFAULT_WRITE_5M_MULTIPLE - DEFAULT_CACHE_READ_MULTIPLE)
TZ = ZoneInfo("Asia/Jerusalem")  # tenant.DefaultStrategyTZ; matches kv_ttl_keepalive_coverage.py
BRANCH_DROP_FRAC = 0.30

# ── the next-request-time buckets ────────────────────────────────────────────
# Every finite edge in milliseconds; BUCKETS[i] covers (EDGES[i-1], EDGES[i]] with the first
# starting at 0 and the last open-ended. "censored" is a 7th, genuinely distinct outcome: a
# row observed idle for less than the top edge with no successor yet, whose true bucket is
# unknowable from this window alone -- not a bug to paper over, an unresolved decision point.
EDGES_MS = [30_000, 300_000, 900_000, 3_600_000, 21_600_000]
BUCKET_NAMES = ["<30s", "30s-5m", "5m-15m", "15m-1h", "1h-6h", ">6h"]
CENSORED = "censored"
ALL_BUCKETS = BUCKET_NAMES + [CENSORED]


def hard_bucket(ms: float) -> str:
    for edge, name in zip(EDGES_MS, BUCKET_NAMES):
        if ms <= edge:
            return name
    return BUCKET_NAMES[-1]


def bucket_of_row(idle_ms: float | None, elapsed_to_window_end_ms: float) -> tuple[str, int]:
    """(bucket, event) where event=1 means idle_ms was actually observed (a successor
    arrived), event=0 means right-censored at elapsed_to_window_end_ms.

    A censored row whose own elapsed time already exceeds every finite edge is DEFINITIVELY
    in the top bucket -- it has already waited longer than any smaller bucket allows, whether
    or not it ever returns. One that hasn't yet is genuinely unresolved: it might still land
    in any bucket at or above its own elapsed time, or never return at all. That row gets the
    'censored' label rather than a guessed bucket -- exactly the "idle 3 days" vs "idle 4
    minutes at the window edge" distinction the brief asks to be handled explicitly.
    """
    if idle_ms is not None:
        return hard_bucket(idle_ms), 1
    if elapsed_to_window_end_ms >= EDGES_MS[-1]:
        return BUCKET_NAMES[-1], 0
    return CENSORED, 0


def in_rescue_band(idle_ms: float | None, elapsed_to_window_end_ms: float) -> float:
    """1.0 / 0.0 / NaN for the (BAND_LO_MS, BAND_HI_MS] outcome, NaN meaning unresolved.

    Resolved as: an observed return decides it outright; a censored row already idle past
    BAND_HI_MS definitely missed the band (it would already be a hit if it had landed inside
    it); anything else -- censored and still short of the band's own upper edge -- truly
    cannot be scored either way from this window and is dropped (never coerced to 0).
    """
    if idle_ms is not None:
        return 1.0 if BAND_LO_MS < idle_ms <= BAND_HI_MS else 0.0
    if elapsed_to_window_end_ms > BAND_HI_MS:
        return 0.0
    return float("nan")


def reached_trigger(idle_ms: float | None, elapsed_to_window_end_ms: float) -> bool:
    """Did this decision point ever actually reach the 280s ping trigger -- observed or,
    for a censored row, by having already been idle that long. This IS the population a
    ping would be sent on; every trigger-conditional rate in this file is denominated on it."""
    if idle_ms is not None:
        return idle_ms >= IDLE_TRIGGER_MS
    return elapsed_to_window_end_ms >= IDLE_TRIGGER_MS


def rescued_at_trigger(idle_ms: float | None, elapsed_to_window_end_ms: float) -> float:
    """Given the row reached the trigger, P(a successor arrives within RESCUE_HORIZON_MS of
    the trigger firing) -- 1/0/NaN(unresolved), same resolution convention as in_rescue_band."""
    if idle_ms is not None:
        return 1.0 if idle_ms <= RESCUE_HORIZON_MS else 0.0
    if elapsed_to_window_end_ms > RESCUE_HORIZON_MS:
        return 0.0
    return float("nan")


# ── data model ────────────────────────────────────────────────────────────────

@dataclass
class FRequest(Request):
    stop_reason: str = ""
    agent: str = ""
    cache_read: int = 0
    cache_write: int = 0
    tokens_before: int = 0
    tokens_after: int = 0
    saved_unique: int = 0
    tools: int = 0
    system_blocks: int = 0
    reasoning_effort: str = ""
    thinking_mode: str = ""
    thinking_budget: int = 0
    # filled by attach_derived(), strictly backward-safe
    turn_index: int = 0
    session_age_ms: int = 0
    prev_gap_ms: float | None = None
    prev_cache_read: int | None = None
    changed_prefix_shape: int | None = None
    branch_compaction_marker: int | None = None
    hist_p_band: float | None = None
    hist_n: int | None = None
    tool_recent_name: str | None = None
    digest_changed_lag1: int | None = None


def load_feature_rows(db_path: str) -> list[FRequest]:
    uri = f"file:{quote(str(Path(db_path).resolve()))}?mode=ro"
    con = sqlite3.connect(uri, uri=True)
    try:
        cols = ["id", "tenant_id", "session_id", "ts", "model", "fresh_input",
                "output_tokens", "cache_read", "cache_write", "cache_miss_reason",
                "upstream_ms", "cost_usd", "stop_reason", "agent", "tokens_before",
                "tokens_after", "saved_unique", "tools", "system_blocks",
                "reasoning_effort", "thinking_mode", "thinking_budget"]
        sql = (f"SELECT {', '.join(cols)} FROM requests WHERE session_id <> '' "
               f"AND token_accounting <> 'missing' AND keepalive = 0 ORDER BY ts, id")
        rows = [
            FRequest(request_id=r[0], user=r[1], conversation=r[2], ts_ms=r[3], model=r[4],
                     input_tokens=r[5] or 0, output_tokens=r[6] or 0,
                     cached_context=(r[7] or 0) + (r[8] or 0), miss_reason=r[9] or "",
                     hit=(r[9] or "") == "hit", upstream_ms=r[10] or 0.0, billed_usd=r[11] or 0.0,
                     stop_reason=r[12] or "", agent=r[13] or "", cache_read=r[7] or 0,
                     cache_write=r[8] or 0, tokens_before=r[14] or 0, tokens_after=r[15] or 0,
                     saved_unique=r[16] or 0, tools=r[17] or 0, system_blocks=r[18] or 0,
                     reasoning_effort=r[19] or "", thinking_mode=r[20] or "",
                     thinking_budget=r[21] or 0)
            for r in con.execute(sql, [])
        ]
    finally:
        con.close()
    return derive(rows)


def load_tool_uses(db_path: str) -> dict[tuple[str, str], list[tuple[int, str]]]:
    uri = f"file:{quote(str(Path(db_path).resolve()))}?mode=ro"
    con = sqlite3.connect(uri, uri=True)
    try:
        out: dict[tuple[str, str], list[tuple[int, str]]] = {}
        for tenant, session, name, last_ts in con.execute(
                "SELECT tenant_id, session_id, name, last_ts FROM tool_uses"):
            out.setdefault((tenant, session), []).append((last_ts, name))
        for v in out.values():
            v.sort()
        return out
    finally:
        con.close()


def load_digest_transitions(db_path: str) -> dict[tuple[str, str], list[tuple[int, str]]]:
    uri = f"file:{quote(str(Path(db_path).resolve()))}?mode=ro"
    con = sqlite3.connect(uri, uri=True)
    try:
        out: dict[tuple[str, str], list[tuple[int, str]]] = {}
        for tenant, session, ts, digest in con.execute(
                "SELECT DISTINCT tenant_id, session_id, ts, digest FROM tool_declarations"):
            out.setdefault((tenant, session), []).append((ts, digest))
        for v in out.values():
            v.sort()
        return out
    finally:
        con.close()


def attach_derived(rows: list[FRequest], tool_uses: dict, digest_tr: dict) -> list[FRequest]:
    """One chronological pass per conversation; everything computed strictly from rows at
    or before the current one (the study's backwards-only rule)."""
    by_conv: dict[tuple[str, str, str], list[FRequest]] = {}
    for r in rows:
        by_conv.setdefault(r.key, []).append(r)
    for key, group in by_conv.items():
        group.sort(key=lambda r: (r.ts_ms, r.request_id))
        first_ts = group[0].ts_ms
        hist = History()
        last_hour_utc = None
        tu = tool_uses.get((key[0], key[1]), [])
        dtr = digest_tr.get((key[0], key[1]), [])
        di = 0
        prev: FRequest | None = None
        for i, r in enumerate(group):
            r.turn_index = i + 1
            r.session_age_ms = r.ts_ms - first_ts
            if prev is not None:
                r.prev_gap_ms = (float(prev.idle_ms) if prev.idle_ms is not None
                                else float(r.ts_ms - prev.ts_ms))
                r.prev_cache_read = prev.cache_read
                r.changed_prefix_shape = int(
                    r.tools != prev.tools or r.system_blocks != prev.system_blocks
                    or r.model != prev.model or r.reasoning_effort != prev.reasoning_effort
                    or r.thinking_mode != prev.thinking_mode
                    or r.thinking_budget != prev.thinking_budget)
                if prev.tokens_after > 0:
                    r.branch_compaction_marker = int(
                        r.tokens_before < prev.tokens_after * (1 - BRANCH_DROP_FRAC))
            if prev is not None and last_hour_utc is not None:
                gap_s = max(0.0, (r.ts_ms - prev.ts_ms) / 1000.0)
                hist.observe(r.user, r.model, bucket_of(last_hour_utc), gap_s)
            p, n = hist.reuse_within(r.user, r.model, bucket_of(r.hour_utc), 300.0)
            r.hist_p_band, r.hist_n = p, n
            name, best_last = None, -1
            for last_ts_t, nm in tu:
                if last_ts_t < r.ts_ms and last_ts_t > best_last:
                    best_last, name = last_ts_t, nm
            r.tool_recent_name = name or "(none yet)"
            while di < len(dtr) and dtr[di][0] < r.ts_ms:
                di += 1
            if di >= 2:
                r.digest_changed_lag1 = int(dtr[di - 2][1] != dtr[di - 1][1])
            elif di == 1:
                r.digest_changed_lag1 = 0
            prev = r
            last_hour_utc = r.hour_utc
    return rows


def weekday_of(ts_ms: int) -> str:
    return datetime.fromtimestamp(ts_ms / 1000.0, tz=timezone.utc).strftime("%a")


# ── the 18 established features (reports3/feat-univariate.md's own catalogue), plus
# tenant identity -- referenced constantly by the strategy analysis, not one of the 18 ────

CANON = [
    dict(id="gap_since_last_ms", kind="numeric", feature_of=lambda r: r.prev_gap_ms),
    dict(id="return_probability_band_hazard", kind="numeric", feature_of=lambda r: r.hist_p_band),
    dict(id="weekday", kind="categorical", feature_of=lambda r: weekday_of(r.ts_ms)),
    dict(id="hour_of_day", kind="categorical", feature_of=lambda r: r.hour_utc),
    dict(id="cold_start_pooling_n", kind="numeric", feature_of=lambda r: r.hist_n),
    dict(id="agent", kind="categorical", feature_of=lambda r: r.agent),
    dict(id="model", kind="categorical", feature_of=lambda r: r.model),
    dict(id="turn_index", kind="numeric", feature_of=lambda r: r.turn_index),
    dict(id="stop_reason", kind="categorical", feature_of=lambda r: r.stop_reason or "(none)"),
    dict(id="session_age_ms", kind="numeric", feature_of=lambda r: r.session_age_ms),
    dict(id="context_window_headroom", kind="numeric", feature_of=lambda r: r.tokens_before),
    dict(id="tool_recent_name", kind="categorical", feature_of=lambda r: r.tool_recent_name),
    dict(id="prefix_size", kind="numeric", feature_of=lambda r: r.cached_context),
    dict(id="changed_tools_system_model_thinking", kind="categorical",
        feature_of=lambda r: r.changed_prefix_shape),
    dict(id="per_prefix_digest_change", kind="categorical",
        feature_of=lambda r: r.digest_changed_lag1),
    dict(id="branch_compaction_marker", kind="categorical",
        feature_of=lambda r: r.branch_compaction_marker),
    dict(id="cache_read_write_prev", kind="numeric", feature_of=lambda r: r.prev_cache_read),
    dict(id="expected_reusable_tokens", kind="numeric", feature_of=lambda r: r.saved_unique),
    dict(id="tenant", kind="categorical", feature_of=lambda r: r.user),  # replaced by pseudonym below
]


# ── stats helpers (self-contained; scipy/sklearn only, no lifelines dependency for the
# core measures -- see the KM sensitivity check for the one place lifelines adds value) ──

def wilson_ci(k: int, n: int, z: float = 1.96) -> tuple[float, float]:
    if n == 0:
        return (float("nan"), float("nan"))
    p = k / n
    denom = 1 + z * z / n
    centre = p + z * z / (2 * n)
    half = z * math.sqrt(p * (1 - p) / n + z * z / (4 * n * n))
    return ((centre - half) / denom, (centre + half) / denom)


def bh_fdr(pvalues: list[float]) -> list[float]:
    idx = [i for i, p in enumerate(pvalues) if p is not None and not math.isnan(p)]
    m = len(idx)
    out = [float("nan")] * len(pvalues)
    if m == 0:
        return out
    ranked = sorted(idx, key=lambda i: pvalues[i])
    prev = 1.0
    for rank, i in enumerate(reversed(ranked), start=1):
        j = m - rank + 1
        q = min(prev, pvalues[i] * m / j)
        prev = q
        out[i] = q
    return out


def bootstrap_auc_by_conv(x: np.ndarray, y: np.ndarray, conv_keys: list, *, reps: int,
                           seed: int) -> dict:
    from sklearn.metrics import roc_auc_score
    if len(x) < 20 or len(set(y.tolist())) < 2:
        return {"auc": None, "ci95": [None, None], "n": len(x)}
    point = float(roc_auc_score(y, x))
    by_key: dict = {}
    for k, xi, yi in zip(conv_keys, x, y):
        by_key.setdefault(k, []).append((xi, yi))
    keys = list(by_key.keys())
    rng = np.random.default_rng(seed)
    reps_out = []
    for _ in range(reps):
        idx = rng.integers(0, len(keys), size=len(keys))
        xs, ys = [], []
        for i in idx:
            for xi, yi in by_key[keys[i]]:
                xs.append(xi)
                ys.append(yi)
        ys = np.asarray(ys)
        if len(set(ys.tolist())) < 2:
            continue
        reps_out.append(roc_auc_score(ys, np.asarray(xs)))
    if not reps_out:
        return {"auc": point, "ci95": [None, None], "n": int(len(x))}
    lo, hi = np.percentile(reps_out, [2.5, 97.5])
    return {"auc": point, "ci95": [float(lo), float(hi)], "n": int(len(x))}


def spearman(x: np.ndarray, y: np.ndarray) -> tuple[float, float]:
    from scipy.stats import spearmanr
    r, p = spearmanr(x, y)
    return float(r), float(p)


def cox_univariate(duration: np.ndarray, event: np.ndarray, covariate: np.ndarray) -> dict:
    """One-covariate Cox PH fit via lifelines: the censoring-correct way to ask "does a
    higher feature value associate with a faster/slower return", using every row (event and
    censored alike) rather than the event-only subset Spearman is stuck with. Reported
    alongside Spearman, not instead of it, because the PH assumption itself is known to be
    violated globally in this corpus (pred-families, night0928/pred-families) -- the
    coefficient's SIGN and significance are still informative, its exact hazard-ratio
    magnitude should be read as approximate.
    """
    import pandas as pd
    from lifelines import CoxPHFitter
    from lifelines.exceptions import ConvergenceError
    df = pd.DataFrame({"T": duration / 1000.0, "E": event, "x": covariate}).dropna()
    df = df[df["T"] > 0]
    if len(df) < 50 or df["x"].nunique() < 2:
        return {"n": int(len(df)), "coef": None, "p": None, "reason": "insufficient data"}
    # Scale the covariate to unit variance so the coefficient is comparable across features
    # of wildly different native scales (milliseconds vs token counts vs small integers).
    sd = df["x"].std()
    if not sd or math.isnan(sd) or sd == 0:
        return {"n": int(len(df)), "coef": None, "p": None, "reason": "zero variance"}
    df["x"] = (df["x"] - df["x"].mean()) / sd
    try:
        cph = CoxPHFitter()
        cph.fit(df, duration_col="T", event_col="E")
        s = cph.summary
        return {"n": int(len(df)), "coef": float(s.loc["x", "coef"]),
                "hazard_ratio": float(s.loc["x", "exp(coef)"]), "p": float(s.loc["x", "p"]),
                "note": "coef is per one SD of the feature; positive = faster return (higher hazard)"}
    except (ConvergenceError, np.linalg.LinAlgError) as e:
        return {"n": int(len(df)), "coef": None, "p": None, "reason": f"did not converge: {e}"}


def km_bucket_probs(duration_ms: np.ndarray, event: np.ndarray) -> dict:
    """Kaplan-Meier estimate of P(bucket), censoring-corrected, as a sensitivity check
    against the naive discrete table (which reports 'censored' as its own terminal bucket).
    S(edge_i) - S(edge_{i-1}) is the KM-consistent share that would fall in that bucket if
    every censored row eventually resolved -- read this as 'what the distribution would look
    like with infinite patience', not a replacement for the primary, honestly-censored table.
    """
    from lifelines import KaplanMeierFitter
    kmf = KaplanMeierFitter()
    kmf.fit(duration_ms / 1000.0, event_observed=event)
    edges_s = [e / 1000.0 for e in EDGES_MS]
    s_at = [1.0] + [float(kmf.predict(e)) for e in edges_s]  # S(0)=1, S(edge_0), ..., S(edge_4)
    probs = {}
    for i in range(len(EDGES_MS)):  # BUCKET_NAMES[0..4]: the 5 finite-width buckets
        probs[BUCKET_NAMES[i]] = max(0.0, s_at[i] - s_at[i + 1])
    probs[BUCKET_NAMES[-1]] = s_at[-1]  # ">6h": KM's own tail past the last edge
    return probs


# ── active strategies (cg-control.db) ────────────────────────────────────────

def load_active_strategies(control_db: str) -> list[dict]:
    """One dict per (strategy id, tenant) pair. All 30 active strategies in this snapshot
    share idle_seconds=280/min_prefix_tokens=20000 (verified directly, COORDINATION.md); kept
    as read fields rather than assumed, so a future snapshot that isn't uniform is visible
    rather than silently mis-scored."""
    con = sqlite3.connect(f"file:{quote(str(Path(control_db).resolve()))}?mode=ro", uri=True)
    try:
        rows = con.execute(
            "SELECT id, max_pings, idle_seconds, min_prefix_tokens, target_json, windows_json "
            "FROM keepalive_strategies WHERE active=1").fetchall()
    finally:
        con.close()
    out = []
    for sid, max_pings, idle_s, min_prefix, target_json, windows_json in rows:
        target = json.loads(target_json)
        windows = json.loads(windows_json)
        if target.get("mode") != "list":
            continue  # none observed in this snapshot; a broad "all" strategy would need a
                      # different membership test this file does not implement
        for tid in target.get("tenant_ids", []):
            out.append({"strategy_id": sid, "tenant_id": tid, "max_pings": max_pings,
                       "idle_seconds": idle_s, "min_prefix_tokens": min_prefix,
                       "windows": windows})
    return out


def in_window(ts_ms: int, windows: list[dict]) -> bool:
    local = datetime.fromtimestamp(ts_ms / 1000, tz=TZ)
    py_to_go_dow = (local.weekday() + 1) % 7  # Go: Sun=0..Sat=6; Python: Mon=0..Sun=6
    hm = local.strftime("%H:%M")
    for w in windows:
        if py_to_go_dow in w.get("days", list(range(7))) and w["start"] <= hm < w["end"]:
            return True
    return False


# ── per-feature analysis ─────────────────────────────────────────────────────

def auc_point(x: np.ndarray, y: np.ndarray) -> float | None:
    from sklearn.metrics import roc_auc_score
    if len(x) < 20 or len(set(y.tolist())) < 2:
        return None
    try:
        return float(roc_auc_score(y, x))
    except ValueError:
        return None


def category_encode(values: np.ndarray, outcome: np.ndarray, *, min_n: int = 10) -> np.ndarray:
    """In-sample category -> mean(outcome) encoding, the standard way to score an
    unordered categorical feature with rank-based measures (AUC, Spearman) built for numeric
    ones. In-sample and therefore optimistic -- disclosed everywhere this is used, exactly
    like a univariate chi-square test is 'in-sample' in the same sense."""
    import pandas as pd
    df = pd.DataFrame({"cat": values, "y": outcome})
    means = df.groupby("cat")["y"].transform(lambda s: s.mean() if len(s) >= min_n else np.nan)
    return means.to_numpy()


def numeric_bins_ci(feature: np.ndarray, outcome: np.ndarray, *, max_bins: int = 8) -> list[dict]:
    import pandas as pd
    sub = pd.DataFrame({"x": feature, "y": outcome}).dropna()
    if len(sub) < 20:
        return []
    try:
        cats = pd.qcut(sub["x"], q=max_bins, duplicates="drop")
    except ValueError:
        return []
    out = []
    for interval, grp in sub.groupby(cats, observed=True):
        n, k = len(grp), int(grp["y"].sum())
        lo, hi = wilson_ci(k, n)
        out.append({"bin": str(interval), "n": n, "k": k, "rate": k / n if n else None,
                   "ci95": [lo, hi]})
    return out


def analyze_feature_buckets(feat_id: str, kind: str, feature_of, rows: list[FRequest],
                            conv_keys: list, buckets: np.ndarray, durations_ms: np.ndarray,
                            events: np.ndarray, in_band: np.ndarray, *, reps: int,
                            seed: int) -> dict:
    """One feature's association with the next-request-time bucket family: numeric
    (Spearman + censoring-aware Cox + binned in-band rate curve) or categorical (per-category
    bucket rates + a chi-square contingency test), plus a single-feature AUC for landing in
    the rescue band, uniformly for both kinds via the in-sample category encoding for the
    categorical case."""
    raw = np.array([feature_of(r) for r in rows], dtype=object)
    if kind == "numeric":
        x = np.array([float(v) if v is not None and not (isinstance(v, float) and math.isnan(v))
                     else np.nan for v in raw])
        present = ~np.isnan(x)
    else:
        x = raw
        present = np.array([v is not None for v in raw])
    n_present = int(present.sum())
    out: dict = {"id": feat_id, "kind": kind, "n_total": len(rows),
                "n_present": n_present,
                "n_missing_pct": 100 * (1 - n_present / len(rows)) if rows else None}
    if n_present < 30:
        out["skipped"] = "fewer than 30 non-missing rows"
        return out

    # -- per-outcome-bucket table (every row lands in exactly one of the 7 buckets; no NaN
    #    here by construction, so denominators below are always the full present-feature n) --
    if kind == "categorical":
        import pandas as pd
        cat = pd.Series(x[present]).astype(str)
        buck = pd.Series(buckets[present])
        counts = cat.value_counts()
        top_cats = counts.index[:20].tolist()
        table = []
        for c in top_cats:
            n_c = int((cat == c).sum())
            row = {"category": c, "n": n_c, "buckets": {}}
            for b in ALL_BUCKETS:
                k = int(((cat == c) & (buck == b)).sum())
                lo, hi = wilson_ci(k, n_c)
                row["buckets"][b] = {"k": k, "rate": k / n_c if n_c else None, "ci95": [lo, hi]}
            table.append(row)
        out["bucket_table"] = table
        # contingency test on categories clearing min_n=30, collapsing the long tail into
        # "(other)" rather than dropping it, so the test's own n matches n_present exactly.
        keep = counts.index[counts >= 30][:20]
        if len(keep) >= 2:
            cat_grouped = cat.where(cat.isin(keep), other="(other)")
            ct = pd.crosstab(cat_grouped, buck)
            ct = ct.loc[:, (ct.sum(axis=0) > 0)]
            if ct.shape[0] >= 2 and ct.shape[1] >= 2:
                from scipy.stats import chi2_contingency
                chi2, p, dof, _ = chi2_contingency(ct.values)
                out["contingency_test"] = {"chi2": float(chi2), "dof": int(dof), "p": float(p),
                                          "n": int(ct.values.sum()),
                                          "categories_tested": ct.shape[0]}
            else:
                out["contingency_test"] = {"p": None, "reason": "fewer than 2x2 after grouping"}
        else:
            out["contingency_test"] = {"p": None, "reason": "no category clears min_n=30"}
        assoc_p = out["contingency_test"].get("p")
        # AUC score input: in-sample category-rate encoding against the rescue-band outcome.
        enc = category_encode(x[present], in_band[present])
    else:
        # numeric: Spearman on the event-only (uncensored) subset -- disclosed limitation --
        # plus a Cox PH univariate fit on every present row, censored ones included properly.
        ev_mask = present & (events == 1)
        rho, sp_p = (float("nan"), float("nan"))
        if ev_mask.sum() >= 30:
            rho, sp_p = spearman(x[ev_mask], durations_ms[ev_mask])
        out["spearman_vs_gap"] = {"rho": rho, "p": sp_p, "n": int(ev_mask.sum()),
                                  "population": "event-observed rows only (excludes censored); "
                                                "vs the raw gap to the next request in ms"}
        cox = cox_univariate(durations_ms[present], events[present], x[present])
        out["cox_ph_univariate"] = cox
        assoc_p = cox.get("p")
        out["binned_rate_curve"] = numeric_bins_ci(x[present], in_band[present], max_bins=8)
        enc = x[present]

    out["p_value"] = assoc_p
    y_band = in_band[present]
    enc = np.asarray(enc, dtype=float)
    # Categorical encoding can itself be NaN (a category too rare to encode, min_n=10 in
    # category_encode) even where the raw feature value is present -- both that and an
    # unresolved bucket outcome must be excluded before scoring, or sklearn's own all-finite
    # check throws rather than silently dropping the row for us.
    resolved = ~np.isnan(y_band) & ~np.isnan(enc)
    conv_present = [conv_keys[i] for i in range(len(rows)) if present[i]]
    conv_resolved = [c for c, r in zip(conv_present, resolved) if r]
    auc_ci = bootstrap_auc_by_conv(enc[resolved].astype(float), y_band[resolved].astype(float),
                                   conv_resolved, reps=reps, seed=seed)
    out["auc_rescue_band"] = auc_ci
    out["auc_rescue_band"]["population"] = ("resolved rows only: an observed return decides "
                                           "it, or a censored row already idle past the "
                                           "band's own 1h upper edge counts as a definite "
                                           "miss; rows censored short of that edge are "
                                           "dropped as unresolved, never coerced to 0")
    return out


def heatmap_cell(x: np.ndarray, present: np.ndarray, bucket_indicator: np.ndarray) -> float | None:
    """None means untestable (too few rows / one-class outcome); 0.0 is a real, reportable
    'no rank signal' result and must not collapse into the same None the caller then maps to
    NaN -- `0.0 or np.nan` would do exactly that, since 0.0 is falsy in Python."""
    a = auc_point(x[present].astype(float), bucket_indicator[present].astype(float))
    return None if a is None else abs(a - 0.5) * 2


# ── plotting ──────────────────────────────────────────────────────────────────
# Static SVGs (this is an offline replay report, not a web artifact): one sequential blue
# hue for magnitude (heatmaps), one fixed categorical hue for the bar/AUC chart, recessive
# gridlines, explicit units and denominators on every axis, uncertainty shown as error bars.

SEQ_HUE = "#2b6cb0"      # single-hue sequential, light->dark via alpha
BAR_HUE = "#2b6cb0"
GRID_GRAY = "#d9dde3"
TEXT_GRAY = "#4a5568"


def _save_caption(path: Path, text: str) -> None:
    path.write_text(text, encoding="utf-8")


def plot_heatmap(features: list[str], matrix: np.ndarray, out_dir: Path) -> None:
    import matplotlib
    matplotlib.use("Agg")
    import matplotlib.pyplot as plt
    fig, ax = plt.subplots(figsize=(9, max(4, 0.35 * len(features))))
    im = ax.imshow(matrix, cmap="Blues", vmin=0, vmax=1, aspect="auto")
    ax.set_xticks(range(len(ALL_BUCKETS)))
    ax.set_xticklabels(ALL_BUCKETS, rotation=30, ha="right", color=TEXT_GRAY)
    ax.set_yticks(range(len(features)))
    ax.set_yticklabels(features, color=TEXT_GRAY, fontsize=8)
    ax.set_xlabel("next-request-time bucket")
    ax.set_title("Feature x bucket association strength\n"
                 "|AUC - 0.5| x 2 (0=no rank signal, 1=perfect separation); "
                 "categorical features via in-sample category-rate encoding")
    for spine in ax.spines.values():
        spine.set_visible(False)
    cb = fig.colorbar(im, ax=ax, shrink=0.7)
    cb.set_label("association strength")
    fig.tight_layout()
    fig.savefig(out_dir / "feature_bucket_heatmap.svg")
    plt.close(fig)
    _save_caption(out_dir / "feature_bucket_heatmap.txt",
                 "Feature x bucket association-strength heatmap. Each cell is "
                 "|AUC-0.5|x2 for that feature ranking whether a decision point's "
                 "resolved next-request-time bucket equals that column, over all rows "
                 "with the feature present (n stated per feature in correlations.json). "
                 "0 = no rank signal, 1 = perfect separation. Categorical features are "
                 "scored via an in-sample category-mean encoding, disclosed as optimistic. "
                 "'censored' is a real 7th bucket: a decision point still idle, with no "
                 "successor yet, at less than the 6-hour edge -- not an artifact to ignore.")


def plot_numeric_curves(results: list[dict], out_dir: Path) -> None:
    import matplotlib
    matplotlib.use("Agg")
    import matplotlib.pyplot as plt
    numeric = [r for r in results if r["kind"] == "numeric" and r.get("binned_rate_curve")]
    if not numeric:
        return
    ncols = 3
    nrows = math.ceil(len(numeric) / ncols)
    fig, axes = plt.subplots(nrows, ncols, figsize=(4 * ncols, 3 * nrows), squeeze=False)
    for i, r in enumerate(numeric):
        ax = axes[i // ncols][i % ncols]
        curve = r["binned_rate_curve"]
        xs = list(range(len(curve)))
        rates = [c["rate"] for c in curve]
        los = [c["ci95"][0] for c in curve]
        his = [c["ci95"][1] for c in curve]
        yerr = [[max(0, rt - lo) for rt, lo in zip(rates, los)],
               [max(0, hi - rt) for rt, hi in zip(rates, his)]]
        ax.errorbar(xs, rates, yerr=yerr, fmt="o-", color=SEQ_HUE, ecolor=SEQ_HUE,
                   markersize=4, linewidth=1.5, capsize=2)
        ax.set_title(r["id"], fontsize=9, color=TEXT_GRAY)
        ax.set_xticks(xs)
        ax.set_xticklabels([str(i + 1) for i in xs], fontsize=7)
        ax.set_xlabel("feature decile (1=lowest)", fontsize=7)
        ax.set_ylabel("P(rescue band | resolved)", fontsize=7)
        ax.set_ylim(-0.02, 1.02)
        ax.grid(color=GRID_GRAY, linewidth=0.6)
        for spine in ax.spines.values():
            spine.set_visible(False)
        n_total = sum(c["n"] for c in curve)
        ax.annotate(f"n={n_total:,}", xy=(0.02, 0.92), xycoords="axes fraction", fontsize=6,
                   color=TEXT_GRAY)
    for j in range(len(numeric), nrows * ncols):
        axes[j // ncols][j % ncols].axis("off")
    fig.suptitle("Per-feature bucket-rate curves: P(next request lands in the rescue "
                "band, 5m-1h] by feature decile, with 95% Wilson CIs", fontsize=10)
    fig.tight_layout()
    fig.savefig(out_dir / "numeric_bucket_rate_curves.svg")
    plt.close(fig)
    _save_caption(out_dir / "numeric_bucket_rate_curves.txt",
                 "Small multiples, one per numeric feature: the feature is split into "
                 "decile bins (equal-count, ties collapsed), and each point is the "
                 "fraction of RESOLVED decision points in that bin whose next request "
                 "landed in the rescue band (idle time in (5 minutes, 1 hour]), with a "
                 "95% Wilson CI. 'Resolved' excludes decision points still idle, with no "
                 "successor yet, for less than 1 hour -- their eventual bucket is "
                 "unknown and they are dropped rather than counted as a miss. n per panel "
                 "is the count of resolved rows across all its bins, stated inline.")


def plot_auc_ranking(results: list[dict], out_dir: Path) -> None:
    import matplotlib
    matplotlib.use("Agg")
    import matplotlib.pyplot as plt
    rows = [(r["id"], r["auc_rescue_band"]) for r in results
           if r.get("auc_rescue_band", {}).get("auc") is not None]
    rows.sort(key=lambda t: t[1]["auc"])
    fig, ax = plt.subplots(figsize=(7, max(3, 0.4 * len(rows))))
    ys = range(len(rows))
    aucs = [t[1]["auc"] for t in rows]
    los = [t[1]["ci95"][0] if t[1]["ci95"][0] is not None else t[1]["auc"] for t in rows]
    his = [t[1]["ci95"][1] if t[1]["ci95"][1] is not None else t[1]["auc"] for t in rows]
    xerr = [[a - lo for a, lo in zip(aucs, los)], [hi - a for a, hi in zip(aucs, his)]]
    ax.errorbar(aucs, ys, xerr=xerr, fmt="o", color=BAR_HUE, ecolor=BAR_HUE, markersize=5,
               capsize=3)
    ax.axvline(0.5, color=TEXT_GRAY, linewidth=1, linestyle="--")
    ax.set_yticks(list(ys))
    ax.set_yticklabels([t[0] for t in rows], fontsize=8, color=TEXT_GRAY)
    ax.set_xlabel("AUC for landing in the rescue band (5m-1h], resolved rows only, "
                 "95% CI (conversation-level bootstrap)")
    ax.set_xlim(0.3, 1.0)
    ax.grid(axis="x", color=GRID_GRAY, linewidth=0.6)
    for n, spine in ax.spines.items():
        if n != "left":
            spine.set_visible(False)
    ax.set_title("Ranked single-feature AUC, every catalogued feature")
    fig.tight_layout()
    fig.savefig(out_dir / "auc_ranking.svg")
    plt.close(fig)
    ns = [t[1]["n"] for t in rows]
    _save_caption(out_dir / "auc_ranking.txt",
                 f"Ranked single-feature AUC for predicting whether a decision point's "
                 f"next request lands in the rescue band, one feature per row, dashed "
                 f"line at 0.5 (no rank signal). Error bars are 95% CIs from a "
                 f"conversation-level bootstrap. Population is resolved rows only per "
                 f"feature (n ranges {min(ns):,}-{max(ns):,} across features, see "
                 f"correlations.json for the exact n behind each bar). Categorical "
                 f"features are scored via an in-sample category-rate encoding, which is "
                 f"optimistic and stated as such.")


def plot_strategy_matrix(feature_ids: list[str], strategy_labels: list[str],
                         matrix: np.ndarray, n_matrix: np.ndarray, out_dir: Path) -> None:
    import matplotlib
    matplotlib.use("Agg")
    import matplotlib.pyplot as plt
    masked = np.ma.masked_invalid(matrix)
    fig, ax = plt.subplots(figsize=(max(8, 0.3 * len(strategy_labels)),
                                    max(4, 0.35 * len(feature_ids))))
    cmap = plt.get_cmap("Blues").copy()
    cmap.set_bad(color="#f0f1f3")
    im = ax.imshow(masked, cmap=cmap, vmin=0, vmax=1, aspect="auto")
    ax.set_xticks(range(len(strategy_labels)))
    ax.set_xticklabels(strategy_labels, rotation=90, fontsize=6, color=TEXT_GRAY)
    ax.set_yticks(range(len(feature_ids)))
    ax.set_yticklabels(feature_ids, fontsize=8, color=TEXT_GRAY)
    ax.set_xlabel("active strategy (tenant, one column per campaign; all share "
                 "idle_seconds=280/min_prefix_tokens=20000)")
    ax.set_title("Feature x strategy: |Spearman rho| vs that strategy's realized rescue "
                "outcome, within its gated population (blank = n<30, untested)")
    for spine in ax.spines.values():
        spine.set_visible(False)
    cb = fig.colorbar(im, ax=ax, shrink=0.7)
    cb.set_label("|rho| (in-window)")
    fig.tight_layout()
    fig.savefig(out_dir / "feature_strategy_matrix.svg")
    plt.close(fig)
    n_tested = int(np.isfinite(matrix).sum())
    _save_caption(out_dir / "feature_strategy_matrix.txt",
                 f"Feature x active-strategy association matrix. Each cell is the "
                 f"absolute Spearman correlation between the feature (or, for "
                 f"categorical features, its population-level category rescue-rate "
                 f"encoding) and whether that decision point was rescued within the "
                 f"280s-trigger/max-two-pings schedule, computed ONLY on the rows that "
                 f"specific strategy's own tenant-and-window gate would actually cover. "
                 f"Blank cells are strategy x feature pairs with fewer than 30 gated, "
                 f"resolved rows -- too small to test, not zero. {n_tested} of "
                 f"{matrix.size} cells were testable. All 30 strategies share "
                 f"idle_seconds=280 and min_prefix_tokens=20000; the only thing that "
                 f"differs between columns is which tenant and which weekly window is "
                 f"targeted, so a column-to-column difference in this matrix reflects "
                 f"tenant/window, not the strategy's own configuration.")


# ── main ──────────────────────────────────────────────────────────────────────

def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--db", default=DEFAULT_DB)
    ap.add_argument("--control-db", default=DEFAULT_CONTROL_DB)
    ap.add_argument("--out-json", required=True)
    ap.add_argument("--out-plots", required=True)
    ap.add_argument("--bootstrap-reps", type=int, default=200)
    ap.add_argument("--seed", type=int, default=0)
    ap.add_argument("--min-strategy-n", type=int, default=30)
    ap.add_argument("--limit", type=int, default=None,
                    help="use only the first N chronological rows (smoke-testing only)")
    args = ap.parse_args(argv)

    plots_dir = Path(args.out_plots)
    plots_dir.mkdir(parents=True, exist_ok=True)

    print(f"loading {args.db} ...", file=sys.stderr)
    rows = load_feature_rows(args.db)
    tool_uses = load_tool_uses(args.db)
    digest_tr = load_digest_transitions(args.db)
    rows = attach_derived(rows, tool_uses, digest_tr)
    if args.limit:
        rows = rows[:args.limit]
    window_end_ms = max(r.ts_ms for r in rows)
    n_total = len(rows)
    n_conversations = len({r.key for r in rows})
    print(f"{n_total:,} decision points, {n_conversations:,} conversations", file=sys.stderr)

    pseudo = {t: f"T{i+1:02d}" for i, t in enumerate(sorted({r.user for r in rows}))}
    for r in rows:
        r.__dict__["_pseudo_tenant"] = pseudo[r.user]

    # -- per-row outcome arrays, computed once --
    durations_ms = np.empty(n_total)
    events = np.empty(n_total, dtype=int)
    buckets = np.empty(n_total, dtype=object)
    in_band = np.empty(n_total)
    reached_trig = np.empty(n_total, dtype=bool)
    rescued = np.empty(n_total)
    conv_keys = [r.key for r in rows]
    for i, r in enumerate(rows):
        elapsed = window_end_ms - r.ts_ms
        durations_ms[i] = r.idle_ms if r.idle_ms is not None else elapsed
        events[i] = 1 if r.idle_ms is not None else 0
        buckets[i], _ = bucket_of_row(r.idle_ms, elapsed)
        in_band[i] = in_rescue_band(r.idle_ms, elapsed)
        reached_trig[i] = reached_trigger(r.idle_ms, elapsed)
        rescued[i] = rescued_at_trigger(r.idle_ms, elapsed) if reached_trig[i] else float("nan")

    # -- reference numbers, reproduced as a pipeline sanity check (team-lead's brief: verify
    #    before trusting anything downstream). Reported either way, not gated on matching. --
    n_first_of_conv = sum(1 for r in rows if r.prev_gap_ms is None)
    n_eligible_gap = n_total - n_first_of_conv
    n_resolved_band = int((~np.isnan(in_band)).sum())
    band_rate_full = float(np.nanmean(in_band))
    cut_idx = int(n_total * 0.6)
    ts_sorted = sorted(r.ts_ms for r in rows)
    cut_ts = ts_sorted[cut_idx] if cut_idx < n_total else ts_sorted[-1]
    last40_mask = np.array([r.ts_ms >= cut_ts for r in rows])
    band_rate_last40 = float(np.nanmean(in_band[last40_mask]))
    n_trigger = int(reached_trig.sum())
    # "a later request eventually arrives" is unbounded in time -- events==1, not the
    # RESCUE_HORIZON_MS-bounded `rescued_at_trigger`. Conflating the two was this script's
    # own first bug, caught against the exact reference figure below (23,800 matched;
    # 4,518/19,276 did not, because those were 880s-bounded, not "ever" un-bounded).
    n_trig_returns = int((events[reached_trig] == 1).sum())
    n_trig_never = int((events[reached_trig] == 0).sum())
    reference_check = {
        "eligible_decision_points_full_window": {"value": n_eligible_gap,
            "population": "rows with a defined previous gap (not the first row of their "
                          "conversation)", "reference_value": 246484},
        "rescue_band_rate_full_window": {"value": band_rate_full, "n": n_resolved_band,
            "population": "resolved rows only (idle observed, or censored past the 1h band "
                          "edge)", "reference_value_pct": 2.60},
        "rescue_band_rate_last_40pct": {"value": band_rate_last40,
            "n": int((~np.isnan(in_band[last40_mask])).sum()),
            "population": "same, restricted to rows with ts >= the 60th-percentile "
                          "timestamp", "reference_value_pct": 3.65},
        "spans_reaching_280s_trigger": {"value": n_trigger, "reference_value": 23800},
        "of_those_a_later_request_arrives": {"value": n_trig_returns, "reference_value": 8391},
        "of_those_never_another_request": {"value": n_trig_never, "reference_value": 15409},
    }
    print("reference check:", json.dumps(reference_check, indent=2, default=str), file=sys.stderr)

    # -- KM sensitivity check on the aggregate population --
    km_probs = km_bucket_probs(durations_ms, events)

    # -- per-feature bucket-family analysis --
    canon = []
    for spec in CANON:
        fo = spec["feature_of"]
        if spec["id"] == "tenant":
            fo = lambda r: pseudo[r.user]  # noqa: B023 -- reassigned per-iteration deliberately
        canon.append({**spec, "feature_of": fo})

    reps, seed = args.bootstrap_reps, args.seed
    feature_results = []
    pvalue_registry = []  # (family, feat_id, extra, p)
    for spec in canon:
        print(f"feature: {spec['id']} ...", file=sys.stderr)
        res = analyze_feature_buckets(spec["id"], spec["kind"], spec["feature_of"], rows,
                                      conv_keys, buckets, durations_ms, events, in_band,
                                      reps=reps, seed=seed)
        feature_results.append(res)
        if res.get("p_value") is not None:
            pvalue_registry.append(("bucket_family", spec["id"], None, res["p_value"]))

    # -- heatmap matrix (feature x bucket, point-estimate AUC-based association strength) --
    heat = np.full((len(canon), len(ALL_BUCKETS)), np.nan)
    for fi, spec in enumerate(canon):
        raw = np.array([spec["feature_of"](r) for r in rows], dtype=object)
        if spec["kind"] == "numeric":
            x = np.array([float(v) if v is not None and not (isinstance(v, float) and math.isnan(v))
                         else np.nan for v in raw])
            present = ~np.isnan(x)
        else:
            present = np.array([v is not None for v in raw])
            x = np.full(n_total, np.nan)
            x[present] = category_encode(raw[present], in_band[present])
            present = present & ~np.isnan(x)
        for bi, b in enumerate(ALL_BUCKETS):
            indicator = (buckets == b).astype(float)
            cell = heatmap_cell(x, present, indicator)
            heat[fi, bi] = np.nan if cell is None else cell

    # -- active strategies: feature x strategy association, in-window vs tenant-superset --
    strategies = load_active_strategies(args.control_db)
    print(f"{len(strategies)} active (strategy, tenant) pairs", file=sys.stderr)
    prefix_ok = np.array([r.cached_context >= 20_000 for r in rows])
    user_arr = np.array([r.user for r in rows], dtype=object)
    ts_arr = np.array([r.ts_ms for r in rows])

    strategy_summ = []
    n_features = len(canon)
    strat_matrix_inwin = np.full((n_features, len(strategies)), np.nan)
    strat_matrix_outwin = np.full((n_features, len(strategies)), np.nan)
    strat_labels = []
    feat_encoded_global = {}  # cache category encodings against `rescued`, computed once
    for si, s in enumerate(strategies):
        label = f"{pseudo.get(s['tenant_id'], s['tenant_id'])}/{s['strategy_id'][:6]}"
        strat_labels.append(label)
        tenant_mask = (user_arr == s["tenant_id"]) & reached_trig & prefix_ok
        inwin_mask = tenant_mask & np.array([in_window(t, s["windows"]) for t in ts_arr])
        n_gated, n_tenant = int(inwin_mask.sum()), int(tenant_mask.sum())
        rez_in = ~np.isnan(rescued) & inwin_mask
        rez_out = ~np.isnan(rescued) & tenant_mask
        rate_in = float(np.nanmean(rescued[inwin_mask])) if rez_in.any() else None
        n_res_in = int(rez_in.sum())
        lo, hi = wilson_ci(int(np.nansum(rescued[inwin_mask] == 1.0)), n_res_in) if n_res_in else (None, None)
        entry = {"strategy_id": s["strategy_id"], "tenant": pseudo.get(s["tenant_id"], "?"),
                "max_pings": s["max_pings"], "n_gated": n_gated, "n_resolved": n_res_in,
                "rescue_rate": rate_in, "ci95": [lo, hi], "n_tenant_all_windows": n_tenant}
        for fi, spec in enumerate(canon):
            raw_all = np.array([spec["feature_of"](r) for r in rows], dtype=object)
            if spec["kind"] == "numeric":
                fx = np.array([float(v) if v is not None and not (isinstance(v, float) and math.isnan(v))
                             else np.nan for v in raw_all])
            else:
                key = spec["id"]
                if key not in feat_encoded_global:
                    # A stable, population-level category -> rescue-band-rate encoding,
                    # computed once and reused across every strategy: correlating each
                    # strategy's SUBSET against a per-strategy-refitted encoding would let
                    # the encoding itself soak up the very tenant/window effect this check
                    # is trying to isolate from the feature's own signal.
                    pres_g = np.array([v is not None for v in raw_all])
                    enc_g = np.full(n_total, np.nan)
                    enc_g[pres_g] = category_encode(raw_all[pres_g], in_band[pres_g])
                    feat_encoded_global[key] = enc_g
                fx = feat_encoded_global[key]
            for mask, out_matrix in ((inwin_mask, strat_matrix_inwin),
                                     (tenant_mask, strat_matrix_outwin)):
                m = mask & ~np.isnan(rescued) & ~np.isnan(fx)
                n_m = int(m.sum())
                if n_m < args.min_strategy_n:
                    continue
                rho, p = spearman(fx[m], rescued[m])
                out_matrix[fi, si] = abs(rho)
                pvalue_registry.append(("strategy_family",
                                       f"{spec['id']}|{s['strategy_id']}"
                                       f"|{'in' if out_matrix is strat_matrix_inwin else 'out'}",
                                       {"n": n_m, "rho": rho}, p))
        strategy_summ.append(entry)

    # -- window-vs-tenant-superset aggregate flag, per feature --
    window_flag = []
    for fi, spec in enumerate(canon):
        col_in = strat_matrix_inwin[fi]
        col_out = strat_matrix_outwin[fi]
        both = np.isfinite(col_in) & np.isfinite(col_out)
        n_tested = int(both.sum())
        if n_tested == 0:
            window_flag.append({"id": spec["id"], "n_strategies_tested": 0})
            continue
        mean_in, mean_out = float(np.mean(col_in[both])), float(np.mean(col_out[both]))
        diff = float(np.mean(np.abs(col_in[both] - col_out[both])))
        window_flag.append({"id": spec["id"], "n_strategies_tested": n_tested,
                            "mean_abs_rho_in_window": mean_in,
                            "mean_abs_rho_tenant_superset": mean_out,
                            "mean_abs_diff": diff,
                            "verdict": ("effect is structurally about tenant/window, not "
                                       "the feature: lifting the window barely changes it"
                                       if diff < 0.05 else
                                       "effect differs meaningfully with/without the window "
                                       "restriction")})

    # -- BH-FDR across the whole pooled family --
    all_p = [p for _, _, _, p in pvalue_registry]
    adj = bh_fdr(all_p)
    n_survive = sum(1 for a in adj if not math.isnan(a) and a < 0.05)
    n_tested_total = sum(1 for p in all_p if not math.isnan(p))

    # -- flat dashboard summary view (dashfeat/PR #353's own schema), IN ADDITION TO the
    #    rich per-feature/per-strategy output above, never instead of it. vs_next_bucket is
    #    2*AUC-1 (Somers' D: a signed, [-1,1] rank-correlation restatement of the same
    #    auc_rescue_band already reported with its own CI and n above -- documented here so
    #    a dashboard reader knows what the float means) rather than a fresh Pearson
    #    correlation, per the brief's own explicit rule that a lone Pearson r is not an
    #    acceptable summary. vs_strategy_outcome's correlation is the SIGNED Spearman rho
    #    (the heatmap/matrix above stores its absolute value; the sign is preserved here by
    #    reading it back out of pvalue_registry's "in-window" entries) against that specific
    #    strategy's own gated, resolved population -- see window_vs_tenant_flag for whether
    #    that is really a window/tenant effect rather than the feature's own signal.
    signed_rho_inwin: dict[tuple[str, str], float] = {}
    for fam, key, extra, _p in pvalue_registry:
        if fam != "strategy_family" or not key.endswith("|in"):
            continue
        feat_id, strategy_id, _ = key.split("|")
        signed_rho_inwin[(feat_id, strategy_id)] = extra["rho"]
    dash_features = []
    for spec, res in zip(canon, feature_results):
        auc = res.get("auc_rescue_band", {}).get("auc")
        dash_features.append({
            "id": spec["id"],
            "vs_next_bucket": None if auc is None else 2.0 * auc - 1.0,
            "vs_next_bucket_measure": "2*AUC-1 (Somers' D) for landing in the rescue band "
                                     "(5m-1h], resolved rows only -- see feature_results "
                                     "for the underlying AUC, its 95% CI and n",
            "vs_strategy_outcome": [
                {"strategy": s["strategy_id"],
                 "correlation": signed_rho_inwin.get((spec["id"], s["strategy_id"]))}
                for s in strategies
            ],
        })

    # -- write correlations.json --
    payload = {
        "features": dash_features,
        "seed": seed, "bootstrap_reps": reps,
        "n_decision_points": n_total, "n_conversations": n_conversations,
        "window_end_ms": window_end_ms,
        "reference_check": reference_check,
        "km_sensitivity_bucket_probs": km_probs,
        "buckets": ALL_BUCKETS,
        "feature_results": feature_results,
        "heatmap": {"features": [c["id"] for c in canon], "buckets": ALL_BUCKETS,
                   "matrix": heat.tolist()},
        "strategies": strategy_summ,
        "strategy_labels": strat_labels,
        "window_vs_tenant_flag": window_flag,
        "fdr": {"family_size": len(all_p), "n_p_values_tested": n_tested_total,
               "n_survive_q05": n_survive,
               "records": [{"family": fam, "key": key, "extra": extra, "p": p, "q": q}
                          for (fam, key, extra, p), q in zip(pvalue_registry, adj)]},
    }
    Path(args.out_json).parent.mkdir(parents=True, exist_ok=True)
    Path(args.out_json).write_text(json.dumps(payload, indent=2, default=str), encoding="utf-8")
    print(f"wrote {args.out_json}", file=sys.stderr)

    # -- plots --
    plot_heatmap([c["id"] for c in canon], heat, plots_dir)
    plot_numeric_curves(feature_results, plots_dir)
    plot_auc_ranking(feature_results, plots_dir)
    plot_strategy_matrix([c["id"] for c in canon], strat_labels, strat_matrix_inwin, None,
                        plots_dir)
    print(f"wrote plots to {plots_dir}", file=sys.stderr)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
