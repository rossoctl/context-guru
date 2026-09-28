#!/usr/bin/env python3
"""New predictor arms and gap statistics on top of kv_ttl_ka_arms.py.

Everything here is ADDITIVE: no function in kv_ttl_cost_model.py, kv_ttl_predictor_arms.py
or kv_ttl_ka_arms.py is modified, only imported and reused, per this study's own established
convention ("reused as a library, not reimplemented"). Same read-only store, same
train_frac/folds/seed convention as kv_ttl_ka_arms.py, so every number here is directly
comparable to that script's own table.

New arms in the $ comparison (fold_results / pooled bootstrap), all built on the SAME
population and features as hybrid2 (NUMERIC_FEATURES + stop_cluster, no tenant one-hot, so
every one of them can score a tenant it never trained on) and the SAME per-tenant-threshold-
with-fallback machinery hybrid2 uses (extracted here as `tune_threshold` so it is not
reimplemented four times):

  gbm-policy            HistGradientBoostingClassifier, calibrated, scored as a POLICY
                         (thresholded on $, not just read for feature importance --
                         kv_ttl_predictor_arms.gbm_feature_importance already does the latter
                         and is not duplicated here).
  ensemble-hg2-gbm       mean of hybrid2's and gbm-policy's calibrated probabilities, same
                         tenant-threshold-with-fallback tuning on top. The "conservative
                         sparse-tenant fallback" the brief asks for IS that fallback --
                         below hybrid2's own 30-event floor an ensemble member reduces to the
                         GLOBAL threshold, never an unstable single-tenant fit.
  cost-aware-simple      two independently-fitted calibrated probabilities, P(return<5m) and
                         P(return<1h), wired into kv_ttl_cost_model.policy_expected_cost --
                         the ALREADY-CORRECT dollar rule the doc's own baseline table scored
                         at only +$6.42/+0.06% using the survival predictor's raw probability.
                         This arm asks whether a BETTER probability estimator moves that
                         number, holding the (already correct) decision rule fixed.
  cost-aware-hazard      the same rule, fed by a discrete-time hazard model with sweep INDEX
                         as a time-varying feature (person-period expansion over horizons
                         {5m,15m,30m,1h}, one row per (request, horizon it reached) --
                         genuinely new to this line of work, distinct from hybrid2's
                         single-shot classifier and from kv_ttl_survival_predictor's
                         unconditional band model) instead of two independent snapshots.

Learning curves and the per-ping cost percentile curve -- flagged in the brief as never
computed -- are both here (`--learning-curve-out`, `--ping-cost-out`).
"""
from __future__ import annotations
import argparse, json, os, sys, warnings
warnings.filterwarnings("ignore")
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import numpy as np
import pandas as pd

from kv_ttl_cost_model import (  # noqa: E402
    PriceBook, Semantics, PingSchedule, PolicyContext, policy_expected_cost,
    EXPIRE, WRITE_5M, PING_5M, derive, DEFAULT_DB, DEFAULT_PRICES,
)
from kv_ttl_predictor_arms import (  # noqa: E402
    load_ext_trajectories, pseudonymize_map, gate_action, stop_cluster,
    build_feature_frame, per_conversation_costs, bootstrap_ci, fold_cost, fixed_5m_actions,
    NUMERIC_FEATURES, MIN_PREFIX, HORIZON_5M_S, HORIZON_1H_S, BREAK_EVEN_P1H,
)
from kv_ttl_ka_arms import (  # noqa: E402
    bootstrap_ci_tenant, fit_hybrid2, HYBRID2_NUMERIC, HYBRID2_CATEGORICAL,
)

DEFAULT_HAZARD_HORIZONS_S = (300.0, 900.0, 1800.0, 3600.0)  # 5m, 15m, 30m, 1h


# ── shared tenant-threshold-with-fallback tuner (hybrid2's own mechanism, generalized) ──

