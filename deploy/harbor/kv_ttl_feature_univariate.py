#!/usr/bin/env python3
"""Univariate evidence for every catalogued kv-ttl keep-alive feature.

Answers, for each feature the site's feature-availability catalogue
(site/gen/p_features.py's FEATURES list, sourced from reports/A4-keepalive.md Phase 2)
marks LIVE or OFFLINE: does it exist in usable volume, what does it look like, and does it
predict anything -- against THREE separate outcomes (return-within-band hazard, prefix
compatibility given a request arrives, net dollar decision value), because the study's
running lesson is that a feature can answer those three questions differently.

Reuses rather than reinvents: kv_ttl_cost_model's Request/derive/evaluate/PriceBook (the
tested cost engine) and kv_ttl_predictor_arms's History/bucket_of/stop_cluster/gate_action/
per_conversation_costs/bootstrap_ci/pseudonymize_map (the tested replay + bootstrap
machinery). This script only adds: the wider column set the catalogue's other features
need, the per-feature statistical battery (coverage, distribution, 3-outcome association
with CI, BH-FDR, confounding stratification), and the plot/report generation.

READ-ONLY against the snapshot. Never opens the live service or /etc/context-guru itself;
--prices defaults to a local patched copy (see prices.local.yaml) with the #324 bare-alias
fix, exactly as A4-keepalive.md's Phase 3 did.

Usage:
    python3 kv_ttl_feature_univariate.py \
        --db /home/vpcuser/cg-night-0928/snap/cg.db \
        --prices /home/vpcuser/cg-night-0928/exp3/features/prices.local.yaml \
        --out-json /home/vpcuser/cg-night-0928/exp3/features/feature_evidence.json \
        --out-plots /home/vpcuser/cg-night-0928/exp3/features/plots \
        --train-frac 0.6 --bootstrap-reps 400 --seed 0
"""
from __future__ import annotations

import argparse
import json
import math
import os
import sqlite3
import sys
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from urllib.parse import quote

import numpy as np

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from kv_ttl_cost_model import (  # noqa: E402
    EXPIRE, WRITE_5M, PING_5M, UNRESCUABLE,
    DEFAULT_CACHE_READ_MULTIPLE, DEFAULT_WRITE_5M_MULTIPLE,
    PriceBook, Request, Semantics, PingSchedule, derive, evaluate,
)
from kv_ttl_predictor_arms import (  # noqa: E402
    History, bucket_of, stop_cluster, gate_action, pseudonymize_map,
    per_conversation_costs, bootstrap_ci, MIN_PREFIX,
)

DEFAULT_DB = "/home/vpcuser/cg-night-0928/snap/cg.db"
DEFAULT_PRICES = str(Path(__file__).with_name("prices.local.yaml"))
IDLE_5M_MS = PingSchedule().idle_5m_ms  # 280_000 -- the production ping trigger's own delay
HORIZON_5M_MS = 300_000
HORIZON_1H_MS = 3_600_000
# r/(w-r): the marginal cost of a write over a read is the true break-even a gate has to
# clear, not r/w -- matches kvcache/keepalive.go's own constant and the site's 8.70% figure.
BREAK_EVEN_PCT = 100 * DEFAULT_CACHE_READ_MULTIPLE / (DEFAULT_WRITE_5M_MULTIPLE - DEFAULT_CACHE_READ_MULTIPLE)

# The 2026-08-17 -> 08-19 bootstrap window carries a price step and an unrelated attribution
# defect (per COORDINATION.md); excluded from any per-week/time-of-collection figure. This
# script produces none (no per-week time series), so the exclusion is a no-op, stated here
# so a reader does not have to guess whether it was forgotten.
EXCLUDE_WINDOW_MS = None  # not used; see note above


# ── data model: superset of ExtRequest's columns for the features this sweep needs ─────

@dataclass
class FRequest(Request):
    stop_reason: str = ""
    agent: str = ""
    cache_read: int = 0
    cache_write: int = 0
    tokens_before: int = 0
    tokens_after: int = 0
    attempted_tokens: int = 0
    frozen_tokens: int = 0
    saved_unique: int = 0
    tools: int = 0
    system_blocks: int = 0
    reasoning_effort: str = ""
    thinking_mode: str = ""
    thinking_budget: int = 0
    expands: int = 0
    reverts: int = 0
    cache_write_1h: int = 0
    # filled by attach_derived(), never by the SQL load: strictly-backward quantities.
    turn_index: int = 0
    session_age_ms: int = 0
    prev_gap_ms: float | None = None       # gap BEFORE this row (None for a conversation's 1st)
    prev_reverts: int | None = None        # lag-1, per the brief's explicit rule
    prev_expands: int | None = None
    prev_miss_reason: str | None = None
    prev_tools: int | None = None
    prev_system_blocks: int | None = None
    prev_model: str | None = None
    prev_reasoning_effort: str | None = None
    prev_thinking_mode: str | None = None
    prev_thinking_budget: int | None = None
    prev_tokens_after: int | None = None
    prev_cache_read: int | None = None
    prev_cache_write: int | None = None
    changed_prefix_shape: int | None = None    # 1 if any of tools/system/model/thinking changed
    branch_compaction_marker: int | None = None
    hist_p_band: float | None = None       # kvcache.History's leak-free P(<=5m) at this cell
    hist_n: int | None = None              # sample count backing that estimate (cold-start proxy)
    tool_recent_name: str | None = None    # most recently-finished tool, strictly before ts_ms
    digest_changed_lag1: int | None = None  # tool_declarations digest changed one transition back
    next_compatible: bool | None = None    # outcome (b): only defined when next_ts_ms is not None
    next_stop_cluster: str | None = None


def load_feature_rows(db_path: str) -> list[FRequest]:
    uri = f"file:{quote(str(Path(db_path).resolve()))}?mode=ro"
    con = sqlite3.connect(uri, uri=True)
    try:
        cols = ["id", "tenant_id", "session_id", "ts", "model", "fresh_input",
                "output_tokens", "cache_read", "cache_write", "cache_miss_reason",
                "upstream_ms", "cost_usd", "stop_reason", "agent",
                "tokens_before", "tokens_after", "attempted_tokens", "frozen_tokens",
                "saved_unique", "tools", "system_blocks", "reasoning_effort",
                "thinking_mode", "thinking_budget", "expands", "reverts", "cache_write_1h"]
        sql = (f"SELECT {', '.join(cols)} FROM requests WHERE session_id <> '' "
               f"AND token_accounting <> 'missing' AND keepalive = 0 ORDER BY ts, id")
        rows = [
            FRequest(request_id=r[0], user=r[1], conversation=r[2], ts_ms=r[3], model=r[4],
                     input_tokens=r[5] or 0, output_tokens=r[6] or 0,
                     cached_context=(r[7] or 0) + (r[8] or 0), miss_reason=r[9] or "",
                     hit=(r[9] or "") == "hit", upstream_ms=r[10] or 0.0, billed_usd=r[11] or 0.0,
                     stop_reason=r[12] or "", agent=r[13] or "",
                     cache_read=r[7] or 0, cache_write=r[8] or 0,
                     tokens_before=r[14] or 0, tokens_after=r[15] or 0,
                     attempted_tokens=r[16] or 0, frozen_tokens=r[17] or 0,
                     saved_unique=r[18] or 0, tools=r[19] or 0, system_blocks=r[20] or 0,
                     reasoning_effort=r[21] or "", thinking_mode=r[22] or "",
                     thinking_budget=r[23] or 0, expands=r[24] or 0, reverts=r[25] or 0,
                     cache_write_1h=r[26] or 0)
            for r in con.execute(sql, [])
        ]
    finally:
        con.close()
    return derive(rows)


def load_tool_uses(db_path: str) -> dict[tuple[str, str], list[tuple[int, int, str]]]:
    """(tenant, session) -> sorted list of (first_ts, last_ts, name). Session-level only --
    the catalogue's confirmed limit: no per-call row, no per-request linkage exists."""
    uri = f"file:{quote(str(Path(db_path).resolve()))}?mode=ro"
    con = sqlite3.connect(uri, uri=True)
    try:
        out: dict[tuple[str, str], list[tuple[int, int, str]]] = {}
        for tenant, session, name, first_ts, last_ts in con.execute(
                "SELECT tenant_id, session_id, name, first_ts, last_ts FROM tool_uses"):
            out.setdefault((tenant, session), []).append((first_ts, last_ts, name))
        for v in out.values():
            v.sort()
        return out
    finally:
        con.close()


def load_digest_transitions(db_path: str) -> dict[tuple[str, str], list[tuple[int, str]]]:
    """(tenant, session) -> sorted distinct (ts, digest) pairs -- one row per moment the
    session's declared-tool set was recorded (many declaration rows share one ts; digest is
    already the whole-set fingerprint, so DISTINCT collapses them for free)."""
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


