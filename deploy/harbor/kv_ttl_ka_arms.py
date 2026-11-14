#!/usr/bin/env python3
"""KA-predictor's own additions to A4's arm comparison (reused as a library, not
reimplemented): flat, no-predictor ping-cap arms; a second calibrated hybrid with
tenant-to-global fallback and NO tenant one-hot (so it can score an unseen tenant at all);
tenant-level paired bootstrap (A4/run_all only had session-level); leave-one-tenant-out and
leave-one-model-out generalization; and calibration (Brier/log-loss/reliability) for the
two learned arms. Same train/fold split as kv_ttl_predictor_arms.run_all (train_frac=0.6,
3 folds) so every number here is directly comparable to reports/A4-keepalive.md's table.
"""
from __future__ import annotations
import argparse, json, os, sys, warnings
warnings.filterwarnings("ignore")
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import numpy as np
import pandas as pd

from kv_ttl_cost_model import (  # noqa: E402
    PriceBook, Semantics, PingSchedule, EXPIRE, WRITE_5M, PING_5M, derive,
    DEFAULT_DB, DEFAULT_PRICES,
)
from kv_ttl_predictor_arms import (  # noqa: E402
    load_ext_trajectories, pseudonymize_map, gate_action, stop_cluster,
    build_feature_frame, per_conversation_costs, bootstrap_ci, fold_cost,
    NUMERIC_FEATURES, MIN_PREFIX, HORIZON_5M_S, HORIZON_1H_S, BREAK_EVEN_P1H,
)

# NUMERIC_FEATURES only (no user_id one-hot): the point of hybrid-2 is that it must be
# scoreable on a tenant it never trained on, which a one-hot column structurally forbids
# (OneHotEncoder(handle_unknown="ignore") zeroes an unseen category, silently degrading to
# the intercept -- fine as a fallback, but logreg-v1 does that by ACCIDENT of feature choice,
# never measured; this hybrid is built to make that fallback the explicit, tuned mechanism).
HYBRID2_NUMERIC = NUMERIC_FEATURES
HYBRID2_CATEGORICAL = ("stop_cluster",)


def bootstrap_ci_tenant(baseline_by_conv: dict, arm_by_conv: dict, *, reps: int, seed: int) -> dict:
    """Same statistic as kv_ttl_predictor_arms.bootstrap_ci, but resamples TENANTS with
    replacement rather than conversations. A tenant contributes the SUM of its own
    conversations' costs, exactly the aggregation kvcache bills at, so one whale tenant is
    one draw here just as one long session is one draw in the session-level bootstrap --
    the two levels answer different robustness questions (session non-independence vs.
    tenant-mix sensitivity) and the brief asks for both."""
    by_tenant_base: dict[str, float] = {}
    by_tenant_arm: dict[str, float] = {}
    for k, v in baseline_by_conv.items():
        by_tenant_base[k[1]] = by_tenant_base.get(k[1], 0.0) + v
    for k, v in arm_by_conv.items():
        by_tenant_arm[k[1]] = by_tenant_arm.get(k[1], 0.0) + v
    tenants = sorted(by_tenant_base)
    if not tenants:
        return {"n_tenants": 0}
    rng = np.random.default_rng(seed)
    base = np.array([by_tenant_base[t] for t in tenants])
    arm = np.array([by_tenant_arm.get(t, by_tenant_base[t]) for t in tenants])
    n = len(tenants)
    deltas = np.empty(reps)
    for i in range(reps):
        idx = rng.integers(0, n, size=n)
        deltas[i] = base[idx].sum() - arm[idx].sum()
    point = base.sum() - arm.sum()
    return {
        "n_tenants": n,
        "point_delta_usd": float(point),
        "point_delta_pct": float(100 * point / base.sum()) if base.sum() else None,
        "ci95_delta_usd": [float(np.percentile(deltas, 2.5)), float(np.percentile(deltas, 97.5))],
    }


