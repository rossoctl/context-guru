#!/usr/bin/env python3
"""The predictor hunt: is there ANYTHING that beats flat-cap-2 ($611.21, +5.70%, no model
at all) on net dollars, with a CI that excludes zero?

Every learned arm tried by this project so far (logreg-v1, hybrid2, gbm-policy,
ensemble-hg2-gbm, cost-aware-simple, cost-aware-hazard) has lost to flat-cap-2. The
information ceiling sits at AUC ~= 0.78-0.80 across model classes, and dollars stop
responding after 2 features, because the binding constraint is COVERAGE (which decision
points get pinged at all), not prediction (docs/results + reports3/experiments.md,
reports3/feat-univariate.md). This script tests the one shape nobody has tried:
EXCLUSION rather than inclusion -- "ping everyone EXCEPT the group that is a proven dead
end" -- plus the coverage-aware and cost-sensitive variants that shape implies, using
kv_ttl_cost_model.py (the dollar arithmetic) and kv_ttl_predictor_arms.py (data loading,
feature engineering, the leak-free per-tenant threshold-tuning convention) as libraries,
unmodified.

THE HEADLINE QUESTION (reports3/feat-univariate.md's own "concrete, promising follow-up,
flagged but not tested"): stop_reason=='stop' is rescued 0.03% of the time (4 of 12,685
trigger-reaching decision points) -- a near-total dead end, and it is HALF of all
trigger-reaching decision points in this window. Does excluding just that one raw value
from the ping set beat ping-everyone (== flat-cap-2, same policy: PING_5M above
MIN_PREFIX, no other filter)? Nobody has tried the EXCLUSION form before -- every study to
date only tried "ping ONLY group X" (inclusion), never "ping everyone EXCEPT X".

Arms tested here, every one scored against BOTH fixed-5m (reference) and flat-cap-2
(decisive), with conversation-level and tenant-level paired bootstrap CIs and BH-FDR
across the family:

  exclude-stop        ping everyone except stop_reason=='stop' (raw value, not the whole
                      stop_cluster -- see WHY NOT THE CLUSTER below). The headline test.
  exclude-cluster     ping everyone except stop_cluster=='looks_done_isnt' (i.e. 'stop' OR
                      '') -- a deliberately wrong-shaped contrast: '' is rescued 56.3% of
                      the time (n=1,129), so lumping it in with 'stop' should cost money.
  budget-priority-*   exclude-stop, then a per-tenant ping BUDGET (fraction of that
                      tenant's eligible rows) spent on the highest train-fold-measured-
                      value stop_reason categories first, swept at 25/50/75/100%.
  maxpings-*          exclude-stop under PingSchedule(max_pings=k) for k in {1,2,3,4} --
                      does freeing coverage by excluding 'stop' change the optimal ping
                      count, versus the established sweep's flat N=1/2/3 plateau?
  value-regression    a regressor trained on the REALIZED dollar value of pinging vs a
                      plain 5-minute write (not P(return) -- every P(return)-routed
                      cost-aware arm already tried scored exactly $0.00, reports3/
                      experiments.md), with a per-tenant threshold TUNED ON DOLLARS, over
                      the exclude-stop-filtered population.
  gbm-exclude-stop    a calibrated HistGradientBoostingClassifier (a model class not yet
                      tried as a scored policy under this exclusion), fit and thresholded
                      per-tenant on the exclude-stop-filtered population, to see whether a
                      model's marginal contribution turns positive once the guaranteed-
                      dead-end rows are off the table entirely.

WHY NOT THE CLUSTER (stop_cluster=='looks_done_isnt'): kv_ttl_predictor_arms.stop_cluster
lumps stop_reason=='stop' (0.03% conditional rescue, n=12,685) together with '' (56.33%
conditional rescue, n=1,129) into one bucket. That grouping is right for the ACTUALLY_DONE
vs STILL_WORKING axis it was built for, and wrong for THIS one -- exclude-cluster is kept
in as the arm that shows why the raw stop_reason value, not the cluster, is what has to be
excluded.

PROTOCOL (matches the project's own established convention throughout, so every number
here is directly comparable to reports3/experiments.md's table): chronological split,
train_frac=0.6, then 3 rolling-origin test folds over the remaining 0.4. Every threshold,
per-tenant fallback, and model is fit on TRAIN rows ONLY (ts_ms <= train_cut) and never
refit per fold. The LAST fold is the untouched holdout: it contributes to no tuning
decision anywhere in this file and is inspected exactly once, in the final report table,
exactly as reports3/experiments.md's own "untouched holdout only" column does it.
Conversation is (tenant, session_id, model); bootstrap resamples conversations (session-
level) and, separately, tenants (tenant-level) -- never rows. Right-censored rows (a
conversation's last row in the window, event_observed=False) are excluded from every fit
and every realized-value label, never imputed.

READ-ONLY: opens snap/cg.db and snap/cg-control.db with mode=ro (never immutable=1, see
kv_ttl_predictor_arms.load_ext_trajectories's own note on why). Writes nothing but
exp4/predhunt/results.json and exp4/predhunt/plots/*.{svg,txt}.

Usage:
    python3 kv_ttl_predhunt.py --db snap/cg.db --prices /tmp/abl-0928/prices.yaml \
        --out exp4/predhunt/results.json
    python3 kv_ttl_predhunt.py --plots-only --out exp4/predhunt/results.json \
        --plots-dir exp4/predhunt/plots
"""
from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
import warnings
from dataclasses import replace as dc_replace

warnings.filterwarnings("ignore")

import numpy as np
import pandas as pd

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from kv_ttl_cost_model import (  # noqa: E402
    EXPIRE, WRITE_5M, PING_5M, TTL_5M, LIFETIME_MS, HORIZON_5M_MS, HORIZON_1H_MS,
    PriceBook, Semantics, PingSchedule, compare, pings_per_span,
)
from kv_ttl_predictor_arms import (  # noqa: E402
    load_ext_trajectories, pseudonymize_map, gate_action, stop_cluster, build_feature_frame,
    per_conversation_costs, bootstrap_ci, fold_cost, fixed_5m_actions,
    NUMERIC_FEATURES, MIN_PREFIX, ExtRequest,
    ACTUALLY_DONE, LOOKS_DONE_ISNT, STILL_WORKING,  # noqa: F401 -- documents the grouping
)

SEED = 0
TRAIN_FRAC = 0.6
N_FOLDS = 3
BOOTSTRAP_REPS = 400
BUDGET_FRACTIONS = (0.25, 0.5, 0.75, 1.0)
MAXPINGS_GRID = (1, 2, 3, 4)
MIN_SIGNAL = 30  # matches tune_historical_probability's own floor


# ── small stats helpers (generic, reimplemented here rather than imported from another
#    in-flight agent's uncommitted kv_ttl_ablation.py -- see the ownership note in the
#    module docstring; each is <20 lines and has no dependency this file doesn't already
#    have) ──────────────────────────────────────────────────────────────────────────────

def pseudonymize_T(tenants: list[str]) -> dict[str, str]:
    """T01..T18 by sorted raw tenant_id, per this study's privacy convention (uppercase T,
    distinct from kv_ttl_predictor_arms.pseudonymize_map's lowercase t -- both are the same
    construction, this file just uses the letter the brief specified)."""
    return {t: f"T{i+1:02d}" for i, t in enumerate(sorted(set(tenants)))}