BRANCH_DROP_FRAC = 0.30  # a >=30% drop vs the preceding tokens_after is the compaction footprint


def attach_derived(rows: list[FRequest], tool_uses: dict, digest_tr: dict) -> list[FRequest]:
    """One chronological pass per conversation, everything computed strictly from rows at or
    before the current one (the brief's backwards-only rule; reverts/expands/miss_reason are
    additionally lagged one full row, per its explicit instruction)."""
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
        di = 0  # pointer into dtr
        prev: FRequest | None = None
        for i, r in enumerate(group):
            r.turn_index = i + 1
            r.session_age_ms = r.ts_ms - first_ts
            if prev is not None:
                r.prev_gap_ms = float(prev.idle_ms) if prev.idle_ms is not None else float(r.ts_ms - prev.ts_ms)
                r.prev_reverts, r.prev_expands, r.prev_miss_reason = prev.reverts, prev.expands, prev.miss_reason
                r.prev_tools, r.prev_system_blocks, r.prev_model = prev.tools, prev.system_blocks, prev.model
                r.prev_reasoning_effort = prev.reasoning_effort
                r.prev_thinking_mode, r.prev_thinking_budget = prev.thinking_mode, prev.thinking_budget
                r.prev_tokens_after = prev.tokens_after
                r.prev_cache_read, r.prev_cache_write = prev.cache_read, prev.cache_write
                r.changed_prefix_shape = int(
                    r.tools != prev.tools or r.system_blocks != prev.system_blocks
                    or r.model != prev.model or r.reasoning_effort != prev.reasoning_effort
                    or r.thinking_mode != prev.thinking_mode or r.thinking_budget != prev.thinking_budget)
                if prev.tokens_after > 0:
                    r.branch_compaction_marker = int(
                        r.tokens_before < prev.tokens_after * (1 - BRANCH_DROP_FRAC))
                prev.next_stop_cluster = stop_cluster(r.stop_reason)
                mr = r.miss_reason or ""
                prev.next_compatible = r.hit or (mr not in UNRESCUABLE)
            # kvcache.History: observe the gap that just closed (bucketed at the PRIOR
            # request's hour, mirroring replay_history_actions), THEN read the cell for
            # THIS decision -- leak-free by construction, identical class as the arm.
            if prev is not None and last_hour_utc is not None:
                gap_s = max(0.0, (r.ts_ms - prev.ts_ms) / 1000.0)
                hist.observe(r.user, r.model, bucket_of(last_hour_utc), gap_s)
            p, n = hist.reuse_within(r.user, r.model, bucket_of(r.hour_utc), HORIZON_5M_MS / 1000.0)
            r.hist_p_band, r.hist_n = p, n
            # most recently-finished tool call strictly before ts_ms (session-level table;
            # cannot be linked to a specific request, only "has this tool finished by now")
            name = None
            best_last = -1
            for first_ts_t, last_ts_t, nm in tu:
                if last_ts_t < r.ts_ms and last_ts_t > best_last:
                    best_last, name = last_ts_t, nm
            r.tool_recent_name = name or "(none yet)"
            # digest-change, lagged one full transition per the brief's rule for this
            # family of features: did the PREVIOUS pair of consecutive declarations differ,
            # not whether THIS request's own declaration just changed.
            while di < len(dtr) and dtr[di][0] < r.ts_ms:
                di += 1
            if di >= 2:
                r.digest_changed_lag1 = int(dtr[di - 2][1] != dtr[di - 1][1])
            elif di == 1:
                r.digest_changed_lag1 = 0  # only one declaration ever seen before this point
            prev = r
            last_hour_utc = r.hour_utc
    return rows


def weekday_of(ts_ms: int) -> str:
    return datetime.fromtimestamp(ts_ms / 1000.0, tz=timezone.utc).strftime("%a")


def p_from_ci(point: float, lo: float, hi: float) -> float:
    """Two-sided p-value approximated from a 95% CI's implied normal SE. Used only for
    outcome (c), whose reused bootstrap_ci helper returns percentiles, not the raw
    resample array -- an exact bootstrap p-value would need duplicating that resampling
    loop for no real gain in this context."""
    if lo is None or hi is None or math.isnan(lo) or math.isnan(hi):
        return float("nan")
    se = (hi - lo) / (2 * 1.96)
    if se <= 0:
        return float("nan")
    z = point / se
    return 2 * (1 - _norm_cdf(abs(z)))


# ── outcome frame ────────────────────────────────────────────────────────────

def band_of(idle_ms: float | None) -> str:
    if idle_ms is None:
        return "censored"
    if idle_ms <= HORIZON_5M_MS:
        return "in_5m"
    if idle_ms <= HORIZON_1H_MS:
        return "in_1h"
    return "later"


def build_frame(rows: list[FRequest], tenant_pseudo: dict[str, str]):
    """One row per decision point, every column either an input already known at that
    point, or one of the three outcomes -- never both in a way a feature could see its own
    label."""
    import pandas as pd
    data = {
        "request_id": [r.request_id for r in rows],
        "tenant": [tenant_pseudo[r.user] for r in rows],
        "conv_key": [r.key for r in rows],
        "ts_ms": [r.ts_ms for r in rows],
        "model": [r.model for r in rows],
        "agent": [r.agent for r in rows],
        "weekday": [weekday_of(r.ts_ms) for r in rows],
        "hour_of_day": [r.hour_utc for r in rows],
        "stop_reason": [r.stop_reason or "(none)" for r in rows],
        "stop_cluster": [stop_cluster(r.stop_reason) for r in rows],
        "turn_index": [r.turn_index for r in rows],
        "session_age_ms": [r.session_age_ms for r in rows],
        "cached_context": [r.cached_context for r in rows],
        "tokens_before": [r.tokens_before for r in rows],
        "saved_unique": [r.saved_unique for r in rows],
        "prev_gap_ms": [r.prev_gap_ms for r in rows],
        "hist_p_band": [r.hist_p_band for r in rows],
        "hist_n": [r.hist_n for r in rows],
        "tool_recent_name": [r.tool_recent_name for r in rows],
        "digest_changed_lag1": [r.digest_changed_lag1 for r in rows],
        "changed_prefix_shape": [r.changed_prefix_shape for r in rows],
        "branch_compaction_marker": [r.branch_compaction_marker for r in rows],
        "prev_cache_read": [r.prev_cache_read for r in rows],
        "prev_reverts": [r.prev_reverts for r in rows],
        "prev_expands": [r.prev_expands for r in rows],
        "cached_context_ok": [r.cached_context >= MIN_PREFIX for r in rows],
        "idle_ms": [r.idle_ms for r in rows],
        "band": [band_of(r.idle_ms) for r in rows],
        "next_compatible": [r.next_compatible for r in rows],
        "idle_ge_280s": [None if r.idle_ms is None else r.idle_ms >= IDLE_5M_MS for r in rows],
    }
    df = pd.DataFrame(data)
    df["in_5m"] = (df["band"] == "in_5m").astype(float)
    df.loc[df["band"] == "censored", "in_5m"] = np.nan
    df["in_1h_or_earlier"] = df["band"].isin(["in_5m", "in_1h"]).astype(float)
    df.loc[df["band"] == "censored", "in_1h_or_earlier"] = np.nan
    df["next_compatible_f"] = df["next_compatible"].map({True: 1.0, False: 0.0})
    # supplementary stratifiers for the confounding check (brief: stop_reason / tenant /
    # model / turn index / hour of day) -- turn_index and hour_of_day are continuous/high-
    # cardinality, so bucket them the same way the rest of this study already does.
    try:
        df["turn_tercile"] = pd.qcut(df["turn_index"], q=3, labels=["early", "mid", "late"],
                                     duplicates="drop")
    except ValueError:
        df["turn_tercile"] = "early"
    df["hour_bucket"] = df["hour_of_day"].map(bucket_of)
    return df


STRATA_COLS = ["stop_cluster", "tenant", "model", "turn_tercile", "hour_bucket"]


# ── statistics helpers ───────────────────────────────────────────────────────

def wilson_ci(k: int, n: int, z: float = 1.96) -> tuple[float, float]:
    if n == 0:
        return (float("nan"), float("nan"))
    p = k / n
    denom = 1 + z * z / n
    centre = p + z * z / (2 * n)
    half = z * math.sqrt(p * (1 - p) / n + z * z / (4 * n * n))
    return ((centre - half) / denom, (centre + half) / denom)