def flat_cap_actions(rows) -> dict[int, str]:
    return {r.request_id: gate_action(r.cached_context, True) for r in rows}


def tune_good_hours_unconditional(train_ext) -> set[int]:
    """Same shape as kv_ttl_predictor_arms.tune_good_hours but WITHOUT the stop_reason
    filter: pure clock/working-hours, the baseline the brief asks for as distinct from
    stop-reason-x-hour."""
    n = [0] * 24
    band = [0] * 24
    for r in train_ext:
        if r.idle_ms is None:
            continue
        h = r.hour_utc
        n[h] += 1
        if HORIZON_5M_S * 1000 <= r.idle_ms < HORIZON_1H_S * 1000:
            band[h] += 1
    return {h for h in range(24) if n[h] >= 20 and band[h] / n[h] >= BREAK_EVEN_P1H}


def clock_actions(rows, good_hours: set[int]) -> dict[int, str]:
    return {r.request_id: gate_action(r.cached_context, r.hour_utc in good_hours) for r in rows}


def age_turn_actions(rows, turn_cutoff: int) -> dict[int, str]:
    """Ping only on a conversation's first `turn_cutoff` turns -- the 'is this session
    still young' heuristic the brief asks for, distinct from stop_reason or the clock.
    Turn index is computed here (Request carries no turn field) by chronological order
    within (user, conversation, model), matching build_feature_frame's own convention."""
    by_key: dict[tuple, list] = {}
    for r in rows:
        by_key.setdefault(r.key, []).append(r)
    acts: dict[int, str] = {}
    for group in by_key.values():
        for i, r in enumerate(sorted(group, key=lambda r: (r.ts_ms, r.request_id)), start=1):
            acts[r.request_id] = gate_action(r.cached_context, i <= turn_cutoff)
    return acts