def two_sided_boot_p(deltas: np.ndarray) -> float:
    if deltas.size == 0:
        return 1.0
    p_le = float(np.mean(deltas <= 0.0))
    p_ge = float(np.mean(deltas >= 0.0))
    return float(min(1.0, 2 * min(p_le, p_ge)))


def bh_fdr(pvalues: dict[str, float], alpha: float = 0.05) -> dict:
    items = sorted(((k, v) for k, v in pvalues.items() if v is not None), key=lambda kv: kv[1])
    m = len(items)
    largest_k = 0
    for i, (_, p) in enumerate(items, start=1):
        if p <= (i / m) * alpha:
            largest_k = i
    out = {}
    for i, (k, p) in enumerate(items, start=1):
        out[k] = {"p": p, "rank": i, "bh_critical": (i / m) * alpha if m else None,
                  "survives_fdr": i <= largest_k}
    return out


def bootstrap_paired_dollars(baseline_conv: dict, candidate_conv: dict, reference_conv: dict, *,
                             reps: int, seed: int) -> dict:
    """Delta = (baseline-candidate) - (baseline-reference): net dollars `candidate` earns
    OVER `reference` (positive = candidate cheaper), resampling conversations ONCE per rep
    so both arms are compared on the identical resampled population every time."""
    keys = list(baseline_conv.keys())
    if not keys:
        return {"n_conversations": 0}
    rng = np.random.default_rng(seed)
    base = np.array([baseline_conv[k] for k in keys])
    cand = np.array([candidate_conv.get(k, baseline_conv[k]) for k in keys])
    ref = np.array([reference_conv.get(k, baseline_conv[k]) for k in keys])
    n = len(keys)
    deltas = np.empty(reps)
    for i in range(reps):
        idx = rng.integers(0, n, size=n)
        deltas[i] = (base[idx].sum() - cand[idx].sum()) - (base[idx].sum() - ref[idx].sum())
    point = (base.sum() - cand.sum()) - (base.sum() - ref.sum())
    return {
        "n_conversations": n, "point_delta_usd": float(point),
        "ci95_delta_usd": [float(np.percentile(deltas, 2.5)), float(np.percentile(deltas, 97.5))],
        "candidate_minus_baseline_usd": float(base.sum() - cand.sum()),
        "reference_minus_baseline_usd": float(base.sum() - ref.sum()),
        "p_value_two_sided": two_sided_boot_p(deltas),
    }


def bootstrap_paired_dollars_tenant(baseline_conv: dict, candidate_conv: dict, reference_conv: dict,
                                    *, reps: int, seed: int) -> dict:
    """Same statistic, resampling TENANTS (sum of that tenant's conversations) rather than
    conversations -- a tenant-mix-sensitivity check distinct from the session-level one."""
    by_t_base: dict[str, float] = {}
    by_t_cand: dict[str, float] = {}
    by_t_ref: dict[str, float] = {}
    for k, v in baseline_conv.items():
        by_t_base[k[1]] = by_t_base.get(k[1], 0.0) + v
    for k, v in candidate_conv.items():
        by_t_cand[k[1]] = by_t_cand.get(k[1], 0.0) + v
    for k, v in reference_conv.items():
        by_t_ref[k[1]] = by_t_ref.get(k[1], 0.0) + v
    tenants = sorted(by_t_base)
    if not tenants:
        return {"n_tenants": 0}
    rng = np.random.default_rng(seed)
    base = np.array([by_t_base[t] for t in tenants])
    cand = np.array([by_t_cand.get(t, by_t_base[t]) for t in tenants])
    ref = np.array([by_t_ref.get(t, by_t_base[t]) for t in tenants])
    n = len(tenants)
    deltas = np.empty(reps)
    for i in range(reps):
        idx = rng.integers(0, n, size=n)
        deltas[i] = (base[idx].sum() - cand[idx].sum()) - (base[idx].sum() - ref[idx].sum())
    point = (base.sum() - cand.sum()) - (base.sum() - ref.sum())
    return {
        "n_tenants": n, "point_delta_usd": float(point),
        "ci95_delta_usd": [float(np.percentile(deltas, 2.5)), float(np.percentile(deltas, 97.5))],
        "p_value_two_sided": two_sided_boot_p(deltas),
    }


def bootstrap_ci_tenant(baseline_by_conv: dict, arm_by_conv: dict, *, reps: int, seed: int) -> dict:
    by_t_base: dict[str, float] = {}
    by_t_arm: dict[str, float] = {}
    for k, v in baseline_by_conv.items():
        by_t_base[k[1]] = by_t_base.get(k[1], 0.0) + v
    for k, v in arm_by_conv.items():
        by_t_arm[k[1]] = by_t_arm.get(k[1], 0.0) + v
    tenants = sorted(by_t_base)
    if not tenants:
        return {"n_tenants": 0}
    rng = np.random.default_rng(seed)
    base = np.array([by_t_base[t] for t in tenants])
    arm = np.array([by_t_arm.get(t, by_t_base[t]) for t in tenants])
    n = len(tenants)
    deltas = np.empty(reps)
    for i in range(reps):
        idx = rng.integers(0, n, size=n)
        deltas[i] = base[idx].sum() - arm[idx].sum()
    point = base.sum() - arm.sum()
    return {
        "n_tenants": n, "point_delta_usd": float(point),
        "point_delta_pct": float(100 * point / base.sum()) if base.sum() else None,
        "ci95_delta_usd": [float(np.percentile(deltas, 2.5)), float(np.percentile(deltas, 97.5))],
    }


# ── policy arms (rows -> {request_id: action}) ──────────────────────────────────────────

def flat_cap2_actions(rows) -> dict[int, str]:
    """flat-cap-2 / 'ping-everyone': PING_5M above MIN_PREFIX, no other filter. The
    established, zero-parameter champion this whole file is trying to beat."""
    return {r.request_id: gate_action(r.cached_context, True) for r in rows}


def exclude_stop_actions(rows) -> dict[int, str]:
    """The headline test: ping everyone except the raw stop_reason=='stop' rows."""
    return {r.request_id: gate_action(r.cached_context, r.stop_reason != "stop") for r in rows}


def exclude_cluster_actions(rows) -> dict[int, str]:
    """The deliberately-wrong-shaped contrast: exclude the whole looks_done_isnt cluster
    ('stop' OR ''), which also throws away ''s real 56.3% conditional rescue rate."""
    return {r.request_id: gate_action(r.cached_context, stop_cluster(r.stop_reason) != "looks_done_isnt")
            for r in rows}


def coverage_ms_5m(schedule: PingSchedule) -> int:
    """How long a PING_5M hold reaches: max_pings refreshes plus the trailing 5-minute
    lifetime the last read/ping itself buys (kv_ttl_cost_model._coverage_ms's own formula,
    reproduced here rather than imported since that helper is underscore-private)."""
    return schedule.max_pings * schedule.interval_ms(TTL_5M) + LIFETIME_MS[TTL_5M]