def two_proportion_z(k1: int, n1: int, k2: int, n2: int) -> tuple[float, float]:
    """Two-sided z-test for a difference of proportions. Returns (z, p). NaN when either
    group is empty or the pooled variance is degenerate."""
    if n1 == 0 or n2 == 0:
        return (float("nan"), float("nan"))
    p1, p2 = k1 / n1, k2 / n2
    p_pool = (k1 + k2) / (n1 + n2)
    se = math.sqrt(p_pool * (1 - p_pool) * (1 / n1 + 1 / n2))
    if se == 0:
        return (0.0, 1.0)
    z = (p1 - p2) / se
    p = 2 * (1 - _norm_cdf(abs(z)))
    return (z, p)


def _norm_cdf(x: float) -> float:
    return 0.5 * (1 + math.erf(x / math.sqrt(2)))


def bh_fdr(pvalues: list[float]) -> list[float]:
    """Benjamini-Hochberg step-up. NaN p-values pass through as NaN and are excluded from
    the ranking (they carry no comparison, e.g. a stratum too small to test)."""
    idx = [i for i, p in enumerate(pvalues) if not math.isnan(p)]
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


def bootstrap_auc(df, feature_col: str, outcome_col: str, *, reps: int, seed: int) -> dict:
    """AUC of one numeric feature ranking one binary outcome, CI via conversation
    resampling (never row resampling -- rows in one conversation share history)."""
    from sklearn.metrics import roc_auc_score
    sub = df[[feature_col, outcome_col, "conv_key"]].dropna()
    if sub[outcome_col].nunique() < 2 or len(sub) < 20:
        return {"auc": None, "ci95": [None, None], "n": len(sub)}
    by_key: dict = {}
    for key, x, y in zip(sub["conv_key"], sub[feature_col], sub[outcome_col]):
        by_key.setdefault(key, []).append((x, y))
    keys = list(by_key.keys())
    x_all, y_all = sub[feature_col].to_numpy(), sub[outcome_col].to_numpy()
    point = roc_auc_score(y_all, x_all)
    rng = np.random.default_rng(seed)
    n = len(keys)
    reps_out = []
    for _ in range(reps):
        idx = rng.integers(0, n, size=n)
        xs, ys = [], []
        for i in idx:
            for x, y in by_key[keys[i]]:
                xs.append(x)
                ys.append(y)
        ys = np.asarray(ys)
        if len(set(ys.tolist())) < 2:
            continue
        reps_out.append(roc_auc_score(ys, np.asarray(xs)))
    if not reps_out:
        return {"auc": float(point), "ci95": [None, None], "n": len(sub)}
    lo, hi = np.percentile(reps_out, [2.5, 97.5])
    return {"auc": float(point), "ci95": [float(lo), float(hi)], "n": int(len(sub))}


def numeric_bins(df, feature_col: str, outcome_col: str, *, max_bins: int = 10) -> list[dict]:
    """Quantile-binned event rate curve with Wilson CIs. Duplicate quantile edges (heavy
    ties, e.g. many-zero columns) collapse bins rather than erroring."""
    import pandas as pd
    sub = df[[feature_col, outcome_col]].dropna()
    if len(sub) < 20:
        return []
    try:
        cats = pd.qcut(sub[feature_col], q=max_bins, duplicates="drop")
    except ValueError:
        return []
    out = []
    for interval, grp in sub.groupby(cats, observed=True):
        n, k = len(grp), int(grp[outcome_col].sum())
        lo, hi = wilson_ci(k, n)
        out.append({"bin": str(interval), "n": n, "k": k, "rate": k / n if n else None,
                    "ci95": [lo, hi]})
    return out


def categorical_rates(df, feature_col: str, outcome_col: str, *, min_n: int = 30,
                      max_categories: int = 30) -> list[dict]:
    import pandas as pd
    sub = df[[feature_col, outcome_col]].dropna()
    if sub.empty:
        return []
    counts = sub[feature_col].value_counts()
    keep = counts.index[:max_categories]
    out = []
    for cat in keep:
        grp = sub[sub[feature_col] == cat]
        n, k = len(grp), int(grp[outcome_col].sum())
        lo, hi = wilson_ci(k, n) if n >= min_n else (float("nan"), float("nan"))
        out.append({"category": str(cat), "n": n, "k": k,
                    "rate": k / n if n else None, "ci95": [lo, hi], "meets_min_n": n >= min_n})
    return out


def headline_categorical_contrast(rates: list[dict]) -> dict | None:
    """The two highest-n categories that both clear min_n, compared by a two-proportion
    z-test and a risk ratio -- the brief's 'rank-based measure where sensible' for a
    categorical feature, standing in for the AUC used with a numeric one."""
    elig = [r for r in rates if r["meets_min_n"]]
    if len(elig) < 2:
        return None
    elig = sorted(elig, key=lambda r: -r["n"])[:2]
    a, b = elig
    z, p = two_proportion_z(a["k"], a["n"], b["k"], b["n"])
    rr = (a["rate"] / b["rate"]) if b["rate"] else None
    return {"a": a["category"], "b": b["category"], "rate_a": a["rate"], "rate_b": b["rate"],
            "risk_ratio_a_over_b": rr, "z": z, "p_value": p}


def stratified_check(df, feature_col: str, outcome_col: str, strata_col: str, *,
                     kind: str, min_n: int = 50) -> dict:
    """The brief's confounding check: recompute the headline effect within each stratum of
    a covariate (stop_reason cluster, tenant, model, turn tercile, hour bucket) and report
    whether the pooled direction/magnitude survives, or was an artefact of the stratifier
    (the '280s idle trigger already conditioned the population' trap)."""
    import pandas as pd
    out = {"strata_col": strata_col, "strata": []}
    for stratum, grp in df.groupby(strata_col, observed=True):
        sub = grp[[feature_col, outcome_col]].dropna()
        if len(sub) < min_n:
            out["strata"].append({"stratum": str(stratum), "n": len(sub), "skipped": True})
            continue
        if kind == "numeric":
            if sub[outcome_col].nunique() < 2:
                out["strata"].append({"stratum": str(stratum), "n": len(sub), "skipped": True})
                continue
            from sklearn.metrics import roc_auc_score
            auc = float(roc_auc_score(sub[outcome_col], sub[feature_col]))
            out["strata"].append({"stratum": str(stratum), "n": len(sub), "effect": auc,
                                  "effect_type": "auc"})
        else:
            top = sub[feature_col].value_counts().index[:1]
            if len(top) == 0:
                out["strata"].append({"stratum": str(stratum), "n": len(sub), "skipped": True})
                continue
            mask = sub[feature_col] == top[0]
            rate_in = sub.loc[mask, outcome_col].mean() if mask.any() else None
            rate_out = sub.loc[~mask, outcome_col].mean() if (~mask).any() else None
            out["strata"].append({"stratum": str(stratum), "n": len(sub),
                                  "top_category": str(top[0]), "rate_top": rate_in,
                                  "rate_rest": rate_out, "effect_type": "rate_diff",
                                  "effect": (rate_in - rate_out) if (rate_in is not None and
                                                                     rate_out is not None) else None})
    return out


DISAGREEMENT_NULL = {"numeric": 0.5, "categorical": 0.0}


def summarize_disagreement(pooled_effect, by_stratum: dict, kind: str) -> dict:
    """Per team-lead's tenant-weighting finding: a pooled effect can be dominated by one
    tenant's changing share of the window, so a disagreement between the pooled number and
    the per-stratum picture is itself a finding, not noise -- flagged here rather than
    picked around. Heuristic, not a formal test: 'disagree' if any non-skipped stratum's
    effect sits on the opposite side of the null from the pooled effect, or if the
    stratum-to-stratum range is at least 2x the pooled effect's own distance from the null.
    """
    null = DISAGREEMENT_NULL[kind]
    out = {"null": null, "flip_in": [], "range_ratio": None, "disagrees": False}
    if pooled_effect is None:
        return out
    pooled_dist = pooled_effect - null
    effects = []
    for strata_name, res in by_stratum.items():
        for s in res.get("strata", []):
            if s.get("skipped") or s.get("effect") is None:
                continue
            effects.append(s["effect"])
            if pooled_dist != 0 and (s["effect"] - null) * pooled_dist < 0 and abs(s["effect"] - null) > 0.02:
                out["flip_in"].append({"stratifier": strata_name, "stratum": s["stratum"],
                                       "effect": s["effect"], "n": s["n"]})
    if effects and pooled_dist != 0:
        span = max(effects) - min(effects)
        out["range_ratio"] = float(span / abs(pooled_dist)) if pooled_dist else None
        if out["range_ratio"] is not None and out["range_ratio"] >= 2.0:
            out["disagrees"] = True
    if out["flip_in"]:
        out["disagrees"] = True
    return out