def fit_hybrid2(train_feat: pd.DataFrame, train_rows, tenants, prices, semantics, schedule,
                window_end_ms):
    """Calibrated logistic regression (isotonic, 3-fold internal CV) over NUMERIC_FEATURES +
    stop_cluster only -- no user_id -- plus a per-tenant threshold tuned the same way
    tune_historical_probability tunes its (p5,p1) pair: grid-searched on that tenant's own
    train rows, falling back to the GLOBAL tuned threshold below a 30-event floor. This is
    the second calibrated hybrid the brief asks for, and it differs from A4's
    stop-reason-x-tenant-history in kind: that one is a rule/History-cell hybrid, this one is
    a calibrated ML probability with a tenant-tuned decision threshold on top."""
    from sklearn.compose import ColumnTransformer
    from sklearn.impute import SimpleImputer
    from sklearn.linear_model import LogisticRegression
    from sklearn.pipeline import Pipeline
    from sklearn.preprocessing import OneHotEncoder, StandardScaler
    from sklearn.calibration import CalibratedClassifierCV

    fit_rows = train_feat[train_feat["event_observed"]].copy()
    y = fit_rows["band_5m_1h"].astype(int).to_numpy()
    numeric = Pipeline([("imputer", SimpleImputer(strategy="median")), ("scaler", StandardScaler())])
    categorical = Pipeline([("imputer", SimpleImputer(strategy="most_frequent")),
                            ("onehot", OneHotEncoder(handle_unknown="ignore"))])
    pre = ColumnTransformer([("num", numeric, list(HYBRID2_NUMERIC)),
                             ("cat", categorical, list(HYBRID2_CATEGORICAL))])
    base_clf = LogisticRegression(max_iter=2000, C=1.0, random_state=0)
    pipe = Pipeline([("pre", pre), ("clf", CalibratedClassifierCV(base_clf, method="isotonic", cv=3))])
    X = fit_rows[list(HYBRID2_NUMERIC) + list(HYBRID2_CATEGORICAL)]
    pipe.fit(X, y)

    def proba_for(df: pd.DataFrame) -> np.ndarray:
        return pipe.predict_proba(df[list(HYBRID2_NUMERIC) + list(HYBRID2_CATEGORICAL)])[:, 1]

    train_feat = train_feat.assign(p_hybrid2=proba_for(train_feat))
    by_tenant_feat = {t: train_feat[train_feat["user_id"] == t] for t in tenants}

    def cost_at(threshold: float, feat_slice: pd.DataFrame, rows_slice) -> float:
        ids_ping = set(feat_slice.loc[feat_slice["p_hybrid2"] >= threshold, "request_id"])
        acts = {r.request_id: gate_action(r.cached_context, r.request_id in ids_ping)
                for r in rows_slice}
        return sum(per_conversation_costs(rows_slice, acts, prices, semantics, schedule,
                                          window_end_ms).values())

    grid = (0.01, 0.02, 0.03, 0.05, 0.08, 0.12, 0.2, 0.35, 0.5)
    rows_by_tenant = {}
    for r in train_rows:
        rows_by_tenant.setdefault(r.user, []).append(r)

    best_global, best_cost = 0.05, float("inf")
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
        if n_actually_done.get(t, 0) < 30:
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

    return pipe, best_global, per_tenant_thr, skipped, proba_for


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--db", default=DEFAULT_DB)
    ap.add_argument("--prices", default=DEFAULT_PRICES)
    ap.add_argument("--train-frac", type=float, default=0.6)
    ap.add_argument("--folds", type=int, default=3)
    ap.add_argument("--bootstrap", type=int, default=400)
    ap.add_argument("--seed", type=int, default=0)
    ap.add_argument("--out", default="ka_arms_result.json",
                    help="full result set (aggregates + pseudonymized per-tenant $, no "
                         "transcript content, no real tenant id)")
    args = ap.parse_args()

    ext_rows = load_ext_trajectories(args.db)
    prices = PriceBook.from_operator_file(args.prices)
    semantics = Semantics()
    schedule = PingSchedule()  # max_pings=2, the live default

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
    print(f"train<= {train_cut} ({len(train_rows):,}); folds: "
          f"{[len(f) for f in fold_rows]}", file=sys.stderr)

    good_hours = tune_good_hours_unconditional(train_rows)
    print(f"clock (unconditional) good hours UTC: {sorted(good_hours)}", file=sys.stderr)

    feat_df = build_feature_frame(ext_rows, window_end_ms=hi)
    train_feat = feat_df[feat_df["ts_ms"] <= train_cut].copy()
    pipe2, hy2_global_thr, hy2_per_tenant_thr, hy2_skipped, hy2_proba = fit_hybrid2(
        train_feat, train_rows, tenants, prices, semantics, schedule, train_cut)
    print(f"hybrid2: global threshold={hy2_global_thr}, "
          f"tenants tuned={len(hy2_per_tenant_thr)}, skipped={len(hy2_skipped)}", file=sys.stderr)

    def hybrid2_actions_for(rows) -> dict[int, str]:
        ids = {r.request_id for r in rows}
        sub = feat_df[feat_df["request_id"].isin(ids)]
        proba = hy2_proba(sub)
        p_by_id = dict(zip(sub["request_id"], proba))
        acts = {}
        for r in rows:
            thr = hy2_per_tenant_thr.get(r.user, hy2_global_thr)
            acts[r.request_id] = gate_action(r.cached_context, p_by_id.get(r.request_id, 0.0) >= thr)
        return acts

    arms = {
        "flat-cap-1": lambda rows: flat_cap_actions(rows),  # schedule overridden per-call below
        "flat-cap-2": lambda rows: flat_cap_actions(rows),
        "flat-cap-6": lambda rows: flat_cap_actions(rows),
        "clock-unconditional": lambda rows: clock_actions(rows, good_hours),
        "age-turn-le3": lambda rows: age_turn_actions(rows, 3),
        "hybrid2-calibrated-tenant-fallback": lambda rows: hybrid2_actions_for(rows),
    }
    schedules = {"flat-cap-1": PingSchedule(max_pings=1), "flat-cap-6": PingSchedule(max_pings=6)}

    pooled_conv: dict[str, dict] = {name: {} for name in arms}
    pooled_baseline_conv: dict = {}
    fold_results = []
    for fi, rows in enumerate(fold_rows):
        if not rows:
            continue
        window_end = fold_edges[fi + 1]
        base_acts = {r.request_id: gate_action(r.cached_context, False) for r in rows}
        base_cost, base_local = fold_cost(rows, base_acts, prices, semantics, schedule, window_end)
        base_conv = per_conversation_costs(base_local, base_acts, prices, semantics, schedule, window_end)
        for k, v in base_conv.items():
            pooled_baseline_conv[(fi, *k)] = v
        entry = {"fold": fi, "n_rows": len(rows), "baseline_usd": base_cost.total_usd, "arms": {}}
        for name, fn in arms.items():
            sched = schedules.get(name, schedule)
            acts = fn(rows)
            arm_cost, local = fold_cost(rows, acts, prices, semantics, sched, window_end)
            local_acts = {r.request_id: acts.get(r.request_id, EXPIRE) for r in local}
            arm_conv = per_conversation_costs(local, local_acts, prices, semantics, sched, window_end)
            for k, v in arm_conv.items():
                pooled_conv[name][(fi, *k)] = v
            entry["arms"][name] = {"total_usd": arm_cost.total_usd, "pings": arm_cost.pings,
                                    "hit_rate_pct": arm_cost.hit_rate_pct}
        fold_results.append(entry)

    pooled_session = {name: bootstrap_ci(pooled_baseline_conv, pooled_conv[name],
                                         reps=args.bootstrap, seed=args.seed) for name in arms}
    pooled_tenant = {name: bootstrap_ci_tenant(pooled_baseline_conv, pooled_conv[name],
                                               reps=args.bootstrap, seed=args.seed) for name in arms}

    per_tenant_report = {}
    n_ad_test = {}
    for r in all_test_rows:
        if stop_cluster(r.stop_reason) == "actually_done":
            n_ad_test[r.user] = n_ad_test.get(r.user, 0) + 1
    for t in tenants:
        pseudo = tenant_pseudo[t]
        n_ad = n_ad_test.get(t, 0)
        base_t = sum(v for k, v in pooled_baseline_conv.items() if k[1] == t)
        entry = {"n_actually_done_test": n_ad, "baseline_usd": base_t, "arms": {}}
        for name in arms:
            arm_t = sum(v for k, v in pooled_conv[name].items() if k[1] == t)
            pct = 100 * (base_t - arm_t) / base_t if base_t else None
            entry["arms"][name] = {"usd": arm_t, "percent_savings": pct,
                                   "harmed": (pct is not None and pct < 0)}
        per_tenant_report[pseudo] = entry

    # ---- calibration: hybrid2's own probabilities against the realized band label, on the
    # pooled TEST folds only (never on train) ----------------------------------------------
    test_ids = {r.request_id for r in all_test_rows}
    test_feat = feat_df[feat_df["request_id"].isin(test_ids) & feat_df["event_observed"]]
    y_true = test_feat["band_5m_1h"].astype(int).to_numpy()
    y_prob = hy2_proba(test_feat)
    from sklearn.metrics import brier_score_loss, log_loss, roc_auc_score
    from sklearn.calibration import calibration_curve
    frac_pos, mean_pred = calibration_curve(y_true, y_prob, n_bins=10, strategy="quantile")
    ece = float(np.mean(np.abs(frac_pos - mean_pred)))
    calibration = {
        "n": int(len(y_true)), "base_rate": float(y_true.mean()),
        "brier": float(brier_score_loss(y_true, y_prob)),
        "log_loss": float(log_loss(y_true, np.clip(y_prob, 1e-6, 1 - 1e-6))),
        "auc": float(roc_auc_score(y_true, y_prob)) if 0 < y_true.mean() < 1 else None,
        "ece_10bin_quantile": ece,
        "reliability_frac_pos": frac_pos.tolist(), "reliability_mean_pred": mean_pred.tolist(),
    }

    # ---- generalization: leave-one-tenant-out, top-5 tenants by test volume --------------
    tenant_test_n = sorted(n_ad_test.items(), key=lambda kv: -kv[1])[:5]
    loto = {}
    for t, _n in tenant_test_n:
        train_wo = train_feat[train_feat["user_id"] != t]
        t_test_feat = feat_df[(feat_df["request_id"].isin(test_ids)) & (feat_df["user_id"] == t)
                              & feat_df["event_observed"]]
        if len(t_test_feat) < 10 or train_wo["event_observed"].sum() < 30:
            loto[tenant_pseudo[t]] = {"skipped": True}
            continue
        pipe_wo, thr_wo, _, _, proba_wo = fit_hybrid2(
            train_wo, [r for r in train_rows if r.user != t], [tt for tt in tenants if tt != t],
            prices, semantics, schedule, train_cut)
        yt = t_test_feat["band_5m_1h"].astype(int).to_numpy()
        pt = proba_wo(t_test_feat)
        loto[tenant_pseudo[t]] = {
            "skipped": False, "n_test": int(len(yt)), "base_rate": float(yt.mean()),
            "auc_excl_tenant": float(roc_auc_score(yt, pt)) if 0 < yt.mean() < 1 else None,
            "brier_excl_tenant": float(brier_score_loss(yt, pt)),
        }

    # ---- generalization: leave-one-model-out, top-4 models --------------------------------
    model_n = {}
    for r in all_test_rows:
        model_n[r.model] = model_n.get(r.model, 0) + 1
    top_models = [m for m, _ in sorted(model_n.items(), key=lambda kv: -kv[1])[:4]]
    lomo = {}
    for m in top_models:
        train_wo = train_feat[train_feat["model"] != m]
        m_test_feat = feat_df[(feat_df["request_id"].isin(test_ids)) & (feat_df["model"] == m)
                              & feat_df["event_observed"]]
        if len(m_test_feat) < 10 or train_wo["event_observed"].sum() < 30:
            lomo[m] = {"skipped": True}
            continue
        pipe_wo, thr_wo, _, _, proba_wo = fit_hybrid2(
            train_wo, [r for r in train_rows if r.model != m], tenants,
            prices, semantics, schedule, train_cut)
        yt = m_test_feat["band_5m_1h"].astype(int).to_numpy()
        pt = proba_wo(m_test_feat)
        lomo[m] = {
            "skipped": False, "n_test": int(len(yt)), "base_rate": float(yt.mean()),
            "auc_excl_model": float(roc_auc_score(yt, pt)) if 0 < yt.mean() < 1 else None,
            "brier_excl_model": float(brier_score_loss(yt, pt)),
        }

    result = {
        "window": {"since_ms": lo, "until_ms": hi, "train_cut_ms": train_cut, "fold_edges_ms": fold_edges},
        "fold_results": fold_results,
        "pooled_session_bootstrap": pooled_session,
        "pooled_tenant_bootstrap": pooled_tenant,
        "per_tenant": per_tenant_report,
        "hybrid2": {"global_threshold": hy2_global_thr, "per_tenant_thresholds":
                    {tenant_pseudo[t]: v for t, v in hy2_per_tenant_thr.items()},
                    "skipped_tenants": [tenant_pseudo[t] for t in hy2_skipped]},
        "calibration_hybrid2_pooled_test": calibration,
        "leave_one_tenant_out_hybrid2": loto,
        "leave_one_model_out_hybrid2": lomo,
    }
    with open(args.out, "w") as fh:
        json.dump(result, fh, indent=2, default=str)
    print(f"wrote {args.out}", file=sys.stderr)
    print(json.dumps({"pooled_session_bootstrap": pooled_session,
                      "pooled_tenant_bootstrap": pooled_tenant,
                      "calibration": calibration}, indent=2, default=str))


if __name__ == "__main__":
    main()