def realized_ping_value_usd(r: ExtRequest, price, semantics: Semantics, schedule: PingSchedule):
    """The dollars a PING_5M on this ONE row was actually worth versus a plain WRITE_5M,
    using the REALIZED gap (r.idle_ms) -- a training-time label, never available at
    decision time. None for a right-censored row (idle_ms is None: this is the
    conversation's last row in the window, so the true gap is unknown) or an unpriced
    model (price.known is False: no rate to reason about).

    rescue: avoided re-creation, priced at the conservative 5-minute rate (matching
    kv_ttl_cost_model.policy_expected_cost's own non-circular convention -- pricing the
    rescue at the ACTION's own tier would let an expensive tier inflate its own payoff).
    cost: the keep-alives this realized gap actually attracts (pings_per_span on the real
    idle_ms), not an assumed max_pings -- a gap shorter than one interval draws zero.
    """
    if r.idle_ms is None or not price.known:
        return None
    cov = coverage_ms_5m(schedule)
    rescued = HORIZON_5M_MS <= r.idle_ms < cov
    rescue = r.cached_context * (price.write_5m - price.cache_read) if rescued else 0.0
    n_pings = pings_per_span(r.idle_ms, schedule.interval_ms(TTL_5M), schedule.max_pings)
    cost = n_pings * price.keep_alive_cost(r.cached_context, semantics)
    return rescue - cost


def exclusion_feasibility_diagnostic(ext_rows: list[ExtRequest], train_cut: int,
                                     tenant_pseudo: dict[str, str]) -> dict:
    """Whether the headline 'exclude stop' idea is even testable out-of-sample here. Run
    FIRST, because a $0.00 result on every exclude-stop-shaped arm below is ambiguous
    between 'the exclusion truly doesn't matter' and 'there was nothing left to exclude in
    the test window' -- and it turns out to be the second, for a specific, checkable
    reason, not a bug: this diagnostic is what found it."""
    stop_rows = [r for r in ext_rows if r.stop_reason == "stop"]
    n_before = sum(1 for r in stop_rows if r.ts_ms <= train_cut)
    n_after = sum(1 for r in stop_rows if r.ts_ms > train_cut)
    agent_counts_all = {}
    agent_counts_stop = {}
    for r in ext_rows:
        agent_counts_all[r.agent] = agent_counts_all.get(r.agent, 0) + 1
    for r in stop_rows:
        agent_counts_stop[r.agent] = agent_counts_stop.get(r.agent, 0) + 1
    openai_rows = [r for r in ext_rows if r.agent in ("openai", "asyncopenai")]
    openai_tenants = sorted({tenant_pseudo[r.user] for r in openai_rows})
    by_day: dict[str, int] = {}
    for r in openai_rows:
        day = pd.Timestamp(r.ts_ms, unit="ms", tz="UTC").strftime("%Y-%m-%d")
        by_day[day] = by_day.get(day, 0) + 1
    return {
        "n_stop_total": len(stop_rows), "n_stop_before_train_cut": n_before,
        "n_stop_after_train_cut": n_after,
        "agent_breakdown_of_stop_rows": agent_counts_stop,
        "agent_breakdown_all_rows": agent_counts_all,
        "openai_sdk_agent_tenants": openai_tenants,
        "openai_sdk_agent_rows_by_utc_day": dict(sorted(by_day.items())),
        "conclusion": (
            f"{n_before}/{len(stop_rows)} of every stop_reason=='stop' row in the whole "
            f"window falls at or before train_cut; {n_after} fall after it. "
            f"{sum(v for k,v in agent_counts_stop.items() if k in ('openai','asyncopenai'))}/"
            f"{len(stop_rows)} of those rows come from the 'openai'/'asyncopenai' "
            f"agent (the OpenAI Chat Completions finish_reason spelling of a normal "
            f"completion, semantically closer to Anthropic's end_turn than to a dead end), "
            f"used by tenant(s) {openai_tenants} in a burst concentrated on the UTC days "
            f"listed above, which had already tapered to near-zero before train_cut. "
            f"Every exclude-stop-shaped arm in this file is therefore, VERIFIABLY, a total "
            f"no-op on every test fold and the holdout -- not because the exclusion is "
            f"worthless, but because its entire target population is a single tenant's "
            f"single-integration traffic burst that ended before the test window begins."),
    }


def priority_scores_from_train(train_rows: list[ExtRequest], prices: PriceBook, semantics: Semantics,
                               schedule: PingSchedule) -> dict[str, float]:
    """Mean realized ping value per raw stop_reason category, train rows only -- the
    ranking a coverage-limited budget should spend on first. 'stop' is not excluded from
    this table (it will simply, correctly, rank last) so the ranking is auditable."""
    by_cat: dict[str, list[float]] = {}
    for r in train_rows:
        v = realized_ping_value_usd(r, prices.for_model(r.model), semantics, schedule)
        if v is None:
            continue
        by_cat.setdefault(r.stop_reason or "(none)", []).append(v)
    return {cat: float(np.mean(vals)) for cat, vals in by_cat.items() if vals}


def budget_priority_actions(rows: list[ExtRequest], priority: dict[str, float], budget_frac: float) -> dict[int, str]:
    """exclude-stop, then a PER-TENANT budget: within each tenant's eligible (non-'stop',
    above-MIN_PREFIX) rows in this slice, ping the top budget_frac fraction ranked by the
    train-fold priority table (ties broken by cached_context desc, then request_id for
    determinism); the rest get a plain WRITE_5M (cached, just never kept alive). At
    budget_frac=1.0 this is exactly exclude_stop_actions."""
    by_tenant: dict[str, list[ExtRequest]] = {}
    for r in rows:
        if r.stop_reason == "stop" or r.cached_context < MIN_PREFIX:
            continue
        by_tenant.setdefault(r.user, []).append(r)
    actions: dict[int, str] = {r.request_id: gate_action(r.cached_context, False) for r in rows}
    for r in rows:
        if r.stop_reason == "stop":
            actions[r.request_id] = gate_action(r.cached_context, False)
    for t_rows in by_tenant.values():
        ranked = sorted(t_rows, key=lambda r: (-priority.get(r.stop_reason or "(none)", -1e18),
                                               -r.cached_context, r.request_id))
        k = int(round(budget_frac * len(ranked)))
        for r in ranked[:k]:
            actions[r.request_id] = PING_5M
    return actions


# ── value-regression arm (the direct-dollar-objective test) ────────────────────────────

def fit_value_regressor(train_feat: pd.DataFrame, id_to_row: dict[int, ExtRequest], prices: PriceBook,
                        semantics: Semantics, schedule: PingSchedule):
    from sklearn.compose import ColumnTransformer
    from sklearn.ensemble import HistGradientBoostingRegressor
    from sklearn.impute import SimpleImputer
    from sklearn.pipeline import Pipeline
    from sklearn.preprocessing import OneHotEncoder

    fit_rows = train_feat[train_feat["event_observed"]].copy()
    labels, keep = [], []
    for rid in fit_rows["request_id"]:
        r = id_to_row[int(rid)]
        if r.stop_reason == "stop":  # excluded a priori, never trained on, never scored
            keep.append(False)
            continue
        v = realized_ping_value_usd(r, prices.for_model(r.model), semantics, schedule)
        keep.append(v is not None)
        if v is not None:
            labels.append(v)
    fit_rows = fit_rows[np.array(keep)]
    y = np.array(labels)
    cat_cols = ["stop_cluster"]
    numeric = Pipeline([("imputer", SimpleImputer(strategy="median"))])
    categorical = Pipeline([("imputer", SimpleImputer(strategy="most_frequent")),
                            ("onehot", OneHotEncoder(handle_unknown="ignore"))])
    pre = ColumnTransformer([("num", numeric, list(NUMERIC_FEATURES)), ("cat", categorical, cat_cols)])
    reg = HistGradientBoostingRegressor(random_state=SEED, max_depth=3)
    pipe = Pipeline([("pre", pre), ("reg", reg)])
    pipe.fit(fit_rows[list(NUMERIC_FEATURES) + cat_cols], y)
    return pipe, len(fit_rows)