def tune_threshold(proba_col: str, train_feat: pd.DataFrame, train_rows, tenants,
                   prices, semantics, schedule, window_end_ms, *,
                   grid=(0.01, 0.02, 0.03, 0.05, 0.08, 0.12, 0.2, 0.35, 0.5), min_events=30):
    """Grid-search a $ threshold globally, then per tenant, falling back to the global
    threshold below `min_events` actually-done training events -- exactly hybrid2's own
    tuning (kv_ttl_ka_arms.fit_hybrid2), pulled out so gbm-policy and the ensemble use the
    SAME conservative-fallback mechanism instead of a fourth reimplementation of it."""
    rows_by_tenant: dict[str, list] = {}
    for r in train_rows:
        rows_by_tenant.setdefault(r.user, []).append(r)
    by_tenant_feat = {t: train_feat[train_feat["user_id"] == t] for t in tenants}

    def cost_at(threshold, feat_slice, rows_slice):
        ids_ping = set(feat_slice.loc[feat_slice[proba_col] >= threshold, "request_id"])
        acts = {r.request_id: gate_action(r.cached_context, r.request_id in ids_ping)
                for r in rows_slice}
        return sum(per_conversation_costs(rows_slice, acts, prices, semantics, schedule,
                                          window_end_ms).values())

    best_global, best_cost = grid[len(grid) // 2], float("inf")
    for thr in grid:
        c = cost_at(thr, train_feat, train_rows)
        if c < best_cost:
            best_global, best_cost = thr, c

    n_actually_done: dict[str, int] = {}
    for r in train_rows:
        if stop_cluster(r.stop_reason) == "actually_done":
            n_actually_done[r.user] = n_actually_done.get(r.user, 0) + 1

    per_tenant_thr: dict[str, float] = {}
    skipped = []
    for t in tenants:
        if n_actually_done.get(t, 0) < min_events:
            skipped.append(t)
            continue
        t_rows = rows_by_tenant.get(t, [])
        t_feat = by_tenant_feat.get(t, train_feat.iloc[0:0])
        best_t, best_t_cost = best_global, float("inf")
        for thr in grid:
            c = cost_at(thr, t_feat, t_rows)
            if c < best_t_cost:
                best_t, best_t_cost = thr, c
        per_tenant_thr[t] = best_t
    return best_global, per_tenant_thr, skipped


def actions_from_proba(feat_df, proba_by_id, global_thr, per_tenant_thr, rows):
    return {r.request_id: gate_action(
        r.cached_context, proba_by_id.get(r.request_id, 0.0) >= per_tenant_thr.get(r.user, global_thr))
        for r in rows}


# ── gbm-policy: HistGBM scored AS A POLICY, not read for importance only ──────────────────

def fit_gbm_policy(train_feat: pd.DataFrame):
    from sklearn.compose import ColumnTransformer
    from sklearn.impute import SimpleImputer
    from sklearn.pipeline import Pipeline
    from sklearn.preprocessing import OneHotEncoder
    from sklearn.ensemble import HistGradientBoostingClassifier
    from sklearn.calibration import CalibratedClassifierCV

    fit_rows = train_feat[train_feat["event_observed"]].copy()
    y = fit_rows["band_5m_1h"].astype(int).to_numpy()
    numeric = Pipeline([("imputer", SimpleImputer(strategy="median"))])
    categorical = Pipeline([("imputer", SimpleImputer(strategy="most_frequent")),
                            ("onehot", OneHotEncoder(handle_unknown="ignore"))])
    pre = ColumnTransformer([("num", numeric, list(HYBRID2_NUMERIC)),
                             ("cat", categorical, list(HYBRID2_CATEGORICAL))])
    base = HistGradientBoostingClassifier(max_iter=150, learning_rate=0.08, random_state=0)
    # CalibratedClassifierCV wants a full estimator (pre+clf), not the bare GBM, so the
    # isotonic calibration sees the SAME preprocessed space fit() will score at inference.
    pipe = Pipeline([("pre", pre), ("clf", base)])
    cal = CalibratedClassifierCV(pipe, method="isotonic", cv=3)
    X = fit_rows[list(HYBRID2_NUMERIC) + list(HYBRID2_CATEGORICAL)]
    cal.fit(X, y)

    def proba_for(df: pd.DataFrame) -> np.ndarray:
        return cal.predict_proba(df[list(HYBRID2_NUMERIC) + list(HYBRID2_CATEGORICAL)])[:, 1]

    return cal, proba_for


# ── cost-aware: two calibrated snapshot probabilities feeding the EXISTING $ rule ─────────

def fit_binary_target(train_feat: pd.DataFrame, target_col: str):
    """One calibrated logistic model for an arbitrary binary target on the hybrid2 feature
    set -- reused twice, for P(return<5m) and P(return<1h), rather than writing fit_hybrid2
    a third time with a different label column."""
    from sklearn.compose import ColumnTransformer
    from sklearn.impute import SimpleImputer
    from sklearn.linear_model import LogisticRegression
    from sklearn.pipeline import Pipeline
    from sklearn.preprocessing import OneHotEncoder, StandardScaler
    from sklearn.calibration import CalibratedClassifierCV

    fit_rows = train_feat[train_feat["event_observed"]].copy()
    y = fit_rows[target_col].astype(int).to_numpy()
    if y.sum() == 0 or y.sum() == len(y):
        return None  # degenerate target on this slice; caller falls back
    numeric = Pipeline([("imputer", SimpleImputer(strategy="median")), ("scaler", StandardScaler())])
    categorical = Pipeline([("imputer", SimpleImputer(strategy="most_frequent")),
                            ("onehot", OneHotEncoder(handle_unknown="ignore"))])
    pre = ColumnTransformer([("num", numeric, list(HYBRID2_NUMERIC)),
                             ("cat", categorical, list(HYBRID2_CATEGORICAL))])
    pipe = Pipeline([("pre", pre),
                     ("clf", CalibratedClassifierCV(LogisticRegression(max_iter=2000, random_state=0),
                                                    method="isotonic", cv=3))])
    pipe.fit(fit_rows[list(HYBRID2_NUMERIC) + list(HYBRID2_CATEGORICAL)], y)

    def proba_for(df: pd.DataFrame) -> np.ndarray:
        return pipe.predict_proba(df[list(HYBRID2_NUMERIC) + list(HYBRID2_CATEGORICAL)])[:, 1]

    return proba_for


def cost_aware_actions(rows, feat_df, p5_by_id, p1_by_id, prices, semantics, schedule):
    """gate_action's family degenerates to WRITE_5M/EXPIRE only; policy_expected_cost can
    additionally pick WRITE_1H/PING_1H, which is the whole point of feeding it a real
    probability instead of a fixed threshold -- so this bypasses gate_action entirely and
    calls the cost model's own policy function, per its own docstring's contract."""
    ctx = PolicyContext(prices=prices, semantics=semantics, schedule=schedule,
                        p5m=p5_by_id, p1h=p1_by_id, min_prefix=MIN_PREFIX)
    acts = policy_expected_cost(rows, ctx)
    return {r.request_id: a for r, a in zip(rows, acts)}, ctx.notes


# ── discrete-time hazard, horizon as a TIME-VARYING feature ──────────────────────────────

def build_hazard_frame(feat_df: pd.DataFrame, horizons_s=DEFAULT_HAZARD_HORIZONS_S) -> pd.DataFrame:
    """Person-period expansion: one row per (request, horizon it actually reached), same
    shape as kv_ttl_keepalive_policy.person_periods but over the REQUEST population (every
    request opens a decision point) rather than post-min-prefix idle spans, because this
    feeds the predictor-arms $ comparison, not the keep-alive budget policy.

    y_hazard = 1 iff the gap closes inside this horizon but not the previous one (a
    discrete-time hazard target); horizon itself becomes a feature, so one model spans all
    four horizons instead of fitting hybrid2's frame four separate times.
    """
    base = feat_df[feat_df["event_observed"]]
    frames = []
    prev = 0.0
    for h in horizons_s:
        f = base.copy()
        f["horizon_s"] = h
        f["log_horizon_s"] = np.log1p(h)
        gap = f["time_to_next_request_seconds"]
        f["y_hazard"] = ((gap > prev) & (gap <= h)).astype(int)
        # A row already resolved at an earlier horizon (gap <= prev) contributes no
        # information about THIS horizon's hazard -- it never reaches the decision point.
        f = f[gap > prev]
        frames.append(f)
        prev = h
    return pd.concat(frames, ignore_index=True)


def fit_hazard(train_feat: pd.DataFrame, horizons_s=DEFAULT_HAZARD_HORIZONS_S):
    from sklearn.compose import ColumnTransformer
    from sklearn.impute import SimpleImputer
    from sklearn.linear_model import LogisticRegression
    from sklearn.pipeline import Pipeline
    from sklearn.preprocessing import OneHotEncoder, StandardScaler
    from sklearn.calibration import CalibratedClassifierCV

    pp = build_hazard_frame(train_feat, horizons_s)
    numeric = list(HYBRID2_NUMERIC) + ["log_horizon_s"]
    y = pp["y_hazard"].to_numpy()
    num_pipe = Pipeline([("imputer", SimpleImputer(strategy="median")), ("scaler", StandardScaler())])
    cat_pipe = Pipeline([("imputer", SimpleImputer(strategy="most_frequent")),
                         ("onehot", OneHotEncoder(handle_unknown="ignore"))])
    pre = ColumnTransformer([("num", num_pipe, numeric), ("cat", cat_pipe, list(HYBRID2_CATEGORICAL))])
    pipe = Pipeline([("pre", pre),
                     ("clf", CalibratedClassifierCV(LogisticRegression(max_iter=2000, random_state=0),
                                                    method="isotonic", cv=3))])
    pipe.fit(pp[numeric + list(HYBRID2_CATEGORICAL)], y)

    def hazard_at(df: pd.DataFrame, horizon_s: float) -> np.ndarray:
        d = df.copy()
        d["horizon_s"] = horizon_s
        d["log_horizon_s"] = np.log1p(horizon_s)
        return pipe.predict_proba(d[numeric + list(HYBRID2_CATEGORICAL)])[:, 1]

    def p5_p1_for(df: pd.DataFrame):
        # P(closes by 5m) is the hazard AT the 5m horizon directly (first period, so
        # cumulative == period hazard); P(closes by 1h) is 1 - prod(1 - hazard_j) across the
        # four periods, composed forwards exactly as ReuseModel.cdf_for composes its survival
        # model -- the same identity, reused rather than re-derived.
        surv = np.ones(len(df))
        p5 = None
        for i, h in enumerate(horizons_s):
            hz = hazard_at(df, h)
            if i == 0:
                p5 = hz.copy()
            surv = surv * (1.0 - hz)
        p1 = 1.0 - surv
        return p5, p1

    return pipe, p5_p1_for, len(pp), int(y.sum())


def hazard_auc(train_feat_test: pd.DataFrame, p5_p1_for) -> dict:
    from sklearn.metrics import roc_auc_score
    sub = train_feat_test[train_feat_test["event_observed"]]
    if sub.empty:
        return {}
    p5, p1 = p5_p1_for(sub)
    y5 = (sub["time_to_next_request_seconds"] <= HORIZON_5M_S).astype(int).to_numpy()
    y1 = (sub["time_to_next_request_seconds"] <= HORIZON_1H_S).astype(int).to_numpy()
    out = {}
    if 0 < y5.mean() < 1:
        out["auc_p5"] = float(roc_auc_score(y5, p5))
    if 0 < y1.mean() < 1:
        out["auc_p1"] = float(roc_auc_score(y1, p1))
    return out


# ── per-ping cost, full distribution (never computed before this script) ─────────────────

def per_ping_cost_distribution(ext_rows, prices: PriceBook, semantics: Semantics) -> dict:
    """The dollar cost of ONE keep-alive ping, per row that clears the caching gate
    (cached_context >= MIN_PREFIX), at that row's own model's rates -- i.e. exactly
    ModelPricing.keep_alive_cost applied to every real (model, cached_context) pair in the
    corpus, not a synthetic draw. p50/p99 of this distribution are the two figures already
    quoted (kv-ttl-keepalive-generalization-2026-09.md); p90, max and the full curve are the
    stated gap this fills."""
    costs = []
    for r in ext_rows:
        if r.cached_context < MIN_PREFIX:
            continue
        price = prices.for_model(r.model)
        if not price.known:
            continue
        costs.append(price.keep_alive_cost(r.cached_context, semantics))
    arr = np.array(costs, dtype=float)
    if arr.size == 0:
        return {"n": 0}
    pcts = [1, 5, 10, 25, 50, 75, 90, 95, 99, 99.9]
    return {
        "n": int(arr.size),
        "mean": float(arr.mean()),
        "percentiles": {str(p): float(np.percentile(arr, p)) for p in pcts},
        "max": float(arr.max()),
        "full_sorted_sample": sorted(np.random.default_rng(0).choice(
            arr, size=min(20000, arr.size), replace=False).tolist()) if arr.size > 20000
            else sorted(arr.tolist()),
    }


# ── the funnel: all idle spans -> actionable decision points ─────────────────────────────

def funnel(ext_rows, feat_df: pd.DataFrame) -> dict:
    total = len(ext_rows)
    reaches_gate = sum(1 for r in ext_rows if r.cached_context >= MIN_PREFIX)
    actually_done = sum(1 for r in ext_rows
                        if r.cached_context >= MIN_PREFIX and stop_cluster(r.stop_reason) == "actually_done")
    decidable = int(((feat_df["cached_context"] >= MIN_PREFIX) & feat_df["event_observed"]).sum())
    band_5m_1h = int(((feat_df["cached_context"] >= MIN_PREFIX) & feat_df["band_5m_1h"]).sum())
    return {
        "all_requests_idle_spans_opened": total,
        "clears_min_prefix_gate": reaches_gate,
        "actually_done_cluster": actually_done,
        "has_a_known_next_request_decidable": decidable,
        "in_the_5m_to_1h_band_the_arms_target": band_5m_1h,
    }


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--db", default=DEFAULT_DB)
    ap.add_argument("--prices", default=DEFAULT_PRICES)
    ap.add_argument("--train-frac", type=float, default=0.6)
    ap.add_argument("--folds", type=int, default=3)
    ap.add_argument("--bootstrap", type=int, default=400)
    ap.add_argument("--seed", type=int, default=0)
    ap.add_argument("--low-history-max-events", type=int, default=50,
                    help="a tenant with <= this many actually-done TRAIN events is the "
                         "'new-tenant / low-history' slice")
    ap.add_argument("--out", default="arms_ext_result.json")
    ap.add_argument("--learning-curve-out", default=None)
    ap.add_argument("--ping-cost-out", default=None)
    ap.add_argument("--funnel-out", default=None)
    ap.add_argument("--gap-dist-out", default=None,
                    help="raw (subsampled) gap seconds + censoring flag, for the survival "
                         "curve plot")
    args = ap.parse_args()

    ext_rows = load_ext_trajectories(args.db)
    if not ext_rows:
        raise SystemExit("no requests in that store")
    prices = PriceBook.from_operator_file(args.prices)
    semantics = Semantics()
    schedule = PingSchedule()

    lo, hi = ext_rows[0].ts_ms, max(r.ts_ms for r in ext_rows)
    train_cut = lo + int((hi - lo) * args.train_frac)
    remaining = hi - train_cut
    n_folds = args.folds
    fold_edges = [train_cut + int(remaining * i / n_folds) for i in range(n_folds + 1)]
    train_rows = [r for r in ext_rows if r.ts_ms <= train_cut]
    fold_rows = [[r for r in ext_rows if fold_edges[i] < r.ts_ms <= fold_edges[i + 1]]
                 for i in range(n_folds)]
    all_test_rows = [r for f in fold_rows for r in f]
    tenants = sorted({r.user for r in ext_rows})
    tenant_pseudo = pseudonymize_map(tenants)
    holdout_fold = n_folds - 1  # the most-future fold: never touched by any threshold/model
                                # selection below, only scored -- the "genuinely untouched
                                # final holdout" the brief asks for.
    print(f"window {lo}..{hi} ms; train<= {train_cut} ({len(train_rows):,}); "
          f"folds: {[len(f) for f in fold_rows]}; holdout=fold {holdout_fold}", file=sys.stderr)

    feat_df = build_feature_frame(ext_rows, window_end_ms=hi)
    train_feat = feat_df[feat_df["ts_ms"] <= train_cut].copy()

    # ---- fit every new model, TRAIN-FOLD ONLY --------------------------------------------
    pipe_hy2, hy2_g, hy2_pt, hy2_skip, hy2_proba = fit_hybrid2(
        train_feat, train_rows, tenants, prices, semantics, schedule, train_cut)
    cal_gbm, gbm_proba = fit_gbm_policy(train_feat)
    train_feat_g = train_feat.assign(p_gbm=gbm_proba(train_feat))
    gbm_g, gbm_pt, gbm_skip = tune_threshold("p_gbm", train_feat_g, train_rows, tenants,
                                             prices, semantics, schedule, train_cut)

    train_feat_e = train_feat.assign(
        p_ens=0.5 * hy2_proba(train_feat) + 0.5 * gbm_proba(train_feat))
    ens_g, ens_pt, ens_skip = tune_threshold("p_ens", train_feat_e, train_rows, tenants,
                                             prices, semantics, schedule, train_cut)

    train_feat["target_p5"] = (train_feat["time_to_next_request_seconds"] <= HORIZON_5M_S).astype(int)
    train_feat["target_p1"] = (train_feat["time_to_next_request_seconds"] <= HORIZON_1H_S).astype(int)
    p5_simple = fit_binary_target(train_feat, "target_p5")
    p1_simple = fit_binary_target(train_feat, "target_p1")

    hazard_pipe, hazard_p5p1_for, hz_n, hz_events = fit_hazard(train_feat)
    print(f"hazard: fit on {hz_n:,} person-periods ({hz_events:,} events)", file=sys.stderr)

    def hybrid2_actions_for(rows):
        ids = {r.request_id for r in rows}
        sub = feat_df[feat_df["request_id"].isin(ids)]
        p = dict(zip(sub["request_id"], hy2_proba(sub)))
        return actions_from_proba(feat_df, p, hy2_g, hy2_pt, rows)

    def gbm_actions_for(rows):
        ids = {r.request_id for r in rows}
        sub = feat_df[feat_df["request_id"].isin(ids)]
        p = dict(zip(sub["request_id"], gbm_proba(sub)))
        return actions_from_proba(feat_df, p, gbm_g, gbm_pt, rows)

    def ensemble_actions_for(rows):
        ids = {r.request_id for r in rows}
        sub = feat_df[feat_df["request_id"].isin(ids)]
        p = dict(zip(sub["request_id"], 0.5 * hy2_proba(sub) + 0.5 * gbm_proba(sub)))
        return actions_from_proba(feat_df, p, ens_g, ens_pt, rows)

    def cost_aware_simple_actions_for(rows):
        ids = {r.request_id for r in rows}
        sub = feat_df[feat_df["request_id"].isin(ids)]
        p5 = p5_simple(sub) if p5_simple else np.zeros(len(sub))
        p1 = p1_simple(sub) if p1_simple else np.zeros(len(sub))
        p5_by_id = dict(zip(sub["request_id"], p5))
        p1_by_id = dict(zip(sub["request_id"], p1))
        acts, _ = cost_aware_actions(rows, feat_df, p5_by_id, p1_by_id, prices, semantics, schedule)
        return acts

    def cost_aware_hazard_actions_for(rows):
        ids = {r.request_id for r in rows}
        sub = feat_df[feat_df["request_id"].isin(ids)]
        p5, p1 = hazard_p5p1_for(sub)
        p5_by_id = dict(zip(sub["request_id"], p5))
        p1_by_id = dict(zip(sub["request_id"], p1))
        acts, _ = cost_aware_actions(rows, feat_df, p5_by_id, p1_by_id, prices, semantics, schedule)
        return acts

    arms = {
        "gbm-policy": gbm_actions_for,
        "ensemble-hg2-gbm": ensemble_actions_for,
        "cost-aware-simple": cost_aware_simple_actions_for,
        "cost-aware-hazard": cost_aware_hazard_actions_for,
    }

    pooled_conv: dict[str, dict] = {name: {} for name in arms}
    pooled_baseline_conv: dict = {}
    fold_results = []
    for fi, rows in enumerate(fold_rows):
        if not rows:
            continue
        window_end = fold_edges[fi + 1]
        base_cost, base_local = fold_cost(rows, fixed_5m_actions(rows), prices, semantics,
                                          schedule, window_end)
        base_conv = per_conversation_costs(base_local, fixed_5m_actions(base_local), prices,
                                           semantics, schedule, window_end)
        for k, v in base_conv.items():
            pooled_baseline_conv[(fi, *k)] = v
        entry = {"fold": fi, "n_rows": len(rows), "is_untouched_holdout": fi == holdout_fold,
                 "baseline_usd": base_cost.total_usd, "arms": {}}
        for name, fn in arms.items():
            acts = fn(rows)
            arm_cost, local = fold_cost(rows, acts, prices, semantics, schedule, window_end)
            local_acts = {r.request_id: acts.get(r.request_id, EXPIRE) for r in local}
            arm_conv = per_conversation_costs(local, local_acts, prices, semantics, schedule,
                                              window_end)
            for k, v in arm_conv.items():
                pooled_conv[name][(fi, *k)] = v
            entry["arms"][name] = {
                "total_usd": arm_cost.total_usd, "pings": arm_cost.pings,
                "pings_on_open_spans": arm_cost.pings_on_open_spans,
                "avoided_recomputations": arm_cost.avoided_recomputations,
                "writes_5m": arm_cost.writes_5m, "writes_1h": arm_cost.writes_1h,
                "hit_rate_pct": arm_cost.hit_rate_pct,
            }
        fold_results.append(entry)

    pooled_session = {name: bootstrap_ci(pooled_baseline_conv, pooled_conv[name],
                                         reps=args.bootstrap, seed=args.seed) for name in arms}
    pooled_tenant = {name: bootstrap_ci_tenant(pooled_baseline_conv, pooled_conv[name],
                                               reps=args.bootstrap, seed=args.seed) for name in arms}

    # holdout-only bootstrap: the untouched final fold alone, no fold pooling
    holdout_baseline = {k: v for k, v in pooled_baseline_conv.items() if k[0] == holdout_fold}
    holdout_arm = {name: {k: v for k, v in pooled_conv[name].items() if k[0] == holdout_fold}
                  for name in arms}
    holdout_only = {name: bootstrap_ci(holdout_baseline, holdout_arm[name],
                                       reps=args.bootstrap, seed=args.seed) for name in arms}

    # per-tenant gain/loss for every new arm, and pings-per-rescue / wasted-pings from the
    # Cost accounting Cost already tracks (avoided_recomputations, pings_on_open_spans) --
    # not re-derived, read off the fold_results just written.
    per_tenant_report = {}
    for t in tenants:
        pseudo = tenant_pseudo[t]
        base_t = sum(v for k, v in pooled_baseline_conv.items() if k[1] == t)
        entry = {"baseline_usd": base_t, "arms": {}}
        for name in arms:
            arm_t = sum(v for k, v in pooled_conv[name].items() if k[1] == t)
            pct = 100 * (base_t - arm_t) / base_t if base_t else None
            entry["arms"][name] = {"usd": arm_t, "percent_savings": pct,
                                   "harmed": bool(pct is not None and pct < 0)}
        per_tenant_report[pseudo] = entry

    pings_per_rescue = {}
    wasted_pings = {}
    for name in arms:
        tot_pings = sum(e["arms"][name]["pings"] for e in fold_results)
        tot_rescues = sum(e["arms"][name]["avoided_recomputations"] for e in fold_results)
        tot_open = sum(e["arms"][name]["pings_on_open_spans"] for e in fold_results)
        pings_per_rescue[name] = (tot_pings / tot_rescues) if tot_rescues else None
        wasted_pings[name] = {
            "pings_on_open_spans_lower_bound": tot_open, "total_pings": tot_pings,
            "note": "avoided_recomputations counts RESCUE EVENTS (writes and pings pooled), "
                    "not per-ping attribution -- pings_per_rescue is a coarse ratio, not an "
                    "exact 1:1 mapping, since max_pings=2 lets one rescue consume 1 or 2 "
                    "pings. pings_on_open_spans is a genuine lower bound on waste: pings "
                    "fired on a span still open (outcome unknown) at window end.",
        }

    # ---- calibration for every learned arm's raw probability, pooled TEST folds ----------
    from sklearn.metrics import brier_score_loss, log_loss, roc_auc_score
    from sklearn.calibration import calibration_curve
    test_ids = {r.request_id for r in all_test_rows}
    test_feat = feat_df[feat_df["request_id"].isin(test_ids) & feat_df["event_observed"]]
    y_band = test_feat["band_5m_1h"].astype(int).to_numpy()

    def calib_block(proba: np.ndarray) -> dict:
        frac_pos, mean_pred = calibration_curve(y_band, proba, n_bins=10, strategy="quantile")
        return {
            "n": int(len(y_band)), "base_rate": float(y_band.mean()),
            "brier": float(brier_score_loss(y_band, proba)),
            "log_loss": float(log_loss(y_band, np.clip(proba, 1e-6, 1 - 1e-6))),
            "auc": float(roc_auc_score(y_band, proba)) if 0 < y_band.mean() < 1 else None,
            "ece_10bin_quantile": float(np.mean(np.abs(frac_pos - mean_pred))),
            "reliability_frac_pos": frac_pos.tolist(), "reliability_mean_pred": mean_pred.tolist(),
        }

    calibration = {
        "hybrid2": calib_block(hy2_proba(test_feat)),
        "gbm-policy": calib_block(gbm_proba(test_feat)),
        "ensemble-hg2-gbm": calib_block(0.5 * hy2_proba(test_feat) + 0.5 * gbm_proba(test_feat)),
    }
    hazard_auc_test = hazard_auc(test_feat, hazard_p5p1_for)

    # ---- new-tenant / low-history slice: tenants with <= N actually-done TRAIN events ----
    n_ad_train: dict[str, int] = {}
    for r in train_rows:
        if stop_cluster(r.stop_reason) == "actually_done":
            n_ad_train[r.user] = n_ad_train.get(r.user, 0) + 1
    low_history_tenants = [t for t in tenants if n_ad_train.get(t, 0) <= args.low_history_max_events]
    lh_base = sum(v for k, v in pooled_baseline_conv.items() if k[1] in low_history_tenants)
    low_history_slice = {"tenants": [tenant_pseudo[t] for t in low_history_tenants],
                         "n_tenants": len(low_history_tenants), "baseline_usd": lh_base, "arms": {}}
    for name in arms:
        arm_lh = sum(v for k, v in pooled_conv[name].items() if k[1] in low_history_tenants)
        pct = 100 * (lh_base - arm_lh) / lh_base if lh_base else None
        low_history_slice["arms"][name] = {"usd": arm_lh, "percent_savings": pct}

    result = {
        "config": {"db": args.db, "prices": args.prices, "train_frac": args.train_frac,
                   "folds": args.folds, "bootstrap": args.bootstrap, "seed": args.seed,
                   "holdout_fold_index": holdout_fold,
                   "low_history_max_train_events": args.low_history_max_events},
        "window": {"since_ms": lo, "until_ms": hi, "train_cut_ms": train_cut,
                  "fold_edges_ms": fold_edges},
        "n_requests": len(ext_rows), "n_tenants": len(tenants),
        "fold_results": fold_results,
        "pooled_session_bootstrap": pooled_session,
        "pooled_tenant_bootstrap": pooled_tenant,
        "holdout_fold_only_bootstrap": holdout_only,
        "per_tenant": per_tenant_report,
        "low_history_slice": low_history_slice,
        "pings_per_rescue": pings_per_rescue,
        "wasted_pings": wasted_pings,
        "calibration_pooled_test": calibration,
        "hazard_auc_pooled_test": hazard_auc_test,
        "thresholds": {
            "gbm_policy": {"global": gbm_g, "n_tenants_tuned": len(gbm_pt),
                          "skipped": [tenant_pseudo[t] for t in gbm_skip]},
            "ensemble": {"global": ens_g, "n_tenants_tuned": len(ens_pt),
                        "skipped": [tenant_pseudo[t] for t in ens_skip]},
        },
    }
    with open(args.out, "w") as fh:
        json.dump(result, fh, indent=2, default=str)
    print(f"wrote {args.out}", file=sys.stderr)

    if args.ping_cost_out:
        with open(args.ping_cost_out, "w") as fh:
            json.dump(per_ping_cost_distribution(ext_rows, prices, semantics), fh, indent=2)
        print(f"wrote {args.ping_cost_out}", file=sys.stderr)

    if args.funnel_out:
        with open(args.funnel_out, "w") as fh:
            json.dump(funnel(ext_rows, feat_df), fh, indent=2)
        print(f"wrote {args.funnel_out}", file=sys.stderr)

    if args.gap_dist_out:
        sub = feat_df[["time_to_next_request_seconds", "event_observed"]].dropna()
        n = min(30000, len(sub))
        sample = sub.sample(n=n, random_state=0) if len(sub) > n else sub
        sample.to_json(args.gap_dist_out, orient="records")
        print(f"wrote {args.gap_dist_out}", file=sys.stderr)

    if args.learning_curve_out:
        run_learning_curves(train_feat, feat_df, fold_rows, holdout_fold, fold_edges, prices,
                            semantics, schedule, train_rows, tenants, args.learning_curve_out)


def run_learning_curves(train_feat, feat_df, fold_rows, holdout_fold, fold_edges, prices,
                        semantics, schedule, train_rows, tenants, out_path):
    """AUC and net-$ (single GLOBAL threshold, tuned on the same train slice via the shared
    `tune_threshold` helper -- no per-tenant tuning here, to keep each point one clean
    number) at increasing train fractions, scored ONLY on the untouched holdout fold. Never
    produced before this script."""
    from sklearn.metrics import roc_auc_score
    holdout_rows = fold_rows[holdout_fold]
    window_end = fold_edges[holdout_fold + 1]
    holdout_ids = {r.request_id for r in holdout_rows}
    holdout_feat = feat_df[feat_df["request_id"].isin(holdout_ids) & feat_df["event_observed"]]
    y_holdout = holdout_feat["band_5m_1h"].astype(int).to_numpy()
    holdout_baseline_conv = per_conversation_costs(
        holdout_rows, fixed_5m_actions(holdout_rows), prices, semantics, schedule, window_end)
    holdout_baseline_usd = sum(holdout_baseline_conv.values())

    fracs = (0.1, 0.2, 0.3, 0.5, 0.7, 1.0)
    train_sorted = train_feat.sort_values("ts_ms")
    n_total = len(train_sorted)
    points = []
    for frac in fracs:
        n = max(200, int(n_total * frac))  # a floor so the smallest point can still fit
        sub_train_feat = train_sorted.iloc[:n]
        sub_ids = set(sub_train_feat["request_id"])
        sub_rows = [r for r in train_rows if r.request_id in sub_ids]
        cal_gbm, gbm_proba = fit_gbm_policy(sub_train_feat)
        sub_train_g = sub_train_feat.assign(p_gbm=gbm_proba(sub_train_feat))
        g_thr, _pt, _skip = tune_threshold("p_gbm", sub_train_g, sub_rows, tenants,
                                           prices, semantics, schedule,
                                           int(sub_train_feat["ts_ms"].max()), min_events=10**9)
        # min_events=1e9 forces every tenant into the fallback: a learning-curve POINT is a
        # single number, so it uses the global threshold only, same as this function's own
        # docstring says.
        holdout_proba = gbm_proba(holdout_feat)
        auc = float(roc_auc_score(y_holdout, holdout_proba)) if 0 < y_holdout.mean() < 1 else None
        p_by_id = dict(zip(holdout_feat["request_id"], holdout_proba))
        acts = {r.request_id: gate_action(r.cached_context, p_by_id.get(r.request_id, 0.0) >= g_thr)
                for r in holdout_rows}
        arm_conv = per_conversation_costs(holdout_rows, acts, prices, semantics, schedule, window_end)
        arm_usd = sum(arm_conv.values())
        pct = 100 * (holdout_baseline_usd - arm_usd) / holdout_baseline_usd if holdout_baseline_usd else None
        points.append({"train_frac_of_train_window": frac, "n_train_rows": n,
                       "auc_holdout": auc, "global_threshold": g_thr,
                       "holdout_delta_usd": holdout_baseline_usd - arm_usd,
                       "holdout_delta_pct": pct})
    with open(out_path, "w") as fh:
        json.dump({"holdout_fold": holdout_fold, "holdout_baseline_usd": holdout_baseline_usd,
                  "points": points}, fh, indent=2)
    print(f"wrote {out_path}", file=sys.stderr)


if __name__ == "__main__":
    main()