# ── outcome (c): net dollar value of pinging whenever the feature crosses a rule ───────

from dataclasses import replace as dc_replace  # noqa: E402
from collections import Counter  # noqa: E402


def fixed_5m_actions(rows) -> dict[int, str]:
    return {r.request_id: WRITE_5M for r in rows}


def _rule_train_cost(rows, decide, prices, semantics, schedule, window_end_ms) -> float:
    actions = {r.request_id: gate_action(r.cached_context, decide(r)) for r in rows}
    return sum(per_conversation_costs(rows, actions, prices, semantics, schedule,
                                      window_end_ms).values())


def ping_everyone_actions(rows) -> dict[int, str]:
    """The strawman every rule needs to beat, not just fixed-5m: gate_action's own
    would_ping=True for every MIN_PREFIX-eligible row, no feature involved. Confirmed
    directly against the pre-existing, unmodified cost model (kv_ttl_predictor_arms.
    gate_action/per_conversation_costs) that this ALREADY beats fixed-5m by ~+5.7% on this
    snapshot's test window -- because most gaps close inside PingSchedule's 280s interval
    (no ping fires, no extra cost) while the ones that don't get cheaply rescued. That means
    almost any rule covering most of the MIN_PREFIX-eligible population will look good
    against fixed-5m for a reason that has nothing to do with the feature. Every feature's
    outcome (c) is therefore scored against BOTH baselines; the verdict is decided by the
    harder one."""
    return {r.request_id: gate_action(r.cached_context, True) for r in rows}


def tune_and_score_dollar_value(train_rows, test_rows, feature_of, kind: str, *,
                                prices, semantics, schedule, train_cut_ms: int, hi_ms: int,
                                reps: int, seed: int,
                                local_test=None, base_conv_test=None,
                                pingall_conv_test=None) -> dict | None:
    """Fit the threshold/category on TRAIN only (chronological discipline), score once on
    TEST, re-derived in isolation so 'the next request' never reaches across the train/test
    boundary (kv_ttl_predictor_arms.fold_cost's own convention). local_test/base_conv_test/
    pingall_conv_test are the same for every feature (they don't depend on it) and may be
    passed in precomputed to avoid re-deriving and re-evaluating them 19 times."""
    if kind == "numeric":
        vals = np.array([feature_of(r) for r in train_rows], dtype=float)
        finite = vals[~np.isnan(vals)]
        if finite.size < 30:
            return {"rule": None, "reason": "fewer than 30 non-missing train values"}
        candidates = sorted(set(np.quantile(finite, [0.5, 0.6, 0.7, 0.75, 0.8, 0.85, 0.9, 0.95])))
        best = None
        for direction in ("ge", "le"):
            for thr in candidates:
                def decide(r, thr=thr, direction=direction):
                    v = feature_of(r)
                    if v is None or (isinstance(v, float) and math.isnan(v)):
                        return False
                    return v >= thr if direction == "ge" else v <= thr
                cost = _rule_train_cost(train_rows, decide, prices, semantics, schedule, train_cut_ms)
                if best is None or cost < best[0]:
                    best = (cost, thr, direction)
        _, thr, direction = best
        rule = {"threshold": float(thr), "direction": direction}

        def final_decide(r):
            v = feature_of(r)
            if v is None or (isinstance(v, float) and math.isnan(v)):
                return False
            return v >= thr if direction == "ge" else v <= thr
    elif kind == "categorical":
        cats = Counter(feature_of(r) for r in train_rows)
        top = [c for c, _ in cats.most_common(30)]
        if not top:
            return {"rule": None, "reason": "no categories observed in train"}
        best = None
        for cat in top:
            def decide(r, cat=cat):
                return feature_of(r) == cat
            cost = _rule_train_cost(train_rows, decide, prices, semantics, schedule, train_cut_ms)
            if best is None or cost < best[0]:
                best = (cost, cat)
        _, cat = best
        rule = {"category": str(cat)}

        def final_decide(r):
            return feature_of(r) == cat
    else:
        raise ValueError(kind)

    if local_test is None:
        local_test = derive([dc_replace(r) for r in test_rows])
    if base_conv_test is None:
        base_conv_test = per_conversation_costs(local_test, fixed_5m_actions(local_test),
                                                 prices, semantics, schedule, hi_ms)
    if pingall_conv_test is None:
        pingall_conv_test = per_conversation_costs(local_test, ping_everyone_actions(local_test),
                                                    prices, semantics, schedule, hi_ms)
    arm_actions = {r.request_id: gate_action(r.cached_context, final_decide(r)) for r in local_test}
    arm_conv = per_conversation_costs(local_test, arm_actions, prices, semantics, schedule, hi_ms)

    def _norm(ci: dict) -> dict:
        return {"n_conversations": ci["n_conversations"], "point": ci["point_delta_usd"],
               "point_pct": ci["point_delta_pct"], "ci95": ci["ci95_delta_usd"],
               "baseline_usd": ci["baseline_usd"], "arm_usd": ci["arm_usd"]}

    vs_fixed5m = _norm(bootstrap_ci(base_conv_test, arm_conv, reps=reps, seed=seed))
    vs_ping_everyone = _norm(bootstrap_ci(pingall_conv_test, arm_conv, reps=reps, seed=seed))
    # the harder, decisive comparison: beating fixed-5m is easy (ping-everyone already does,
    # by ~+5.7% here) and is reported for continuity with the rest of this study's own
    # convention, but a feature only "helps AS A GATE" if it beats ping-everyone too.
    return {"rule": rule, "tuned_on": "train (<= train_cut)", "scored_on": "test (> train_cut)",
            "vs_fixed_5m": vs_fixed5m, "vs_ping_everyone": vs_ping_everyone,
            **vs_ping_everyone}


# ── the catalogue's 24 LIVE/OFFLINE rows -> 18 canonical measurements + 4 aliases ───────
# family/availability/prior_verdict are transcribed from site/gen/p_features.py (the
# authoritative catalogue this sweep is asked to replace "NOT MEASURED" in), not
# recomputed -- they describe what was already known, not this script's own output.

CANON = [
    dict(id="gap_since_last_ms", family="User behaviour", availability="LIVE",
        kind="numeric", col="prev_gap_ms", feature_of=lambda r: r.prev_gap_ms,
        catalogue_name="inter-request gap distribution & survival", prior_verdict="HELPS"),
    dict(id="return_probability_band_hazard", family="User behaviour", availability="OFFLINE",
        kind="numeric", col="hist_p_band", feature_of=lambda r: r.hist_p_band,
        catalogue_name="return probability (band / hazard)", prior_verdict="HELPS WEAKLY"),
    dict(id="weekday", family="User behaviour", availability="LIVE",
        kind="categorical", col="weekday", feature_of=lambda r: weekday_of(r.ts_ms),
        catalogue_name="weekday", prior_verdict="NO SIGNAL"),
    dict(id="hour_of_day", family="User behaviour", availability="LIVE",
        kind="categorical", col="hour_of_day", feature_of=lambda r: r.hour_utc,
        catalogue_name="hour of day", prior_verdict="NO SIGNAL"),
    dict(id="cold_start_pooling_n", family="User behaviour", availability="OFFLINE",
        kind="numeric", col="hist_n", feature_of=lambda r: r.hist_n,
        catalogue_name="cold-start hierarchical pooling", prior_verdict="HELPS"),
    dict(id="agent", family="Session metadata", availability="LIVE",
        kind="categorical", col="agent", feature_of=lambda r: r.agent,
        catalogue_name="agent (client dialect)", prior_verdict="NOT MEASURED"),
    dict(id="model", family="Session metadata", availability="LIVE",
        kind="categorical", col="model", feature_of=lambda r: r.model,
        catalogue_name="model", prior_verdict="HELPS (as partition key)"),
    dict(id="turn_index", family="Session metadata", availability="LIVE",
        kind="numeric", col="turn_index", feature_of=lambda r: r.turn_index,
        catalogue_name="turn index", prior_verdict="NO SIGNAL"),
    dict(id="stop_reason", family="Session metadata", availability="LIVE",
        kind="categorical", col="stop_reason", feature_of=lambda r: r.stop_reason or "(none)",
        catalogue_name="stop_reason", prior_verdict="HIGHEST SIGNAL, HARMS AS A GATE"),
    dict(id="session_age_ms", family="Session metadata", availability="LIVE",
        kind="numeric", col="session_age_ms", feature_of=lambda r: r.session_age_ms,
        catalogue_name="session age (wall-clock since first request)", prior_verdict="NOT MEASURED"),
    dict(id="context_window_headroom", family="Agent state", availability="LIVE",
        kind="numeric", col="tokens_before", feature_of=lambda r: r.tokens_before,
        catalogue_name="context-window headroom", prior_verdict="NOT MEASURED"),
    dict(id="tool_recent_name", family="Tools", availability="LIVE",
        kind="categorical", col="tool_recent_name", feature_of=lambda r: r.tool_recent_name,
        catalogue_name="tool name / type, session-level", prior_verdict="NOT MEASURED"),
    dict(id="prefix_size", family="Cache & economics", availability="LIVE",
        kind="numeric", col="cached_context", feature_of=lambda r: r.cached_context,
        catalogue_name="prefix size / cached-prefix length", prior_verdict="HELPS"),
    dict(id="changed_tools_system_model_thinking", family="Cache & economics", availability="LIVE",
        kind="categorical", col="changed_prefix_shape", feature_of=lambda r: r.changed_prefix_shape,
        catalogue_name="changed tools / system / model / thinking mode", prior_verdict="NOT MEASURED"),
    dict(id="per_prefix_digest_change", family="Cache & economics", availability="OFFLINE",
        kind="categorical", col="digest_changed_lag1", feature_of=lambda r: r.digest_changed_lag1,
        catalogue_name="per-prefix stable identity / overlap", prior_verdict="NOT MEASURED"),
    dict(id="branch_compaction_marker", family="Cache & economics", availability="OFFLINE",
        kind="categorical", col="branch_compaction_marker",
        feature_of=lambda r: r.branch_compaction_marker,
        catalogue_name="branch / compaction marker", prior_verdict="NOT MEASURED"),
    dict(id="cache_read_write_prev", family="Cache & economics", availability="LIVE",
        kind="numeric", col="prev_cache_read", feature_of=lambda r: r.prev_cache_read,
        catalogue_name="observed cache read / write", prior_verdict="HELPS"),
    dict(id="expected_reusable_tokens", family="Cache & economics", availability="OFFLINE",
        kind="numeric", col="saved_unique", feature_of=lambda r: r.saved_unique,
        catalogue_name="expected reusable tokens", prior_verdict="HELPS"),
]