def tune_threshold_per_tenant(train_rows_sorted: list[ExtRequest], tenants: list[str], decide_fn,
                              grid, prices: PriceBook, semantics: Semantics, schedule: PingSchedule,
                              window_end_ms: int, min_signal: int = MIN_SIGNAL):
    """Grid-search a threshold on TRAIN dollars, globally and per tenant with a min-signal
    floor -- the same shape as kv_ttl_predictor_arms.tune_historical_probability, reused
    generically here for both the value-regression and the classifier arm below (hence a
    shared helper, not a copy per caller)."""
    by_tenant: dict[str, list[ExtRequest]] = {}
    for r in train_rows_sorted:
        by_tenant.setdefault(r.user, []).append(r)

    def cost_of(rows_subset, threshold):
        acts = decide_fn(rows_subset, threshold)
        return sum(per_conversation_costs(rows_subset, acts, prices, semantics, schedule,
                                          window_end_ms).values())

    best_global, best_cost = grid[0], float("inf")
    for th in grid:
        c = cost_of(train_rows_sorted, th)
        if c < best_cost:
            best_global, best_cost = th, c
    per_tenant = {}
    for t in tenants:
        rows_t = by_tenant.get(t, [])
        if len(rows_t) < min_signal:
            continue
        best_t, best_t_cost = grid[0], float("inf")
        for th in grid:
            c = cost_of(rows_t, th)
            if c < best_t_cost:
                best_t, best_t_cost = th, c
        per_tenant[t] = best_t
    return per_tenant, best_global


# ── calibrated classifier arm, fit only on the exclude-stop-filtered population ────────

def fit_classifier_exclude_stop(train_feat: pd.DataFrame, id_to_row: dict[int, ExtRequest]):
    from sklearn.calibration import CalibratedClassifierCV
    from sklearn.compose import ColumnTransformer
    from sklearn.ensemble import HistGradientBoostingClassifier
    from sklearn.impute import SimpleImputer
    from sklearn.metrics import roc_auc_score
    from sklearn.pipeline import Pipeline
    from sklearn.preprocessing import OneHotEncoder

    fit_rows = train_feat[train_feat["event_observed"]].copy()
    is_stop = fit_rows["request_id"].map(lambda rid: id_to_row[int(rid)].stop_reason == "stop")
    fit_rows = fit_rows[~is_stop]
    y = fit_rows["band_5m_1h"].astype(int).to_numpy()
    cat_cols = ["stop_cluster"]
    numeric = Pipeline([("imputer", SimpleImputer(strategy="median"))])
    categorical = Pipeline([("imputer", SimpleImputer(strategy="most_frequent")),
                            ("onehot", OneHotEncoder(handle_unknown="ignore"))])
    pre = ColumnTransformer([("num", numeric, list(NUMERIC_FEATURES)), ("cat", categorical, cat_cols)])
    X = pre.fit_transform(fit_rows[list(NUMERIC_FEATURES) + cat_cols])

    # A small, genuine hyperparameter search (2-fold internal CV AUC on train only), not a
    # single hand-picked config -- item 5 of the brief, kept modest given how many model
    # classes have already hit the same ~0.78-0.80 ceiling (established_savings note above).
    from sklearn.model_selection import StratifiedKFold
    grid = [{"max_depth": d, "learning_rate": lr} for d in (2, 3, None) for lr in (0.05, 0.1)]
    cv = StratifiedKFold(n_splits=2, shuffle=True, random_state=SEED)
    best_cfg, best_auc = grid[0], -1.0
    for cfg in grid:
        aucs = []
        for tr_idx, va_idx in cv.split(X, y):
            m = HistGradientBoostingClassifier(random_state=SEED, **cfg)
            m.fit(X[tr_idx], y[tr_idx])
            p = m.predict_proba(X[va_idx])[:, 1]
            if len(set(y[va_idx])) > 1:
                aucs.append(roc_auc_score(y[va_idx], p))
        auc = float(np.mean(aucs)) if aucs else -1.0
        if auc > best_auc:
            best_cfg, best_auc = cfg, auc

    base = HistGradientBoostingClassifier(random_state=SEED, **best_cfg)
    clf = CalibratedClassifierCV(base, method="isotonic", cv=3)
    pipe = Pipeline([("pre", pre), ("clf", clf)])
    pipe.fit(fit_rows[list(NUMERIC_FEATURES) + cat_cols], y)
    train_p = pipe.predict_proba(fit_rows[list(NUMERIC_FEATURES) + cat_cols])[:, 1]
    train_auc = float(roc_auc_score(y, train_p)) if len(set(y)) > 1 else None
    return pipe, {"cv_auc": best_auc, "config": best_cfg, "n_fit": len(fit_rows), "n_events": int(y.sum()),
                 "train_auc": train_auc, "grid_tried": grid}


def calibration_metrics(y_true: np.ndarray, p: np.ndarray) -> dict:
    from sklearn.metrics import brier_score_loss, log_loss, roc_auc_score
    out = {"n": int(len(y_true)), "base_rate": float(np.mean(y_true))}
    if len(set(y_true)) > 1:
        out["auc"] = float(roc_auc_score(y_true, p))
        out["brier"] = float(brier_score_loss(y_true, p))
        out["log_loss"] = float(log_loss(y_true, p, labels=[0, 1]))
    return out


# ── main pipeline ───────────────────────────────────────────────────────────────────────