ALIASES = [
    dict(catalogue_name="recency (SinceLastMs)", alias_of="gap_since_last_ms",
        family="User behaviour", availability="LIVE", prior_verdict="HELPS",
        note="Identical array as 'inter-request gap distribution & survival': "
             "SinceLastMs at decision time IS the gap the hazard curve is built from. "
             "Measured once under gap_since_last_ms rather than twice."),
    dict(catalogue_name="active vs idle, right now", alias_of="gap_since_last_ms",
        family="Agent state", availability="OFFLINE", prior_verdict="HELPS",
        note="The running proxy's idle/active state at any instant is a threshold on the "
             "same elapsed-since-last-request quantity gap_since_last_ms measures; outcome "
             "(c)'s threshold sweep on that feature already IS the 'ping when idle crosses "
             "a threshold' policy this row describes."),
    dict(catalogue_name="per-tenant historical probability, tuned", alias_of="return_probability_band_hazard",
        family="User behaviour", availability="OFFLINE", prior_verdict="HARMS",
        note="Same kvcache.History cell ((tenant, model, hour-bucket)) as 'return "
             "probability (band/hazard)'. The prior HARMS verdict was about USING it as a "
             "per-tenant-TUNED gate (a policy choice), not a second measured quantity -- "
             "reproduced here via return_probability_band_hazard's own tenant-stratified "
             "confounding check and outcome (c) threshold sweep."),
    dict(catalogue_name="session length (turns, wall-clock)", alias_of="turn_index, session_age_ms",
        family="User behaviour", availability="LIVE", prior_verdict="NOT MEASURED",
        note="Names two quantities this sweep already measures separately: turn count is "
             "turn_index, wall-clock span is session_age_ms. No third computation."),
]

SPECIAL_NOT_A_FEATURE = dict(
    catalogue_name="count of subsequent compatible requests", family="Cache & economics",
    availability="OFFLINE", prior_verdict="OUTCOME ONLY",
    reason="Defined by a forward scan past the decision point (site/gen/p_features.py's own "
           "verdict: 'a future quantity: usable as a label, never as a feature'). Coverage "
           "and distribution are reported below for completeness; it is deliberately NOT "
           "run through the 3-outcome battery, because doing so would use the row's own "
           "future to predict its own future -- exactly the backwards-only violation the "
           "brief warns against, not a second independent signal.")

PRIVACY_EXCLUDED = dict(
    catalogue_name="text cues in the last message (“wait”, “done”, TODO, error)",
    family="Conversation semantics", availability="OFFLINE", prior_verdict="NOT MEASURED",
    reason="Reading it means reading message text. This sweep's own privacy constraint "
           "(no transcript content in any output) makes that a hard no regardless of the "
           "prior study's own note that it was 'deliberately out of scope on privacy "
           "grounds' -- restated rather than silently dropped.")

# The 14 IMPOSSIBLE-AT-PROXY / NEEDS-INSTRUMENTATION rows: not measured because they do not
# exist, transcribed from the catalogue for the report's absent-feature section, not
# recomputed (there is nothing to compute).
ABSENT_ROWS = [
    ("active hours in tenant-local time", "IMPOSSIBLE AT PROXY",
     "requests.ts is UTC-only; no tenants row carries a timezone."),
    ("project / workspace switching", "IMPOSSIBLE AT PROXY",
     "no project/workspace identifier exists anywhere in requests or request_content."),
    ("remaining dollar budget", "NEEDS INSTRUMENTATION",
     "tenant_spend is a monthly rollup, not a live remaining-budget gauge."),
    ("phase (planning / executing / reviewing)", "IMPOSSIBLE AT PROXY",
     "no such field; would require the client to declare it."),
    ("error / retry / test outcome", "IMPOSSIBLE AT PROXY",
     "requests.status is the proxy's own HTTP response status, not task-level success."),
    ("task completion", "IMPOSSIBLE AT PROXY", "no task-level signal crosses the proxy."),
    ("tool start / elapsed / remaining, per call", "NEEDS INSTRUMENTATION",
     "tool_uses has only session-level first_ts/last_ts, no per-invocation row."),
    ("success / failure, per call", "NEEDS INSTRUMENTATION", "no per-call outcome field exists."),
    ("tool-completion -> next-request probability", "NEEDS INSTRUMENTATION",
     "tool_uses cannot be joined to the specific requests row whose gap it explains."),
    ("parent / child session ids", "IMPOSSIBLE AT PROXY",
     "0 of 264,163 session_ids show a nested pattern; archived_sessions has no parent column."),
    ("child completion -> parent continuation", "IMPOSSIBLE AT PROXY",
     "same root cause: no hierarchy is recorded anywhere."),
    ("subagent instrumentation, going forward", "NEEDS INSTRUMENTATION",
     "would need the client to transmit a parent-session marker; none is transmitted today."),
    ("learned workflow state / transition motifs", "IMPOSSIBLE AT PROXY",
     "depends on the phase and per-call tool features above, neither of which exist."),
    ("gateway ping-rate capacity / opportunity cost", "NEEDS INSTRUMENTATION",
     "not recorded anywhere; an unmeasured precondition on any recommendation that raises "
     "the ping cap."),
]


# ── per-feature analysis ─────────────────────────────────────────────────────

def coverage_stats(df, col: str) -> dict:
    n = len(df)
    non_missing = int(df[col].notna().sum())
    return {
        "n_decision_points": n,
        "n_non_missing": non_missing,
        "pct_missing": 100.0 * (n - non_missing) / n if n else None,
        "n_conversations": int(df["conv_key"].nunique()),
        "n_conversations_with_value": int(df.loc[df[col].notna(), "conv_key"].nunique()),
        "n_tenants": int(df["tenant"].nunique()),
        "n_tenants_with_value": int(df.loc[df[col].notna(), "tenant"].nunique()),
    }


def distribution_stats(df, col: str, kind: str) -> dict:
    sub = df[col].dropna()
    if kind == "numeric":
        if sub.empty:
            return {"kind": "numeric", "n": 0}
        qs = sub.quantile([0.01, 0.05, 0.25, 0.5, 0.75, 0.95, 0.99]).to_dict()
        return {"kind": "numeric", "n": int(len(sub)), "mean": float(sub.mean()),
                "std": float(sub.std()), "min": float(sub.min()), "max": float(sub.max()),
                "quantiles": {f"p{int(k*100)}": float(v) for k, v in qs.items()},
                "heavy_tail": bool(sub.max() > sub.quantile(0.99) * 3) if sub.quantile(0.99) else None}
    counts = sub.value_counts()
    return {"kind": "categorical", "n": int(len(sub)), "n_categories": int(counts.size),
            "top_categories": [{"category": str(k), "n": int(v), "pct": 100.0 * v / len(sub)}
                               for k, v in counts.head(15).items()]}


def analyze_feature(entry: dict, df, train_rows, test_rows, *, prices, semantics, schedule,
                    train_cut_ms: int, hi_ms: int, reps: int, seed: int, plot_dir: Path,
                    local_test=None, base_conv_test=None, pingall_conv_test=None) -> dict:
    fid, col, kind = entry["id"], entry["col"], entry["kind"]
    out = {"id": fid, "catalogue_name": entry["catalogue_name"], "family": entry["family"],
          "availability": entry["availability"], "prior_verdict": entry["prior_verdict"],
          "kind": kind}
    out["coverage"] = coverage_stats(df, col)
    out["distribution"] = distribution_stats(df, col, kind)

    outcomes: dict = {}
    pvalues: dict[str, float] = {}

    if kind == "numeric":
        for oc, oc_name in (("in_1h_or_earlier", "a_in_1h_or_earlier"), ("in_5m", "a_in_5m")):
            auc = bootstrap_auc(df, col, oc, reps=reps, seed=seed)
            bins = numeric_bins(df, col, oc)
            sub = df[[col, oc]].dropna()
            p = float("nan")
            if sub[oc].nunique() == 2 and len(sub) >= 20:
                from scipy.stats import mannwhitneyu
                x1 = sub.loc[sub[oc] == 1, col]
                x0 = sub.loc[sub[oc] == 0, col]
                if len(x1) and len(x0):
                    _, p = mannwhitneyu(x1, x0, alternative="two-sided")
            outcomes[oc_name] = {"auc": auc, "bins": bins, "p_value": p}
            pvalues[oc_name] = p
        pooled_effect = outcomes["a_in_1h_or_earlier"]["auc"].get("auc")
        b_auc = bootstrap_auc(df, col, "next_compatible_f", reps=reps, seed=seed)
        b_bins = numeric_bins(df, col, "next_compatible_f")
        subb = df[[col, "next_compatible_f"]].dropna()
        pb = float("nan")
        if subb["next_compatible_f"].nunique() == 2 and len(subb) >= 20:
            from scipy.stats import mannwhitneyu
            x1 = subb.loc[subb["next_compatible_f"] == 1, col]
            x0 = subb.loc[subb["next_compatible_f"] == 0, col]
            if len(x1) and len(x0):
                _, pb = mannwhitneyu(x1, x0, alternative="two-sided")
        outcomes["b_compatible_given_arrived"] = {"auc": b_auc, "bins": b_bins, "p_value": pb}
        pvalues["b_compatible_given_arrived"] = pb
    else:
        for oc, oc_name in (("in_1h_or_earlier", "a_in_1h_or_earlier"), ("in_5m", "a_in_5m")):
            rates = categorical_rates(df, col, oc)
            contrast = headline_categorical_contrast(rates)
            outcomes[oc_name] = {"rates": rates, "headline_contrast": contrast}
            pvalues[oc_name] = contrast["p_value"] if contrast else float("nan")
        pooled_contrast = outcomes["a_in_1h_or_earlier"]["headline_contrast"]
        pooled_effect = ((pooled_contrast["rate_a"] - pooled_contrast["rate_b"])
                        if pooled_contrast else None)
        rates_b = categorical_rates(df, col, "next_compatible_f")
        contrast_b = headline_categorical_contrast(rates_b)
        outcomes["b_compatible_given_arrived"] = {"rates": rates_b, "headline_contrast": contrast_b}
        pvalues["b_compatible_given_arrived"] = contrast_b["p_value"] if contrast_b else float("nan")

    # confounding check, all 5 of the brief's stratifiers (stop_reason / tenant / model /
    # turn index / hour of day) -- skip stratifying a column by itself or by its own bucketed
    # version, which would be a tautology rather than a check.
    self_cols = {col, {"turn_index": "turn_tercile", "hour_of_day": "hour_bucket"}.get(col, "")}
    confound_by_stratum = {}
    for strata_col in STRATA_COLS:
        if strata_col in self_cols:
            continue
        confound_by_stratum[strata_col] = stratified_check(
            df, col, "in_1h_or_earlier", strata_col, kind=kind)
    confound = {"pooled_effect": pooled_effect, "by_stratum": confound_by_stratum,
               "disagreement": summarize_disagreement(pooled_effect, confound_by_stratum, kind)}

    feature_of = entry["feature_of"]
    dollar = tune_and_score_dollar_value(train_rows, test_rows, feature_of, kind,
                                         prices=prices, semantics=semantics, schedule=schedule,
                                         train_cut_ms=train_cut_ms, hi_ms=hi_ms,
                                         reps=reps, seed=seed, local_test=local_test,
                                         base_conv_test=base_conv_test,
                                         pingall_conv_test=pingall_conv_test)
    outcomes["c_dollar_value"] = dollar
    if dollar and dollar.get("point") is not None:
        pvalues["c_dollar_value"] = p_from_ci(dollar["point"], *dollar["ci95"])
    else:
        pvalues["c_dollar_value"] = float("nan")

    out["outcomes"] = outcomes
    out["pvalues_raw"] = pvalues
    out["confounding"] = confound

    if fid == "stop_reason":
        out["confounding_idle_ge_280s"] = stop_reason_idle_trigger_check(df)

    plot_feature(entry, df, outcomes, out["distribution"], plot_dir)
    return out


def plot_feature(entry: dict, df, outcomes: dict, dist: dict, plot_dir: Path) -> None:
    import matplotlib
    matplotlib.use("svg")
    import matplotlib.pyplot as plt

    fid, kind = entry["id"], entry["kind"]
    fig, ax = plt.subplots(figsize=(7, 4.2))
    oc = outcomes["a_in_1h_or_earlier"]
    dollar = outcomes.get("c_dollar_value") or {}

    if kind == "numeric":
        bins = oc["bins"]
        if bins:
            xs = list(range(len(bins)))
            rates = [b["rate"] if b["rate"] is not None else np.nan for b in bins]
            lo = [b["ci95"][0] for b in bins]
            hi = [b["ci95"][1] for b in bins]
            err_lo = [max(0, r - l) if not math.isnan(r) else 0 for r, l in zip(rates, lo)]
            err_hi = [max(0, h - r) if not math.isnan(r) else 0 for r, h in zip(rates, hi)]
            ax.errorbar(xs, rates, yerr=[err_lo, err_hi], fmt="o-", capsize=3, color="#2b6cb0")
            for x, b in zip(xs, bins):
                ax.annotate(f"n={b['n']:,}", (x, rates[x] if not math.isnan(rates[x]) else 0),
                           textcoords="offset points", xytext=(0, 8), fontsize=7, ha="center")
            ax.set_xticks(xs)
            ax.set_xticklabels([b["bin"] for b in bins], rotation=35, ha="right", fontsize=7)
        ax.set_xlabel(f"{entry['catalogue_name']} (quantile bin)")
    else:
        rates = oc["rates"][:12]
        xs = list(range(len(rates)))
        vals = [r["rate"] if r["rate"] is not None else 0 for r in rates]
        lo = [r["ci95"][0] if not math.isnan(r["ci95"][0]) else v for r, v in zip(rates, vals)]
        hi = [r["ci95"][1] if not math.isnan(r["ci95"][1]) else v for r, v in zip(rates, vals)]
        err_lo = [max(0, v - l) for v, l in zip(vals, lo)]
        err_hi = [max(0, h - v) for v, h in zip(vals, hi)]
        ax.bar(xs, vals, color="#2b6cb0")
        ax.errorbar(xs, vals, yerr=[err_lo, err_hi], fmt="none", ecolor="black", capsize=3)
        for x, r in zip(xs, rates):
            ax.annotate(f"n={r['n']:,}", (x, vals[x]), textcoords="offset points",
                       xytext=(0, 6), fontsize=7, ha="center")
        ax.set_xticks(xs)
        ax.set_xticklabels([r["category"] for r in rates], rotation=35, ha="right", fontsize=7)
        ax.set_xlabel(entry["catalogue_name"])

    ax.set_ylabel("P(next compatible request arrives within 1h | this feature) -- rate, "
                  "with 95% CI", fontsize=8)
    ax.set_title(f"{entry['catalogue_name']}\nvs. return-within-band hazard", fontsize=10)
    ax.set_ylim(bottom=0)
    fig.tight_layout()
    fig.savefig(plot_dir / f"{fid}.svg", format="svg")
    plt.close(fig)

    caption = caption_for(entry, outcomes, dist)
    (plot_dir / f"{fid}.txt").write_text(caption)