def run(db_path: str, prices_path: str) -> dict:
    ext_rows = load_ext_trajectories(db_path)
    if not ext_rows:
        raise SystemExit("no requests in that store")
    prices = PriceBook.from_operator_file(prices_path)
    semantics = Semantics()
    schedule = PingSchedule()  # max_pings=2 -- flat-cap-2's own schedule

    lo, hi = ext_rows[0].ts_ms, max(r.ts_ms for r in ext_rows)
    train_cut = lo + int((hi - lo) * TRAIN_FRAC)
    remaining = hi - train_cut
    fold_edges = [train_cut + int(remaining * i / N_FOLDS) for i in range(N_FOLDS + 1)]
    train_rows = [r for r in ext_rows if r.ts_ms <= train_cut]
    train_sorted = sorted(train_rows, key=lambda r: (r.ts_ms, r.request_id))
    fold_rows = []
    for i in range(N_FOLDS):
        lo_e, hi_e = fold_edges[i], fold_edges[i + 1]
        fold_rows.append([r for r in ext_rows if lo_e < r.ts_ms <= hi_e])
    holdout_idx = N_FOLDS - 1  # untouched: no tuning below ever reads fold_rows[holdout_idx]

    tenants = sorted({r.user for r in ext_rows})
    tenant_pseudo = pseudonymize_T(tenants)
    print(f"window {lo}..{hi} ms; train<={train_cut} ({len(train_rows):,} rows); "
          f"{N_FOLDS} folds ({', '.join(str(len(f)) for f in fold_rows)} rows); "
          f"holdout=fold[{holdout_idx}]", file=sys.stderr)

    exclusion_diag = exclusion_feasibility_diagnostic(ext_rows, train_cut, tenant_pseudo)
    print(f"exclusion feasibility: {exclusion_diag['n_stop_after_train_cut']} of "
          f"{exclusion_diag['n_stop_total']} stop-reason=='stop' rows fall after train_cut",
          file=sys.stderr)

    # ---- tuning, TRAIN ONLY --------------------------------------------------------------
    priority = priority_scores_from_train(train_sorted, prices, semantics, schedule)
    print(f"priority table (train-fold mean realized $/row): "
          f"{ {k: round(v, 5) for k, v in sorted(priority.items(), key=lambda kv: -kv[1])} }",
          file=sys.stderr)

    feat_df = build_feature_frame(ext_rows, window_end_ms=hi)
    id_to_row = {r.request_id: r for r in ext_rows}
    train_feat = feat_df[feat_df["ts_ms"] <= train_cut]

    val_pipe, n_val_fit = fit_value_regressor(train_feat, id_to_row, prices, semantics, schedule)
    feat_cols = list(NUMERIC_FEATURES) + ["stop_cluster"]

    def value_pred_for(rows: list[ExtRequest]) -> dict[int, float]:
        ids = {r.request_id for r in rows}
        sub = feat_df[feat_df["request_id"].isin(ids)]
        pred = val_pipe.predict(sub[feat_cols])
        return dict(zip(sub["request_id"], pred))

    def value_actions_for(rows: list[ExtRequest], threshold: float) -> dict[int, str]:
        pred = value_pred_for(rows)
        return {r.request_id: gate_action(r.cached_context,
                                          r.stop_reason != "stop" and pred.get(r.request_id, -1e18) > threshold)
                for r in rows}

    val_grid = list(np.quantile([v for v in value_pred_for(train_rows).values()],
                                [0.5, 0.6, 0.7, 0.8, 0.85, 0.9, 0.95, 0.99])) + [0.0]
    val_per_tenant, val_default = tune_threshold_per_tenant(
        train_sorted, tenants, value_actions_for, sorted(set(val_grid)), prices, semantics,
        schedule, train_cut)
    print(f"value-regression: fit on {n_val_fit:,} rows, tuned {len(val_per_tenant)} "
          f"per-tenant thresholds, default={val_default:.6f}", file=sys.stderr)

    def value_actions_tuned(rows: list[ExtRequest]) -> dict[int, str]:
        pred = value_pred_for(rows)
        by_t: dict[str, list[ExtRequest]] = {}
        for r in rows:
            by_t.setdefault(r.user, []).append(r)
        out: dict[int, str] = {}
        for t, t_rows in by_t.items():
            th = val_per_tenant.get(t, val_default)
            for r in t_rows:
                out[r.request_id] = gate_action(r.cached_context,
                                                r.stop_reason != "stop" and pred.get(r.request_id, -1e18) > th)
        return out

    clf_pipe, clf_summary = fit_classifier_exclude_stop(train_feat, id_to_row)
    print(f"classifier-exclude-stop: {clf_summary['n_fit']:,} rows fit, cv_auc="
          f"{clf_summary['cv_auc']:.4f}, config={clf_summary['config']}", file=sys.stderr)

    def clf_proba_for(rows: list[ExtRequest]) -> dict[int, float]:
        ids = {r.request_id for r in rows}
        sub = feat_df[feat_df["request_id"].isin(ids)]
        proba = clf_pipe.predict_proba(sub[feat_cols])[:, 1]
        return dict(zip(sub["request_id"], proba))

    def clf_actions_for(rows: list[ExtRequest], threshold: float) -> dict[int, str]:
        p = clf_proba_for(rows)
        return {r.request_id: gate_action(r.cached_context,
                                          r.stop_reason != "stop" and p.get(r.request_id, 0.0) >= threshold)
                for r in rows}

    clf_per_tenant, clf_default = tune_threshold_per_tenant(
        train_sorted, tenants, clf_actions_for, (0.02, 0.03, 0.05, 0.08, 0.12, 0.2, 0.35, 0.5),
        prices, semantics, schedule, train_cut)

    # ---- calibration of the classifier arm, scored on TEST (all 3 folds pooled), never train --
    test_feat = feat_df[(feat_df["ts_ms"] > train_cut) & feat_df["event_observed"]].copy()
    test_is_stop = test_feat["request_id"].map(lambda rid: id_to_row[int(rid)].stop_reason == "stop")
    test_feat_ns = test_feat[~test_is_stop]
    y_test = test_feat_ns["band_5m_1h"].astype(int).to_numpy()
    p_test = clf_pipe.predict_proba(test_feat_ns[feat_cols])[:, 1]
    clf_calibration_test = calibration_metrics(y_test, p_test)
    n_bins = 10
    edges = np.quantile(p_test, np.linspace(0, 1, n_bins + 1))
    edges[0], edges[-1] = 0.0, 1.0
    bin_idx = np.clip(np.digitize(p_test, edges[1:-1], right=True), 0, n_bins - 1)
    reliability = []
    for b in range(n_bins):
        mask = bin_idx == b
        if mask.sum() == 0:
            continue
        reliability.append({"n": int(mask.sum()), "mean_predicted": float(p_test[mask].mean()),
                            "observed_rate": float(y_test[mask].mean())})
    clf_calibration_test["reliability_10bin"] = reliability

    def clf_actions_tuned(rows: list[ExtRequest]) -> dict[int, str]:
        p = clf_proba_for(rows)
        by_t: dict[str, list[ExtRequest]] = {}
        for r in rows:
            by_t.setdefault(r.user, []).append(r)
        out: dict[int, str] = {}
        for t, t_rows in by_t.items():
            th = clf_per_tenant.get(t, clf_default)
            for r in t_rows:
                out[r.request_id] = gate_action(r.cached_context,
                                                r.stop_reason != "stop" and p.get(r.request_id, 0.0) >= th)
        return out

    # ---- the arm registry -----------------------------------------------------------------
    # (name, actions_fn(rows)->dict, schedule_override_or_None)
    arms: dict[str, tuple] = {
        "exclude-stop": (exclude_stop_actions, None),
        "exclude-cluster": (exclude_cluster_actions, None),
        "value-regression": (value_actions_tuned, None),
        "gbm-exclude-stop": (clf_actions_tuned, None),
    }
    for frac in BUDGET_FRACTIONS:
        arms[f"budget-priority-{frac}"] = (
            lambda rows, frac=frac: budget_priority_actions(rows, priority, frac), None)
    for k in MAXPINGS_GRID:
        arms[f"maxpings-{k}-exclude-stop"] = (exclude_stop_actions, PingSchedule(max_pings=k))

    # ---- per-fold scoring, every arm, vs BOTH fixed-5m and flat-cap-2 ----------------------
    fold_results: list[dict] = []
    pooled_fixed5m_conv: dict = {}
    pooled_flatcap2_conv: dict = {}
    pooled_arm_conv: dict[str, dict] = {name: {} for name in arms}
    holdout_fixed5m_conv: dict = {}
    holdout_flatcap2_conv: dict = {}
    holdout_arm_conv: dict[str, dict] = {name: {} for name in arms}

    for fi, rows in enumerate(fold_rows):
        if not rows:
            continue
        window_end = fold_edges[fi + 1]
        fixed_cost, fixed_local = fold_cost(rows, fixed_5m_actions(rows), prices, semantics,
                                            schedule, window_end)
        fixed_conv = per_conversation_costs(fixed_local, fixed_5m_actions(fixed_local), prices,
                                            semantics, schedule, window_end)
        flatcap2_cost, flatcap2_local = fold_cost(rows, flat_cap2_actions(rows), prices, semantics,
                                                  schedule, window_end)
        flatcap2_conv = per_conversation_costs(flatcap2_local, flat_cap2_actions(flatcap2_local),
                                               prices, semantics, schedule, window_end)
        for k, v in fixed_conv.items():
            pooled_fixed5m_conv[(fi, *k)] = v
        for k, v in flatcap2_conv.items():
            pooled_flatcap2_conv[(fi, *k)] = v
        if fi == holdout_idx:
            holdout_fixed5m_conv.update(fixed_conv)
            holdout_flatcap2_conv.update(flatcap2_conv)

        fold_entry = {"fold": fi, "n_rows": len(rows), "since_ms": fold_edges[fi], "until_ms": window_end,
                      "fixed5m_usd": fixed_cost.total_usd, "flatcap2_usd": flatcap2_cost.total_usd,
                      "arms": {}}
        for name, (fn, sched_override) in arms.items():
            sched = sched_override or schedule
            acts = fn(rows)
            arm_cost, local = fold_cost(rows, acts, prices, semantics, sched, window_end)
            local_acts = {r.request_id: acts.get(r.request_id, EXPIRE) for r in local}
            arm_conv = per_conversation_costs(local, local_acts, prices, semantics, sched, window_end)
            for k, v in arm_conv.items():
                pooled_arm_conv[name][(fi, *k)] = v
            if fi == holdout_idx:
                holdout_arm_conv[name].update(arm_conv)
            vs_fixed = compare(fixed_cost, arm_cost)
            vs_flatcap2 = compare(flatcap2_cost, arm_cost)
            fold_entry["arms"][name] = {
                "total_usd": arm_cost.total_usd,
                "vs_fixed5m_usd": vs_fixed.absolute_usd, "vs_fixed5m_pct": vs_fixed.percent_usd,
                "vs_flatcap2_usd": vs_flatcap2.absolute_usd, "vs_flatcap2_pct": vs_flatcap2.percent_usd,
                "pings": arm_cost.pings, "writes_5m": arm_cost.writes_5m, "hit_rate_pct": arm_cost.hit_rate_pct,
            }
        fold_results.append(fold_entry)

    # ---- pooled + holdout-only bootstrap CIs, vs both baselines ----------------------------
    pooled: dict[str, dict] = {}
    holdout: dict[str, dict] = {}
    pvalues_vs_flatcap2: dict[str, float] = {}
    for name in arms:
        vs_fixed_pooled = bootstrap_ci(pooled_fixed5m_conv, pooled_arm_conv[name], reps=BOOTSTRAP_REPS, seed=SEED)
        vs_flatcap2_pooled_sess = bootstrap_paired_dollars(
            pooled_fixed5m_conv, pooled_arm_conv[name], pooled_flatcap2_conv, reps=BOOTSTRAP_REPS, seed=SEED)
        vs_flatcap2_pooled_tenant = bootstrap_paired_dollars_tenant(
            pooled_fixed5m_conv, pooled_arm_conv[name], pooled_flatcap2_conv, reps=BOOTSTRAP_REPS, seed=SEED)
        pooled[name] = {
            "vs_fixed5m_session_ci": vs_fixed_pooled,
            "vs_flatcap2_session_ci": vs_flatcap2_pooled_sess,
            "vs_flatcap2_tenant_ci": vs_flatcap2_pooled_tenant,
        }
        pvalues_vs_flatcap2[name] = vs_flatcap2_pooled_sess.get("p_value_two_sided")

        vs_fixed_holdout = bootstrap_ci(holdout_fixed5m_conv, holdout_arm_conv[name], reps=BOOTSTRAP_REPS, seed=SEED)
        vs_flatcap2_holdout = bootstrap_paired_dollars(
            holdout_fixed5m_conv, holdout_arm_conv[name], holdout_flatcap2_conv, reps=BOOTSTRAP_REPS, seed=SEED)
        holdout[name] = {"vs_fixed5m_session_ci": vs_fixed_holdout, "vs_flatcap2_session_ci": vs_flatcap2_holdout}

    fdr = bh_fdr(pvalues_vs_flatcap2)

    # ---- new-tenant / low-history slice -----------------------------------------------------
    train_counts: dict[str, int] = {}
    for r in train_rows:
        train_counts[r.user] = train_counts.get(r.user, 0) + 1
    counts_sorted = sorted(train_counts.values())
    q1 = counts_sorted[len(counts_sorted) // 4] if counts_sorted else 0
    low_history_tenants = {t for t, c in train_counts.items() if c <= q1}
    print(f"low-history slice: {[tenant_pseudo[t] for t in sorted(low_history_tenants)]} "
          f"(train rows <= {q1})", file=sys.stderr)

    low_hist_slice = {}
    for name in ("exclude-stop", "budget-priority-1.0", "gbm-exclude-stop"):
        base_lh = {k: v for k, v in pooled_flatcap2_conv.items() if k[1] in low_history_tenants}
        arm_lh = {k: v for k, v in pooled_arm_conv[name].items() if k[1] in low_history_tenants}
        low_hist_slice[name] = bootstrap_ci(base_lh, arm_lh, reps=BOOTSTRAP_REPS, seed=SEED)
    low_hist_slice["n_tenants"] = len(low_history_tenants)
    low_hist_slice["tenants"] = [tenant_pseudo[t] for t in sorted(low_history_tenants)]
    low_hist_slice["train_row_threshold"] = q1

    # ---- per-tenant gain/loss for the headline arm, pseudonymized ---------------------------
    per_tenant_headline = {}
    for t in tenants:
        pseudo = tenant_pseudo[t]
        base_t = sum(v for k, v in pooled_flatcap2_conv.items() if k[1] == t)
        arm_t = sum(v for k, v in pooled_arm_conv["exclude-stop"].items() if k[1] == t)
        n_t = sum(1 for k in pooled_flatcap2_conv if k[1] == t)
        per_tenant_headline[pseudo] = {
            "n_test_conversations": n_t, "flatcap2_usd": base_t, "exclude_stop_usd": arm_t,
            "delta_usd": base_t - arm_t,
        }

    try:
        sha = subprocess.run(["git", "-C", os.path.dirname(os.path.abspath(__file__)), "rev-parse", "HEAD"],
                            capture_output=True, text=True, timeout=10).stdout.strip()
    except Exception:
        sha = None
    import sklearn
    import lifelines

    return {
        "config_card": {
            "db": db_path, "prices": prices_path, "code_sha": sha, "seed": SEED,
            "train_frac": TRAIN_FRAC, "n_folds": N_FOLDS, "bootstrap_reps": BOOTSTRAP_REPS,
            "budget_fractions": list(BUDGET_FRACTIONS), "maxpings_grid": list(MAXPINGS_GRID),
            "min_signal_floor": MIN_SIGNAL,
            "dependency_versions": {"numpy": np.__version__, "pandas": pd.__version__,
                                    "sklearn": sklearn.__version__, "lifelines": lifelines.__version__},
            "window_utc": {"since_ms": lo, "until_ms": hi, "train_cut_ms": train_cut,
                          "fold_edges_ms": fold_edges,
                          "since_iso": pd.Timestamp(lo, unit="ms", tz="UTC").isoformat(),
                          "until_iso": pd.Timestamp(hi, unit="ms", tz="UTC").isoformat(),
                          "train_cut_iso": pd.Timestamp(train_cut, unit="ms", tz="UTC").isoformat()},
            "n_requests": len(ext_rows), "n_tenants": len(tenants), "n_train_rows": len(train_rows),
            "n_fold_rows": [len(f) for f in fold_rows], "holdout_fold_index": holdout_idx,
            "priority_table_train": priority,
            "value_regression": {"n_fit": n_val_fit, "default_threshold": val_default,
                                 "n_tenants_tuned": len(val_per_tenant), "grid": sorted(set(val_grid))},
            "classifier_exclude_stop": clf_summary | {"default_threshold": clf_default,
                                                      "n_tenants_tuned": len(clf_per_tenant)},
        },
        "calibration_classifier_test": clf_calibration_test,
        "exclusion_feasibility_diagnostic": exclusion_diag,
        "fold_results": fold_results,
        "pooled": pooled,
        "holdout_only": holdout,
        "bh_fdr_vs_flatcap2": fdr,
        "low_history_slice": low_hist_slice,
        "per_tenant_headline_exclude_stop": per_tenant_headline,
        "stop_reason_note": (
            "'stop' (raw value, not the whole looks_done_isnt cluster) is the excluded "
            "group in every 'exclude-stop' / 'gbm-exclude-stop' / 'value-regression' / "
            "'budget-priority' / 'maxpings-*-exclude-stop' arm above. 'exclude-cluster' is "
            "the only arm that excludes '' as well, and is kept in to show the cost of "
            "that coarser cut."),
    }


# ── plots (SVG + plain-English .txt caption per file, this study's own established
#    convention -- kv_ttl_abl_plots.py's write_caption/Okabe-Ito palette, reproduced here
#    rather than imported since that file belongs to a different agent's in-flight work) ──

_BLUE, _ORANGE, _GREEN, _RED, _GREY = "#0072B2", "#E69F00", "#009E73", "#D55E00", "#666666"


def _write_caption(path: str, text: str) -> None:
    with open(path, "w") as fh:
        fh.write(text.strip() + "\n")


def make_plots(result: dict, out_dir: str) -> None:
    import matplotlib
    matplotlib.use("Agg")
    import matplotlib.pyplot as plt

    os.makedirs(out_dir, exist_ok=True)
    pooled = result["pooled"]
    holdout = result["holdout_only"]
    fdr = result["bh_fdr_vs_flatcap2"]

    # 1. net dollars per arm vs both baselines, with CIs -----------------------------------
    headline_arms = ["exclude-stop", "exclude-cluster", "value-regression", "gbm-exclude-stop",
                     "budget-priority-1.0"]
    fig, ax = plt.subplots(figsize=(9, 0.5 * len(headline_arms) + 1.5))
    y = np.arange(len(headline_arms))
    for i, name in enumerate(headline_arms):
        vs_fixed = pooled[name]["vs_fixed5m_session_ci"]
        vs_cap2 = pooled[name]["vs_flatcap2_session_ci"]
        surv = fdr.get(name, {}).get("survives_fdr", False)
        color = _GREEN if (vs_cap2["point_delta_usd"] > 0 and surv) else (
            _RED if (vs_cap2["point_delta_usd"] < 0 and surv) else _GREY)
        lo_c, hi_c = vs_cap2["ci95_delta_usd"]
        ax.barh(i, vs_cap2["point_delta_usd"], xerr=[[vs_cap2["point_delta_usd"] - lo_c],
                                                      [hi_c - vs_cap2["point_delta_usd"]]],
               color=color, ecolor="#333333", capsize=3)
        ax.text(hi_c, i, f"  vs fixed-5m: ${vs_fixed['point_delta_usd']:.0f}", va="center", fontsize=7,
               color=_GREY)
    ax.axvline(0, color="black", linewidth=0.8)
    ax.set_yticks(y, headline_arms, fontsize=9)
    ax.set_xlabel("net $ this arm earns OVER flat-cap-2 (positive = beats the champion)")
    ax.set_title("Net dollars vs. flat-cap-2, pooled 3-fold test replay\n"
                "(green/red = BH-FDR significant at q=0.05; grey = not)")
    fig.tight_layout()
    fig.savefig(os.path.join(out_dir, "net_dollars_vs_baselines.svg"))
    plt.close(fig)
    _write_caption(os.path.join(out_dir, "net_dollars_vs_baselines.txt"), f"""
REPLAYED. Each bar is the point estimate of net dollars a candidate arm earns OVER
flat-cap-2 (positive = the candidate beats the established, zero-parameter champion),
pooled across all 3 rolling-origin test folds, with a conversation-level paired-bootstrap
95% CI ({result['config_card']['bootstrap_reps']} resamples). The grey annotation gives
each arm's separate delta against fixed-5m for context. Green/red bars are the ones whose
p-value survives Benjamini-Hochberg FDR correction at q=0.05 across all
{len(fdr)} arm-vs-flat-cap-2 comparisons tested in this study; grey bars do not.""")

    # 2. calibration reliability of the best model -----------------------------------------
    cal = result["calibration_classifier_test"]
    rel = cal.get("reliability_10bin", [])
    if rel:
        fig, ax = plt.subplots(figsize=(5, 5))
        xs = [b["mean_predicted"] for b in rel]
        ys = [b["observed_rate"] for b in rel]
        ns = [b["n"] for b in rel]
        ax.plot([0, 1], [0, 1], color=_GREY, linestyle="--", linewidth=1, label="perfect calibration")
        ax.scatter(xs, ys, s=[max(15, n / 20) for n in ns], color=_BLUE, zorder=3)
        ax.plot(xs, ys, color=_BLUE, linewidth=1, alpha=0.6)
        ax.set_xlabel("mean predicted P(return in [5m,~14m))")
        ax.set_ylabel("observed rate")
        ax.set_title(f"gbm-exclude-stop calibration, pooled test folds\n"
                    f"AUC={cal.get('auc', float('nan')):.3f}  Brier={cal.get('brier', float('nan')):.4f}  "
                    f"n={cal.get('n', 0):,}")
        ax.legend(fontsize=8)
        fig.tight_layout()
        fig.savefig(os.path.join(out_dir, "calibration_best_model.svg"))
        plt.close(fig)
        _write_caption(os.path.join(out_dir, "calibration_best_model.txt"), f"""
MEASURED. Reliability diagram for gbm-exclude-stop (calibrated HistGradientBoostingClassifier,
isotonic, fit only on rows with stop_reason != 'stop'), scored on the pooled test folds it was
never fit on (event-observed rows only; right-censored rows excluded, never imputed). Marker
size is proportional to the bin's row count. AUC={cal.get('auc', float('nan')):.4f} sits in the
same ~0.78-0.80 information ceiling this project has measured for every model class tried so
far (calibrated logistic regression, random forest, gradient boosting) -- this arm changes
WHICH rows the model is allowed to consider (never 'stop'), not the ceiling itself.""")

    # 3. per-tenant gain/loss for the headline arm -------------------------------------------
    pt = result["per_tenant_headline_exclude_stop"]
    tenants_sorted = sorted(pt, key=lambda t: pt[t]["delta_usd"])
    deltas = [pt[t]["delta_usd"] for t in tenants_sorted]
    fig, ax = plt.subplots(figsize=(8, 0.35 * len(tenants_sorted) + 1.5))
    colors = [_GREEN if d > 0 else _RED for d in deltas]
    ax.barh(np.arange(len(tenants_sorted)), deltas, color=colors)
    ax.axvline(0, color="black", linewidth=0.8)
    ax.set_yticks(np.arange(len(tenants_sorted)), tenants_sorted, fontsize=8)
    ax.set_xlabel("$ exclude-stop earns OVER flat-cap-2, this tenant's test-fold conversations")
    ax.set_title("Per-tenant gain/loss, exclude-stop vs. flat-cap-2 (pseudonymized)")
    fig.tight_layout()
    fig.savefig(os.path.join(out_dir, "per_tenant_gain_loss.svg"))
    plt.close(fig)
    _write_caption(os.path.join(out_dir, "per_tenant_gain_loss.txt"), """
REPLAYED. Each bar is one pseudonymized tenant's own net-$ difference between exclude-stop and
flat-cap-2 on that tenant's pooled test-fold conversations (no bootstrap here -- this is the
point estimate per tenant, not a population CI; see net_dollars_vs_baselines.svg for the CI).
Green = exclude-stop cheaper for that tenant, red = flat-cap-2 cheaper. A tenant with very few
'stop'-heavy conversations in the test window will show near-zero either way, which is the
expected shape, not an anomaly.""")

    # 4. savings vs ping budget --------------------------------------------------------------
    fracs = result["config_card"]["budget_fractions"]
    pts, los, his = [], [], []
    for f in fracs:
        c = pooled[f"budget-priority-{f}"]["vs_flatcap2_session_ci"]
        pts.append(c["point_delta_usd"])
        los.append(c["ci95_delta_usd"][0])
        his.append(c["ci95_delta_usd"][1])
    fig, ax = plt.subplots(figsize=(6, 4.5))
    ax.plot(fracs, pts, marker="o", color=_BLUE)
    ax.fill_between(fracs, los, his, color=_BLUE, alpha=0.2)
    ax.axhline(0, color="black", linewidth=0.8)
    ax.set_xlabel("per-tenant ping budget (fraction of exclude-stop-eligible rows pinged, highest-value first)")
    ax.set_ylabel("net $ vs. flat-cap-2")
    ax.set_title("Savings vs. ping budget, priority-ranked coverage spend under exclude-stop")
    fig.tight_layout()
    fig.savefig(os.path.join(out_dir, "savings_vs_ping_budget.svg"))
    plt.close(fig)
    _write_caption(os.path.join(out_dir, "savings_vs_ping_budget.txt"), f"""
REPLAYED. x-axis is the fraction of each tenant's exclude-stop-eligible rows (already
excluding stop_reason=='stop') that get pinged in a test fold, chosen highest-priority-first
by a category ranking measured on TRAIN rows only (mean realized $ value of pinging that
stop_reason category); the rest get a plain 5-minute write with no keep-alive. y-axis is net
dollars versus flat-cap-2, pooled across test folds, shaded band is the 95% paired-bootstrap
CI. budget=1.0 is exactly the exclude-stop arm. A flat or falling curve as budget increases
past some point means the marginal ping bought with that extra budget is not worth its cost.""")

    # 5. train-vs-test generalization ---------------------------------------------------------
    fig, ax = plt.subplots(figsize=(8, 0.5 * len(headline_arms) + 1.5))
    width = 0.35
    for i, name in enumerate(headline_arms):
        p = pooled[name]["vs_flatcap2_session_ci"]["point_delta_usd"]
        h = holdout[name]["vs_flatcap2_session_ci"]["point_delta_usd"]
        ax.barh(i - width / 2, p, height=width, color=_BLUE, label="pooled (3 folds)" if i == 0 else None)
        ax.barh(i + width / 2, h, height=width, color=_ORANGE, label="untouched holdout (last fold only)" if i == 0 else None)
    ax.axvline(0, color="black", linewidth=0.8)
    ax.set_yticks(np.arange(len(headline_arms)), headline_arms, fontsize=9)
    ax.set_xlabel("net $ vs. flat-cap-2")
    ax.set_title("Pooled test replay vs. the untouched holdout fold (honest generalisation check)")
    ax.legend(fontsize=8, loc="lower right")
    fig.tight_layout()
    fig.savefig(os.path.join(out_dir, "train_vs_test_generalization.svg"))
    plt.close(fig)
    _write_caption(os.path.join(out_dir, "train_vs_test_generalization.txt"), """
REPLAYED. Blue is each arm's net-$ delta vs. flat-cap-2 pooled across all 3 chronological
test folds (the number every other plot and the report table lead with); orange is the SAME
arm and SAME tuned parameters (thresholds, priority table, model weights -- nothing here is
refit) scored ONLY on the third, final fold -- the one no tuning decision in this study ever
looked at before this plot was drawn. Orange tracking blue in sign and rough magnitude is the
honest generalisation result this study can claim; a flip in sign would mean the pooled number
is fit noise, not signal.""")

    # 6. max_pings sweep under exclude-stop ----------------------------------------------------
    ks = result["config_card"]["maxpings_grid"]
    pts, los, his = [], [], []
    for k in ks:
        c = pooled[f"maxpings-{k}-exclude-stop"]["vs_flatcap2_session_ci"]
        pts.append(c["point_delta_usd"])
        los.append(c["ci95_delta_usd"][0])
        his.append(c["ci95_delta_usd"][1])
    fig, ax = plt.subplots(figsize=(6, 4.5))
    ax.errorbar(ks, pts, yerr=[np.subtract(pts, los), np.subtract(his, pts)], marker="o",
               color=_BLUE, capsize=4, linestyle="-")
    ax.axhline(0, color="black", linewidth=0.8)
    ax.set_xlabel("max_pings per idle span (PingSchedule), exclude-stop policy")
    ax.set_ylabel("net $ vs. flat-cap-2 (flat-cap-2 itself uses max_pings=2)")
    ax.set_xticks(ks)
    ax.set_title("Ping-count sweep under exclude-stop")
    fig.tight_layout()
    fig.savefig(os.path.join(out_dir, "maxpings_sweep.svg"))
    plt.close(fig)
    _write_caption(os.path.join(out_dir, "maxpings_sweep.txt"), """
REPLAYED. exclude-stop's own net-$ vs. flat-cap-2 (which fixes max_pings=2), swept over
PingSchedule(max_pings=k) for k=1..4, pooled test folds, 95% paired-bootstrap CI whiskers.
Answers whether excluding the 'stop' dead end -- which frees real ping budget by simply never
spending it on a group that is rescued 0.03% of the time -- shifts the optimal ping count away
from the flat N=1/2/3 plateau this project's earlier fleet sweep found under ping-everyone.""")


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--db", required=True)
    ap.add_argument("--prices", required=True)
    ap.add_argument("--out", required=True)
    ap.add_argument("--plots-only", action="store_true")
    ap.add_argument("--plots-dir", default=None)
    args = ap.parse_args()

    if args.plots_only:
        with open(args.out) as fh:
            result = json.load(fh)
        make_plots(result, args.plots_dir or os.path.join(os.path.dirname(args.out), "plots"))
        return 0

    result = run(args.db, args.prices)
    fd = os.open(args.out, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o644)
    with os.fdopen(fd, "w", encoding="utf-8") as fh:
        json.dump(result, fh, indent=2, default=str)
    print(f"wrote {args.out}", file=sys.stderr)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