def caption_for(entry: dict, outcomes: dict, dist: dict) -> str:
    name = entry["catalogue_name"]
    kind = entry["kind"]
    oc = outcomes["a_in_1h_or_earlier"]
    dollar = outcomes.get("c_dollar_value") or {}
    if kind == "numeric":
        auc = oc["auc"].get("auc")
        auc_txt = (f"ranking by this value alone predicts whether the next compatible "
                  f"request lands within an hour with an AUC of {auc:.2f} "
                  f"(0.5 = no better than a coin flip, 1.0 = perfect)") if auc is not None \
            else "there was not enough data to rank by this value at all"
    else:
        contrast = oc.get("headline_contrast")
        if contrast:
            auc_txt = (f"its two most common values show return rates of "
                      f"{100*contrast['rate_a']:.1f}% vs {100*contrast['rate_b']:.1f}% "
                      f"(p={contrast['p_value']:.3g})")
        else:
            auc_txt = "there were not two categories with enough decision points to compare"
    if dollar and dollar.get("point") is not None:
        lo, hi = dollar["ci95"]
        f5 = dollar.get("vs_fixed_5m", {})
        money_txt = (f"Pinging whenever it crossed the tuned rule would have changed cost "
                    f"by ${dollar['point']:,.2f} on the held-out test window (95% CI "
                    f"${lo:,.2f} to ${hi:,.2f}) compared with pinging EVERY eligible request "
                    f"regardless of this feature -- the real competitor, since that "
                    f"strawman already beats the older fixed-5-minute baseline by itself "
                    f"(${f5.get('point', 0):,.2f} there too, for reference).")
    else:
        money_txt = "There was not enough train-window data to tune a ping rule for this one."
    return (f"'{name}' (measured={dist.get('n', 0):,} decision points): {auc_txt}. {money_txt} "
           f"A big difference in this chart is not the same thing as money saved -- see the "
           f"report for whether this feature survives the stop_reason confounding check.")


def stop_reason_idle_trigger_check(df) -> dict:
    """The brief's central trap, demonstrated directly: stop_reason's relationship to
    return timing, marginal vs. restricted to decision points that already survived to the
    production ping trigger's own 280s idle delay (PingSchedule.idle_5m_ms)."""
    marginal = categorical_rates(df, "stop_reason", "in_1h_or_earlier")
    cond = categorical_rates(df[df["idle_ge_280s"] == True], "stop_reason", "in_1h_or_earlier")  # noqa: E712
    return {
        "idle_trigger_ms": IDLE_5M_MS,
        "marginal_rates": marginal,
        "conditional_on_idle_ge_280s_rates": cond,
        "note": "marginal = every decision point; conditional = restricted to points whose "
                "observed forward gap already reached the 280s ping-trigger delay (a "
                "population definition using the row's OWN already-elapsed idle time at "
                "the trigger's firing instant, not a feature used to predict anything -- "
                "landmark analysis, not leakage). If stop_reason's spread collapses in the "
                "conditional table relative to the marginal one, the marginal signal was "
                "largely explained by the trigger's own conditioning.",
    }


def assign_verdict(entry_out: dict) -> tuple[str, str]:
    """HELPS / NO SIGNAL / HARMS AS A GATE / NOT IDENTIFIABLE, from the measured numbers,
    never asserted ahead of them. Decided against ping-everyone, not fixed-5m: beating
    fixed-5m is easy (an unconditional ping already does, by ~+5.7% on this snapshot's test
    window -- see ping_everyone_actions), so a feature only earns HELPS by clearing the
    harder bar. dollar['point']/['ci95'] are the vs-ping-everyone numbers (see
    tune_and_score_dollar_value); vs-fixed-5m is reported alongside for continuity with the
    rest of this study's own convention, not used for the verdict."""
    dollar = entry_out["outcomes"].get("c_dollar_value") or {}
    q_c = entry_out.get("fdr", {}).get("c_dollar_value")
    if dollar.get("point") is None:
        return ("NOT IDENTIFIABLE", "not enough train-window data to tune a ping rule for "
                "this feature")
    lo, hi = dollar["ci95"]
    survives = bool(q_c is not None and not math.isnan(q_c) and q_c < 0.05)
    if lo is not None and hi is not None and lo > 0:
        if survives:
            return ("HELPS", f"tuned rule beats ping-everyone by ${dollar['point']:,.0f} on "
                    f"test (CI excludes 0, survives FDR q={q_c:.3g})")
        return ("HELPS, NOT FDR-SIGNIFICANT", f"tuned rule beats ping-everyone by "
                f"${dollar['point']:,.0f} on test but does not survive multiple-comparison "
                f"correction (q={q_c:.3g} if computable)")
    if hi is not None and lo is not None and hi < 0:
        return ("HARMS AS A GATE", f"tuned rule costs ${-dollar['point']:,.0f} more than "
                f"just pinging everyone above MIN_PREFIX, CI entirely negative")
    return ("NO SIGNAL", "outcome (c) CI vs. ping-everyone on the held-out test window "
            "includes zero -- the feature adds nothing beyond an unconditional ping")


def run(args) -> dict:
    prices = PriceBook.from_operator_file(args.prices)
    semantics, schedule = Semantics(), PingSchedule()

    print("loading rows...", file=sys.stderr)
    rows = load_feature_rows(args.db)
    if not rows:
        raise SystemExit("no requests in that store")
    tool_uses = load_tool_uses(args.db)
    digest_tr = load_digest_transitions(args.db)
    attach_derived(rows, tool_uses, digest_tr)

    tenants = sorted({r.user for r in rows})
    tenant_pseudo = pseudonymize_map(tenants)
    print(f"{len(rows):,} decision points, {len(tenants)} tenants, "
         f"{len({r.key for r in rows}):,} conversations", file=sys.stderr)

    df = build_frame(rows, tenant_pseudo)

    lo_ms, hi_ms = rows[0].ts_ms, max(r.ts_ms for r in rows)
    train_cut_ms = lo_ms + int((hi_ms - lo_ms) * args.train_frac)
    train_rows = [r for r in rows if r.ts_ms <= train_cut_ms]
    test_rows = [r for r in rows if r.ts_ms > train_cut_ms]
    print(f"train <= {train_cut_ms} ({len(train_rows):,} rows); "
         f"test > train_cut ({len(test_rows):,} rows)", file=sys.stderr)

    plot_dir = Path(args.out_plots)
    plot_dir.mkdir(parents=True, exist_ok=True)

    # shared outcome-(c) test-window artifacts: identical for every feature (none of them
    # depend on it), computed once rather than 19 times. See ping_everyone_actions's own
    # docstring for why the ping-everyone comparison exists at all.
    local_test = derive([dc_replace(r) for r in test_rows])
    base_conv_test = per_conversation_costs(local_test, fixed_5m_actions(local_test),
                                            prices, semantics, schedule, hi_ms)
    pingall_conv_test = per_conversation_costs(local_test, ping_everyone_actions(local_test),
                                               prices, semantics, schedule, hi_ms)
    pingall_vs_fixed5m = bootstrap_ci(base_conv_test, pingall_conv_test,
                                      reps=args.bootstrap_reps, seed=args.seed)
    print(f"  reference: ping-everyone (MIN_PREFIX-eligible) vs fixed-5m on test window: "
         f"${pingall_vs_fixed5m['point_delta_usd']:,.2f} "
         f"({pingall_vs_fixed5m['point_delta_pct']:.2f}%) -- the strawman every feature's "
         f"own rule is scored against below, not just fixed-5m.", file=sys.stderr)

    features_out = []
    for entry in CANON:
        print(f"  analyzing {entry['id']}...", file=sys.stderr)
        features_out.append(analyze_feature(
            entry, df, train_rows, test_rows, prices=prices, semantics=semantics,
            schedule=schedule, train_cut_ms=train_cut_ms, hi_ms=hi_ms,
            reps=args.bootstrap_reps, seed=args.seed, plot_dir=plot_dir,
            local_test=local_test, base_conv_test=base_conv_test,
            pingall_conv_test=pingall_conv_test))

    # Tenant identity is not one of the catalogue's 38 rows (it's the confound behind every
    # other row, not a candidate signal on its own) -- added as its own card because it is
    # the single strongest univariate split in the corpus and the break-even comparison is
    # a clean, requested negative result. Pseudonyms only; feature_of never returns the raw
    # tenant_id (it closes over tenant_pseudo, built just above).
    print("  analyzing tenant_identity...", file=sys.stderr)
    tenant_entry = dict(id="tenant_identity", family="Confound context", availability="LIVE",
                        kind="categorical", col="tenant", prior_verdict="n/a (not a catalogue row)",
                        catalogue_name="tenant identity",
                        feature_of=lambda r, tp=tenant_pseudo: tp[r.user])
    tenant_out = analyze_feature(tenant_entry, df, train_rows, test_rows, prices=prices,
                                 semantics=semantics, schedule=schedule,
                                 train_cut_ms=train_cut_ms, hi_ms=hi_ms,
                                 reps=args.bootstrap_reps, seed=args.seed, plot_dir=plot_dir,
                                 local_test=local_test, base_conv_test=base_conv_test,
                                 pingall_conv_test=pingall_conv_test)
    per_tenant_band = (df[df["idle_ms"].notna()].groupby("tenant")
                      .agg(n=("band", "size"),
                           band_rate_pct=("band", lambda s: 100 * (s == "in_1h").mean()))
                      .query("n >= 1000").sort_values("n", ascending=False))
    tenant_out["break_even_pct"] = BREAK_EVEN_PCT
    tenant_out["max_tenant_band_rate_pct"] = float(per_tenant_band["band_rate_pct"].max())
    tenant_out["any_tenant_clears_break_even"] = bool(
        (per_tenant_band["band_rate_pct"] > BREAK_EVEN_PCT).any())
    tenant_out["per_tenant_band_rate"] = [
        {"tenant": t, "n": int(row["n"]), "band_rate_pct": float(row["band_rate_pct"])}
        for t, row in per_tenant_band.iterrows()]

    # Direct test of "ping whenever tenant==X loses money for every X" via the SAME full
    # cost-model simulation the rest of this sweep uses, rather than inheriting the myopic
    # r/(w-r) band-rate comparison unchecked: PingSchedule only actually pays for a ping
    # when idle time crosses its interval, so a full replay can (and, as the ping-everyone
    # reference above shows, does) come out more favorably than the simple formula.
    print("  tenant_identity: per-tenant 'ping when tenant==X' dollar test...", file=sys.stderr)
    per_tenant_dollar = []
    for t in sorted(tenant_pseudo.values()):
        t_test_rows = [r for r in local_test if tenant_pseudo[r.user] == t]
        if len(t_test_rows) < 200:
            per_tenant_dollar.append({"tenant": t, "n": len(t_test_rows), "skipped": True})
            continue
        actions_t = {r.request_id: gate_action(r.cached_context, True) for r in t_test_rows}
        for r in local_test:
            if tenant_pseudo[r.user] != t:
                actions_t.setdefault(r.request_id, WRITE_5M)
        conv_t = per_conversation_costs(local_test, actions_t, prices, semantics, schedule, hi_ms)
        vs_f5 = bootstrap_ci(base_conv_test, conv_t, reps=args.bootstrap_reps, seed=args.seed)
        vs_pe = bootstrap_ci(pingall_conv_test, conv_t, reps=args.bootstrap_reps, seed=args.seed)
        per_tenant_dollar.append({
            "tenant": t, "n": len(t_test_rows), "skipped": False,
            "vs_fixed_5m_usd": vs_f5["point_delta_usd"], "vs_fixed_5m_ci95": vs_f5["ci95_delta_usd"],
            "vs_ping_everyone_usd": vs_pe["point_delta_usd"],
            "vs_ping_everyone_ci95": vs_pe["ci95_delta_usd"]})
    tenant_out["per_tenant_ping_dollar_value"] = per_tenant_dollar
    tenant_out["any_tenant_beats_fixed_5m"] = any(
        d.get("vs_fixed_5m_ci95", [None, None])[0] is not None and d["vs_fixed_5m_ci95"][0] > 0
        for d in per_tenant_dollar if not d.get("skipped"))
    tenant_out["any_tenant_beats_ping_everyone"] = any(
        d.get("vs_ping_everyone_ci95", [None, None])[0] is not None and d["vs_ping_everyone_ci95"][0] > 0
        for d in per_tenant_dollar if not d.get("skipped"))

    # tenant_identity is not part of the 18-feature BH-FDR pool (it isn't a catalogue row);
    # its own verdict runs on its own un-adjusted fdr={}, stated explicitly rather than
    # silently borrowing significance from a correction pass it was never a member of.
    tenant_out["fdr"] = {}
    verdict, reason = assign_verdict(tenant_out)
    tenant_out["verdict"], tenant_out["verdict_reason"] = verdict, reason


    # ---- BH-FDR across every feature x outcome test -----------------------------------
    test_keys: list[tuple[int, str]] = []
    pvals: list[float] = []
    for fi, f in enumerate(features_out):
        for oc_name, p in f["pvalues_raw"].items():
            test_keys.append((fi, oc_name))
            pvals.append(p)
    qvals = bh_fdr(pvals)
    n_tested = sum(1 for p in pvals if not math.isnan(p))
    n_survive = sum(1 for q in qvals if q is not None and not math.isnan(q) and q < 0.05)
    for f in features_out:
        f["fdr"] = {}
    for (fi, oc_name), q in zip(test_keys, qvals):
        features_out[fi]["fdr"][oc_name] = q

    for f in features_out:
        verdict, reason = assign_verdict(f)
        f["verdict"] = verdict
        f["verdict_reason"] = reason

    # ---- special / alias / absent rows, transcribed rather than computed --------------
    # coverage/distribution only for the forward-looking "count of subsequent compatible
    # requests" -- computed here, once, purely descriptively; never fed to analyze_feature.
    by_conv_fwd: dict = {}
    for r in rows:
        by_conv_fwd.setdefault(r.key, []).append(r)
    counts_fwd = []
    for group in by_conv_fwd.values():
        group = sorted(group, key=lambda r: (r.ts_ms, r.request_id))
        compat_suffix = 0
        for r in reversed(group):
            counts_fwd.append(compat_suffix)
            if r.next_compatible:
                compat_suffix += 1
    counts_fwd = np.array(counts_fwd, dtype=float)
    special = dict(SPECIAL_NOT_A_FEATURE)
    special["coverage"] = {"n_decision_points": len(rows), "n_conversations": len(by_conv_fwd)}
    special["distribution"] = {
        "kind": "numeric", "n": int(len(counts_fwd)), "mean": float(counts_fwd.mean()),
        "quantiles": {f"p{p}": float(np.quantile(counts_fwd, p / 100)) for p in (50, 90, 99)},
        "pct_zero": float(100 * (counts_fwd == 0).mean()),
    }

    evidence = {
        "generated_by": "deploy/harbor/kv_ttl_feature_univariate.py",
        "command": " ".join(sys.argv),
        "db": args.db, "prices": args.prices,
        "n_decision_points": len(rows), "n_conversations": len({r.key for r in rows}),
        "n_tenants": len(tenants), "window_ms": {"since": lo_ms, "until": hi_ms},
        "train_frac": args.train_frac, "train_cut_ms": train_cut_ms,
        "bootstrap_reps": args.bootstrap_reps, "seed": args.seed,
        "multiple_comparisons": {
            "n_tests_raw": len(pvals), "n_tests_with_a_pvalue": n_tested,
            "n_survive_fdr_0.05": n_survive, "method": "Benjamini-Hochberg",
        },
        "ping_everyone_vs_fixed_5m_reference": {
            "note": "The strawman every feature's own outcome-(c) rule is scored against "
                    "(see ping_everyone_actions docstring): pinging every MIN_PREFIX-"
                    "eligible row, no feature involved, already beats fixed-5m on this test "
                    "window. Any rule covering most of that population will look good vs. "
                    "fixed-5m for this reason alone, which is why verdicts are decided "
                    "against this number, not against fixed-5m.",
            "point_delta_usd": pingall_vs_fixed5m["point_delta_usd"],
            "point_delta_pct": pingall_vs_fixed5m["point_delta_pct"],
            "ci95_delta_usd": pingall_vs_fixed5m["ci95_delta_usd"],
        },
        "features": features_out,
        "tenant_identity": tenant_out,
        "aliases": [dict(a) for a in ALIASES],
        "not_a_feature": special,
        "absent": [{"catalogue_name": n, "availability": a, "reason": r}
                   for n, a, r in ABSENT_ROWS],
        "privacy_excluded": [PRIVACY_EXCLUDED],
    }
    return evidence


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--db", default=DEFAULT_DB)
    ap.add_argument("--prices", default=DEFAULT_PRICES)
    ap.add_argument("--out-json", required=True)
    ap.add_argument("--out-plots", required=True)
    ap.add_argument("--train-frac", type=float, default=0.6)
    ap.add_argument("--bootstrap-reps", type=int, default=400)
    ap.add_argument("--seed", type=int, default=0)
    args = ap.parse_args(argv)

    evidence = run(args)
    out_path = Path(args.out_json)
    out_path.parent.mkdir(parents=True, exist_ok=True)
    out_path.write_text(json.dumps(evidence, indent=2, default=str))
    print(f"wrote {out_path}", file=sys.stderr)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
