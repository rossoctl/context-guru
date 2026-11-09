#!/usr/bin/env python3
"""Rule-based and learned TTL-decision arms, scored against fixed-5m via kv_ttl_cost_model.

Extends docs/how-to/kv-cache-ttl.md and docs/results/kv-ttl-predictor-features.md: builds
the stop_reason-gated ping, a per-tenant-tuned historical-probability arm (a Python port of
kvcache.History/HistoricalProbability, leak-free by the same construction), a stop_reason x
hour-of-day rule, and a logistic-regression learned arm over engineered features (reusing
kv_ttl_survival_predictor's feature engineering as a library). A gradient-boosted tree is
fit purely for a feature-importance comparison and is not scored as a policy.

SECURITY BOUNDARY: this script reads raw per-request rows (tenant_id, session_id,
stop_reason, timestamps) from the live DB. It must be invoked as `sudo -n -u cg
<venv>/python3 kv_ttl_predictor_arms.py ...` so the whole read + feature engineering + fit
+ simulate pipeline runs inside that one process. Every value that leaves this process —
stdout, --out, --coefs-out — is an AGGREGATE: dollar totals, percentages, coefficients,
counts, feature importances, keyed by PSEUDONYMIZED tenant ids (t01, t02, ...), never the
real tenant_id and never a per-row value. See pseudonymize() below, ported from
the research feature extractor's function of the same name.

Usage (read-only against the live store, as cg):
    sudo -n -u cg /tmp/kvpred-venv/bin/python3 kv_ttl_predictor_arms.py \
        --db /var/lib/context-guru/cg.db --prices /etc/context-guru/prices.yaml \
        --out /tmp/kv_ttl_arms_result.json --coefs-out /tmp/kvttl_logreg_v1_coefs.json

The interpreter and this script must themselves live somewhere world-readable/executable
(the venv and a copy of this file plus kv_ttl_cost_model.py/kv_ttl_survival_predictor.py
under /tmp) for `cg` to run them — the repo checkout is not traversable by `cg`.
"""
from __future__ import annotations

import argparse
import json
import os
import sqlite3
import sys
from dataclasses import dataclass, replace as dc_replace
from urllib.parse import quote

import numpy as np
import pandas as pd

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from kv_ttl_cost_model import (  # noqa: E402
    EXPIRE, WRITE_5M, WRITE_1H, PING_5M,
    PriceBook, Request, Semantics, PingSchedule, derive, evaluate, compare,
)
from kv_ttl_survival_predictor import (  # noqa: E402
    KVTTLTimePredictor, build_next_request_targets,
)

# ── stop_reason clusters (docs/results/kv-ttl-predictor-features.md) ────────

STILL_WORKING = frozenset({"tool_use", "stop_sequence", "tool_calls", "length", "content_filter"})
LOOKS_DONE_ISNT = frozenset({"stop", ""})
ACTUALLY_DONE = frozenset({"end_turn", "max_tokens", "refusal"})


def stop_cluster(stop_reason: str) -> str:
    sr = stop_reason or ""
    if sr in ACTUALLY_DONE:
        return "actually_done"
    if sr in LOOKS_DONE_ISNT:
        return "looks_done_isnt"
    return "still_working"


# ── kvcache.BucketOf, ported (kvcache/kvcache.go) ────────────────────────────

def bucket_of(hour_utc: int) -> str:
    if hour_utc < 0 or hour_utc > 23:
        return "night"
    if hour_utc >= 18:
        return "evening"
    if hour_utc >= 12:
        return "afternoon"
    if hour_utc >= 6:
        return "morning"
    return "night"


# ── kvcache.History/Stats, ported (kvcache/strategy.go) ─────────────────────
# Leak-free by construction: Observe() is only ever called on a gap AFTER it has closed,
# and the fallback chain (user+model+bucket -> user+model -> user -> model -> global) and
# minCell=6 are copied verbatim rather than reinvented, per the study's own instruction to
# reuse kvcache.Stats/History rather than build a second fallback ladder.

MIN_CELL = 6
HORIZON_5M_S = 300.0
HORIZON_1H_S = 3600.0


class History:
    __slots__ = ("cells",)

    def __init__(self) -> None:
        self.cells: dict[tuple[str, str, str], list[float]] = {}

    @staticmethod
    def _keys(user: str, model: str, bucket: str) -> list[tuple[str, str, str]]:
        return [(user, model, bucket), (user, model, ""), (user, "", ""),
                ("", model, ""), ("", "", "")]

    def observe(self, user: str, model: str, bucket: str, gap_s: float) -> None:
        gap_s = max(0.0, gap_s)
        for k in self._keys(user, model, bucket):
            self.cells.setdefault(k, []).append(gap_s)

    def reuse_within(self, user: str, model: str, bucket: str, horizon_s: float) -> tuple[float, int]:
        for k in self._keys(user, model, bucket):
            gaps = self.cells.get(k)
            if gaps is not None and len(gaps) >= MIN_CELL:
                hits = sum(1 for g in gaps if g <= horizon_s)
                return hits / len(gaps), len(gaps)
        last = self.cells.get(("", "", ""))
        if last:
            hits = sum(1 for g in last if g <= horizon_s)
            return hits / len(last), len(last)
        return 0.0, 0


def historical_probability_long_hold(prefix: int, price, semantics: Semantics, max_pings: int) -> str:
    """Port of kvcache.HistoricalProbability.longHold: write_1h vs write_5m+K pings."""
    if not price.known:
        return WRITE_1H
    write_1h = prefix * price.write_1h
    ping_hold = prefix * price.write_5m + max_pings * price.keep_alive_cost(prefix, semantics)
    return PING_5M if ping_hold < write_1h else WRITE_1H


def historical_probability_decide(r, hist: History, p5: float, p1: float, min_prefix: int,
                                   prices: PriceBook, semantics: Semantics, max_pings: int) -> str:
    if r.cached_context < min_prefix:
        return EXPIRE
    b = bucket_of(r.hour_utc)
    p, n = hist.reuse_within(r.user, r.model, b, HORIZON_5M_S)
    if n > 0 and p >= p5:
        return WRITE_5M
    p, n = hist.reuse_within(r.user, r.model, b, HORIZON_1H_S)
    if n > 0 and p >= p1:
        return historical_probability_long_hold(r.cached_context, prices.for_model(r.model),
                                                 semantics, max_pings)
    return EXPIRE


def replay_history_actions(all_rows_sorted: list, decide_ids: set[int], *,
                            per_tenant_thresholds: dict[str, tuple[float, float]],
                            default_threshold: tuple[float, float], min_prefix: int,
                            prices: PriceBook, semantics: Semantics, max_pings: int,
                            history: "History | None" = None) -> tuple[dict[int, str], History]:
    """One global chronological pass: observe each conversation's just-closed gap (bucketed
    at the PREVIOUS request's hour, exactly kvcache/simulate.go's `hist.Observe(r.User,
    r.Model, BucketAt(st.lastTS), gap)`), THEN decide for rows in decide_ids. Rows not in
    decide_ids (training warm-up rows) are observed but never decided, so this one function
    serves both warm-up and scoring without duplicating the loop."""
    hist = history if history is not None else History()
    last: dict[tuple[str, str, str], tuple[int, int]] = {}  # key -> (ts_ms, hour_utc)
    actions: dict[int, str] = {}
    for r in all_rows_sorted:
        key = r.key
        prev = last.get(key)
        if prev is not None:
            gap_s = max(0.0, (r.ts_ms - prev[0]) / 1000.0)
            hist.observe(r.user, r.model, bucket_of(prev[1]), gap_s)
        if r.request_id in decide_ids:
            p5, p1 = per_tenant_thresholds.get(r.user, default_threshold)
            actions[r.request_id] = historical_probability_decide(
                r, hist, p5, p1, min_prefix, prices, semantics, max_pings)
        last[key] = (r.ts_ms, r.hour_utc)
    return actions, hist


# ── data model ────────────────────────────────────────────────────────────

@dataclass
class ExtRequest(Request):
    """Request plus the two columns these arms need that the pinned cost model doesn't
    carry. hour_utc is inherited unchanged from Request."""

    stop_reason: str = ""
    agent: str = ""


DEFAULT_DB = "/var/lib/context-guru/cg.db"
DEFAULT_PRICES = "/etc/context-guru/prices.yaml"


def load_ext_trajectories(db_path: str) -> list[ExtRequest]:
    # mode=ro only, matching kv_ttl_cost_model.load_trajectories: &immutable=1 skips SQLite's
    # own change-detection and throws intermittent "database disk image is malformed" errors
    # against this live, actively-checkpointing WAL database (see kv-ttl-predictor-features.md).
    uri = f"file:{quote(db_path)}?mode=ro"
    con = sqlite3.connect(uri, uri=True)
    try:
        have = {r[1] for r in con.execute("PRAGMA table_info(requests)")}
        conds = ["session_id <> ''", "token_accounting <> 'missing'"]
        if "keepalive" in have:
            conds.append("keepalive = 0")
        cols = ["id", "tenant_id", "session_id", "ts", "model", "fresh_input",
                "output_tokens", "cache_read", "cache_write", "cache_miss_reason",
                "upstream_ms", "cost_usd", "stop_reason", "agent"]
        ttl_col = "cache_ttl" in have
        if ttl_col:
            cols.append("cache_ttl")
        sql = f"SELECT {', '.join(cols)} FROM requests WHERE {' AND '.join(conds)} ORDER BY ts, id"
        rows = [
            ExtRequest(request_id=r[0], user=r[1], conversation=r[2], ts_ms=r[3], model=r[4],
                       input_tokens=r[5] or 0, output_tokens=r[6] or 0,
                       cached_context=(r[7] or 0) + (r[8] or 0), miss_reason=r[9] or "",
                       hit=(r[9] or "") == "hit", upstream_ms=r[10] or 0.0,
                       billed_usd=r[11] or 0.0, stop_reason=r[12] or "", agent=r[13] or "",
                       ttl_recorded=(r[14] if ttl_col else None))
            for r in con.execute(sql)
        ]
    finally:
        con.close()
    return derive(rows)


def pseudonymize_map(tenants: list[str]) -> dict[str, str]:
    return {t: f"t{i+1:02d}" for i, t in enumerate(sorted(set(tenants)))}


# ── per-conversation cost decomposition, for a session/tenant-level bootstrap ──

def per_conversation_costs(rows: list, actions: dict[int, str], prices: PriceBook,
                            semantics: Semantics, schedule: PingSchedule,
                            window_end_ms: int) -> dict[tuple[str, str, str], float]:
    """Cost is additive across conversations (kvcache's state is keyed per (user,
    conversation, model) and independent): evaluating each conversation's rows in isolation
    with the SAME window_end_ms reproduces exactly the total a single evaluate() over all of
    them would give, and gives the per-conversation breakdown a session-level bootstrap
    needs without a second cost engine."""
    by_key: dict[tuple[str, str, str], list] = {}
    for r in rows:
        by_key.setdefault(r.key, []).append(r)
    out: dict[tuple[str, str, str], float] = {}
    for key, group in by_key.items():
        acts = {r.request_id: actions.get(r.request_id, EXPIRE) for r in group}
        out[key] = evaluate(group, acts, prices, semantics=semantics, schedule=schedule,
                            window_end_ms=window_end_ms).total_usd
    return out


def bootstrap_ci(baseline_by_conv: dict, arm_by_conv: dict, *, reps: int, seed: int) -> dict:
    """Resample CONVERSATIONS (not rows) with replacement, per the leak/correlation note:
    rows within a session are correlated, so the unit of resampling must be the session."""
    keys = list(baseline_by_conv.keys())
    if not keys:
        return {"n": 0}
    rng = np.random.default_rng(seed)
    base = np.array([baseline_by_conv[k] for k in keys])
    arm = np.array([arm_by_conv.get(k, baseline_by_conv[k]) for k in keys])
    n = len(keys)
    deltas = np.empty(reps)
    for i in range(reps):
        idx = rng.integers(0, n, size=n)
        deltas[i] = base[idx].sum() - arm[idx].sum()  # positive = arm cheaper
    point = base.sum() - arm.sum()
    return {
        "n_conversations": n,
        "point_delta_usd": float(point),
        "point_delta_pct": float(100 * point / base.sum()) if base.sum() else None,
        "ci95_delta_usd": [float(np.percentile(deltas, 2.5)), float(np.percentile(deltas, 97.5))],
        "baseline_usd": float(base.sum()),
        "arm_usd": float(arm.sum()),
    }


# ── feature engineering (reuses kv_ttl_survival_predictor as a library) ─────

def build_feature_frame(ext_rows: list[ExtRequest], window_end_ms: int) -> pd.DataFrame:
    frame = pd.DataFrame([{
        "request_id": r.request_id, "user_id": r.user, "conversation_id": r.conversation,
        "model": r.model, "agent": r.agent, "stop_cluster": stop_cluster(r.stop_reason),
        "cached_context": r.cached_context,
        "request_time": pd.Timestamp(r.ts_ms, unit="ms", tz="UTC"),
        "ts_ms": r.ts_ms,
    } for r in ext_rows])
    frame["turn"] = frame.groupby(["user_id", "conversation_id", "model"]).cumcount() + 1.0
    frame["request_time"] = pd.to_datetime(frame["request_time"], utc=True)

    labelled = build_next_request_targets(
        frame, compatibility_columns=("user_id", "conversation_id", "model"),
        observation_end=pd.Timestamp(window_end_ms, unit="ms", tz="UTC"))
    labelled["band_5m_1h"] = (
        labelled["event_observed"]
        & (labelled["time_to_next_request_seconds"] >= HORIZON_5M_S)
        & (labelled["time_to_next_request_seconds"] < HORIZON_1H_S)
    )

    predictor = KVTTLTimePredictor(user_column="user_id", time_column="request_time")
    engineered = predictor._engineer_request_features(labelled)  # noqa: SLF001 - reuse, don't re-derive
    # _engineer_request_features re-sorts/resets the index; request_id travels with it, so
    # merge back onto the labelled frame's row identity rather than assume position.
    engineered = engineered.set_index("request_id")
    out = labelled.set_index("request_id").join(
        engineered[[c for c in engineered.columns if c not in labelled.columns]])
    return out.reset_index()


NUMERIC_FEATURES = (
    "request_hour_sin", "request_hour_cos", "request_weekday_sin", "request_weekday_cos",
    "previous_gap_seconds", "rolling_gap_median_seconds", "ewma_gap_seconds",
    "past_return_rate_5m", "past_return_rate_15m", "past_return_rate_60m",
    "requests_in_previous_10m", "requests_in_previous_60m", "user_history_count", "turn",
)
CATEGORICAL_FEATURES = ("stop_cluster", "user_id")


def fit_logreg(train_df: pd.DataFrame):
    from sklearn.compose import ColumnTransformer
    from sklearn.impute import SimpleImputer
    from sklearn.linear_model import LogisticRegression
    from sklearn.pipeline import Pipeline
    from sklearn.preprocessing import OneHotEncoder, StandardScaler

    fit_rows = train_df[train_df["event_observed"]].copy()
    y = fit_rows["band_5m_1h"].astype(int).to_numpy()
    numeric = Pipeline([("imputer", SimpleImputer(strategy="median")), ("scaler", StandardScaler())])
    categorical = Pipeline([("imputer", SimpleImputer(strategy="most_frequent")),
                            ("onehot", OneHotEncoder(handle_unknown="ignore"))])
    pre = ColumnTransformer([("num", numeric, list(NUMERIC_FEATURES)),
                             ("cat", categorical, list(CATEGORICAL_FEATURES))])
    clf = LogisticRegression(max_iter=2000, C=1.0, random_state=0)
    pipe = Pipeline([("pre", pre), ("clf", clf)])
    pipe.fit(fit_rows[list(NUMERIC_FEATURES) + list(CATEGORICAL_FEATURES)], y)
    return pipe, len(fit_rows), int(y.sum())


def flatten_logreg(pipe, tenant_pseudo: dict[str, str]) -> dict:
    """Fold the fitted ColumnTransformer(StandardScaler + OneHotEncoder) + LogisticRegression
    into raw-space weight = coef/scale, intercept += -coef*mean/scale, so the exported JSON
    is a plain dot-product-plus-sigmoid a Go port can apply with no preprocessing step."""
    pre = pipe.named_steps["pre"]
    clf = pipe.named_steps["clf"]
    coefs = clf.coef_.ravel()
    intercept = float(clf.intercept_[0])

    weights: dict[str, float] = {}
    idx = 0
    num_pipe = pre.named_transformers_["num"]
    scaler = num_pipe.named_steps["scaler"]
    for i, name in enumerate(NUMERIC_FEATURES):
        w = coefs[idx]
        mean, scale = scaler.mean_[i], scaler.scale_[i] or 1.0
        weights[name] = float(w / scale)
        intercept += float(-w * mean / scale)
        idx += 1
    cat_pipe = pre.named_transformers_["cat"]
    onehot = cat_pipe.named_steps["onehot"]
    for feat_name, categories in zip(CATEGORICAL_FEATURES, onehot.categories_):
        for cat in categories:
            label = tenant_pseudo.get(cat, cat) if feat_name == "user_id" else cat
            key = f"{feat_name}={label}"
            weights[key] = float(coefs[idx])
            idx += 1
    return {"intercept": intercept, "weights": weights,
            "numeric_features": list(NUMERIC_FEATURES),
            "categorical_features": list(CATEGORICAL_FEATURES)}


def gbm_feature_importance(train_df: pd.DataFrame, tenant_pseudo: dict[str, str]) -> list[dict]:
    from sklearn.compose import ColumnTransformer
    from sklearn.ensemble import GradientBoostingClassifier
    from sklearn.impute import SimpleImputer
    from sklearn.pipeline import Pipeline
    from sklearn.preprocessing import OneHotEncoder

    fit_rows = train_df[train_df["event_observed"]].copy()
    y = fit_rows["band_5m_1h"].astype(int).to_numpy()
    numeric = Pipeline([("imputer", SimpleImputer(strategy="median"))])
    categorical = Pipeline([("imputer", SimpleImputer(strategy="most_frequent")),
                            ("onehot", OneHotEncoder(handle_unknown="ignore"))])
    pre = ColumnTransformer([("num", numeric, list(NUMERIC_FEATURES)),
                             ("cat", categorical, list(CATEGORICAL_FEATURES))])
    X = pre.fit_transform(fit_rows[list(NUMERIC_FEATURES) + list(CATEGORICAL_FEATURES)])
    gbm = GradientBoostingClassifier(random_state=0, n_estimators=100, max_depth=3)
    gbm.fit(X, y)
    # Built by hand from onehot.categories_, not get_feature_names_out(): a category value IS
    # the real tenant_id for the user_id column, and it must never reach a name string that
    # leaves this process. tenant_pseudo relabels it before the name is ever formed.
    names = list(NUMERIC_FEATURES)
    onehot = pre.named_transformers_["cat"].named_steps["onehot"]
    for feat_name, categories in zip(CATEGORICAL_FEATURES, onehot.categories_):
        for cat in categories:
            label = tenant_pseudo.get(cat, cat) if feat_name == "user_id" else cat
            names.append(f"{feat_name}={label}")
    imps = sorted(zip(names, gbm.feature_importances_), key=lambda kv: -kv[1])
    return [{"feature": n, "importance": float(v)} for n, v in imps]


# ── the simple rule arms ─────────────────────────────────────────────────

MIN_PREFIX = 20_000  # kvcache.DefaultMinPrefix
MAX_PINGS = 2
BREAK_EVEN_P1H = 0.08  # docs/results/kv-ttl-predictor-features.md's derivation


def gate_action(cached_context: int, would_ping: bool) -> str:
    if cached_context < MIN_PREFIX:
        return EXPIRE
    return PING_5M if would_ping else WRITE_5M


def stop_reason_gate_actions(rows: list[ExtRequest]) -> dict[int, str]:
    """Ping only on the 'actually done' cluster; never on the other two."""
    return {r.request_id: gate_action(r.cached_context, stop_cluster(r.stop_reason) == "actually_done")
            for r in rows}


def tune_good_hours(train_ext: list[ExtRequest]) -> list[int]:
    n = [0] * 24
    band = [0] * 24
    for r in train_ext:
        if stop_cluster(r.stop_reason) != "actually_done" or r.idle_ms is None:
            continue
        h = r.hour_utc
        n[h] += 1
        if HORIZON_5M_S * 1000 <= r.idle_ms < HORIZON_1H_S * 1000:
            band[h] += 1
    return [h for h in range(24) if n[h] >= 20 and band[h] / n[h] >= BREAK_EVEN_P1H]


def stop_reason_hour_actions(rows: list[ExtRequest], good_hours: set[int]) -> dict[int, str]:
    return {r.request_id: gate_action(
                r.cached_context,
                stop_cluster(r.stop_reason) == "actually_done" and r.hour_utc in good_hours)
            for r in rows}


def fixed_5m_actions(rows) -> dict[int, str]:
    return {r.request_id: WRITE_5M for r in rows}


# ── historical-probability tuning ────────────────────────────────────────

def tune_historical_probability(train_sorted: list[ExtRequest], tenants: list[str], *,
                                 prices: PriceBook, semantics: Semantics, window_end_ms: int,
                                 grid_p5=(0.5, 0.7), grid_p1=(0.05, 0.08, 0.15, 0.3),
                                 min_signal: int = 30) -> tuple[dict, tuple, list]:
    schedule = PingSchedule()
    by_tenant: dict[str, list] = {}
    for r in train_sorted:
        by_tenant.setdefault(r.user, []).append(r)

    def cost_of(ids: set[int], threshold: tuple[float, float], score_rows: list) -> float:
        actions, _ = replay_history_actions(
            train_sorted, ids, per_tenant_thresholds={}, default_threshold=threshold,
            min_prefix=MIN_PREFIX, prices=prices, semantics=semantics, max_pings=MAX_PINGS)
        return sum(per_conversation_costs(score_rows, actions, prices, semantics, schedule,
                                          window_end_ms).values())

    all_ids = {r.request_id for r in train_sorted}
    best_global, best_cost = (0.5, 0.5), float("inf")
    for p5 in grid_p5:
        for p1 in grid_p1:
            c = cost_of(all_ids, (p5, p1), train_sorted)
            if c < best_cost:
                best_global, best_cost = (p5, p1), c

    n_actually_done: dict[str, int] = {}
    for r in train_sorted:
        if stop_cluster(r.stop_reason) == "actually_done":
            n_actually_done[r.user] = n_actually_done.get(r.user, 0) + 1

    per_tenant: dict[str, tuple[float, float]] = {}
    skipped: list[str] = []
    for t in tenants:
        if n_actually_done.get(t, 0) < min_signal:
            skipped.append(t)
            continue
        t_rows = by_tenant.get(t, [])
        t_ids = {r.request_id for r in t_rows}
        best_t, best_t_cost = best_global, float("inf")
        for p5 in grid_p5:
            for p1 in grid_p1:
                c = cost_of(t_ids, (p5, p1), t_rows)
                if c < best_t_cost:
                    best_t, best_t_cost = (p5, p1), c
        per_tenant[t] = best_t
    return per_tenant, best_global, skipped


# ── evaluation harness ───────────────────────────────────────────────────

def fold_cost(rows: list[ExtRequest], actions: dict[int, str], prices: PriceBook,
              semantics: Semantics, schedule: PingSchedule, window_end_ms: int):
    """Re-derive the fold's rows in isolation (established convention: a fold is scored as
    its own trajectory set, so 'the next request' never reaches outside the slice being
    scored) and evaluate."""
    local = derive([dc_replace(r) for r in rows])
    local_actions = {r.request_id: actions.get(r.request_id, EXPIRE) for r in local}
    return evaluate(local, local_actions, prices, semantics=semantics, schedule=schedule,
                    window_end_ms=window_end_ms), local


def run_all(db_path: str, prices_path: str, *, train_frac: float, n_folds: int,
            bootstrap_reps: int, seed: int) -> dict:
    ext_rows = load_ext_trajectories(db_path)
    if not ext_rows:
        raise SystemExit("no requests in that store")
    prices = PriceBook.from_operator_file(prices_path)
    semantics = Semantics()
    schedule = PingSchedule()

    lo, hi = ext_rows[0].ts_ms, max(r.ts_ms for r in ext_rows)
    train_cut = lo + int((hi - lo) * train_frac)
    remaining = hi - train_cut
    fold_edges = [train_cut + int(remaining * i / n_folds) for i in range(n_folds + 1)]

    train_rows = [r for r in ext_rows if r.ts_ms <= train_cut]
    fold_rows = []
    for i in range(n_folds):
        lo_e, hi_e = fold_edges[i], fold_edges[i + 1]
        fold_rows.append([r for r in ext_rows if lo_e < r.ts_ms <= hi_e])

    tenants = sorted({r.user for r in ext_rows})
    tenant_pseudo = pseudonymize_map(tenants)

    print(f"window {lo}..{hi} ms ({len(ext_rows):,} rows); train<= {train_cut} "
          f"({len(train_rows):,} rows); {n_folds} test folds "
          f"({', '.join(str(len(f)) for f in fold_rows)} rows)", file=sys.stderr)

    # ---- rule arm 1: stop-reason-gated ping (no fitting) --------------------
    # ---- rule arm 2: historical-probability, per-tenant tuned ---------------
    train_sorted = sorted(train_rows, key=lambda r: (r.ts_ms, r.request_id))
    per_tenant_thr, default_thr, skipped_tenants = tune_historical_probability(
        train_sorted, tenants, prices=prices, semantics=semantics, window_end_ms=train_cut)
    print(f"historical-probability: tuned {len(per_tenant_thr)} tenants, default={default_thr}, "
          f"skipped (insufficient signal): {[tenant_pseudo[t] for t in skipped_tenants]}",
          file=sys.stderr)

    all_test_rows = [r for f in fold_rows for r in f]
    all_test_ids = {r.request_id for r in all_test_rows}
    full_sorted = sorted(ext_rows, key=lambda r: (r.ts_ms, r.request_id))
    hp_actions_all, _ = replay_history_actions(
        full_sorted, all_test_ids, per_tenant_thresholds=per_tenant_thr,
        default_threshold=default_thr, min_prefix=MIN_PREFIX, prices=prices,
        semantics=semantics, max_pings=MAX_PINGS)

    # ---- rule arm 3: stop-reason x hour-of-day -------------------------------
    good_hours = set(tune_good_hours(train_rows))
    print(f"stop-reason x hour: good hours (UTC) = {sorted(good_hours)}", file=sys.stderr)

    # ---- learned arm: logistic regression -----------------------------------
    feat_df = build_feature_frame(ext_rows, window_end_ms=hi)
    train_feat = feat_df[feat_df["ts_ms"] <= train_cut]
    pipe, n_fit, n_events = fit_logreg(train_feat)
    train_pred = pipe.predict_proba(
        train_feat[list(NUMERIC_FEATURES) + list(CATEGORICAL_FEATURES)])[:, 1]
    train_feat = train_feat.assign(p_band=train_pred)

    def logreg_actions_for(rows: list[ExtRequest], threshold: float) -> dict[int, str]:
        ids = {r.request_id for r in rows}
        sub = feat_df[feat_df["request_id"].isin(ids)]
        proba = pipe.predict_proba(sub[list(NUMERIC_FEATURES) + list(CATEGORICAL_FEATURES)])[:, 1]
        p_by_id = dict(zip(sub["request_id"], proba))
        cx_by_id = {r.request_id: r.cached_context for r in rows}
        return {rid: gate_action(cx_by_id[rid], p_by_id.get(rid, 0.0) >= threshold) for rid in ids}

    best_thr, best_cost = 0.5, float("inf")
    for thr in (0.03, 0.05, 0.08, 0.12, 0.2, 0.35, 0.5, 0.7):
        acts = logreg_actions_for(train_rows, thr)
        c = sum(per_conversation_costs(train_rows, acts, prices, semantics, schedule,
                                       train_cut).values())
        if c < best_cost:
            best_thr, best_cost = thr, c
    print(f"logreg: fit on {n_fit:,} labelled rows ({n_events:,} band events), "
          f"tuned threshold={best_thr}", file=sys.stderr)

    coefs_json = flatten_logreg(pipe, tenant_pseudo)
    coefs_json["training_summary"] = {
        "rows_fit": n_fit, "band_events": n_events, "tuned_threshold": best_thr,
    }

    gbm_importance = gbm_feature_importance(train_feat, tenant_pseudo)
    lr_std_coefs = _logreg_standardized_importance(pipe, tenant_pseudo)

    # ---- per-fold scoring for every arm --------------------------------------
    arms = {
        "stop-reason-gated": lambda rows: stop_reason_gate_actions(rows),
        "historical-probability-tenant-tuned": lambda rows: {
            r.request_id: hp_actions_all[r.request_id] for r in rows},
        "stop-reason-x-hour": lambda rows: stop_reason_hour_actions(rows, good_hours),
        "logreg-v1": lambda rows: logreg_actions_for(rows, best_thr),
    }

    fold_results: list[dict] = []
    pooled_conv_costs: dict[str, dict[tuple, float]] = {name: {} for name in arms}
    pooled_baseline_conv_costs: dict[tuple, float] = {}

    for fi, rows in enumerate(fold_rows):
        if not rows:
            continue
        window_end = fold_edges[fi + 1]
        baseline_cost, baseline_local = fold_cost(rows, fixed_5m_actions(rows), prices,
                                                   semantics, schedule, window_end)
        baseline_conv = per_conversation_costs(baseline_local,
                                               fixed_5m_actions(baseline_local), prices,
                                               semantics, schedule, window_end)
        for k, v in baseline_conv.items():
            pooled_baseline_conv_costs[(fi, *k)] = v

        fold_entry = {"fold": fi, "n_rows": len(rows),
                      "since_ms": fold_edges[fi], "until_ms": window_end,
                      "baseline_usd": baseline_cost.total_usd, "arms": {}}
        for name, fn in arms.items():
            acts = fn(rows)
            arm_cost, local = fold_cost(rows, acts, prices, semantics, schedule, window_end)
            local_acts = {r.request_id: acts.get(r.request_id, EXPIRE) for r in local}
            arm_conv = per_conversation_costs(local, local_acts, prices, semantics, schedule,
                                              window_end)
            for k, v in arm_conv.items():
                pooled_conv_costs[name][(fi, *k)] = v
            savings = compare(baseline_cost, arm_cost)
            fold_entry["arms"][name] = {
                "total_usd": arm_cost.total_usd, "absolute_usd": savings.absolute_usd,
                "percent_usd": savings.percent_usd, "hit_rate_pct": arm_cost.hit_rate_pct,
                "pings": arm_cost.pings, "writes_5m": arm_cost.writes_5m,
                "writes_1h": arm_cost.writes_1h,
            }
        fold_results.append(fold_entry)

    # ---- pooled + bootstrap ---------------------------------------------------
    pooled = {}
    for name in arms:
        pooled[name] = bootstrap_ci(pooled_baseline_conv_costs, pooled_conv_costs[name],
                                     reps=bootstrap_reps, seed=seed)

    # ---- per-tenant breakdown (pseudonymized) ---------------------------------
    n_actually_done_test: dict[str, int] = {}
    for r in all_test_rows:
        if stop_cluster(r.stop_reason) == "actually_done":
            n_actually_done_test[r.user] = n_actually_done_test.get(r.user, 0) + 1

    per_tenant_report: dict[str, dict] = {}
    for t in tenants:
        n_ad = n_actually_done_test.get(t, 0)
        pseudo = tenant_pseudo[t]
        if n_ad < 30:
            per_tenant_report[pseudo] = {"n_actually_done_test": n_ad, "skipped": True,
                                         "reason": "fewer than 30 actually-done events in the "
                                                    "test window; not enough signal to report"}
            continue
        base_t = sum(v for k, v in pooled_baseline_conv_costs.items() if k[1] == t)
        entry = {"n_actually_done_test": n_ad, "skipped": False, "baseline_usd": base_t,
                 "arms": {}}
        for name in arms:
            arm_t = sum(v for k, v in pooled_conv_costs[name].items() if k[1] == t)
            pct = 100 * (base_t - arm_t) / base_t if base_t else None
            entry["arms"][name] = {"usd": arm_t, "percent_savings": pct}
        per_tenant_report[pseudo] = entry

    return {
        "window": {"since_ms": lo, "until_ms": hi, "train_cut_ms": train_cut,
                   "fold_edges_ms": fold_edges},
        "n_requests": len(ext_rows), "n_tenants": len(tenants),
        "historical_probability": {
            "default_threshold": default_thr,
            "n_tenants_tuned": len(per_tenant_thr),
            "tenants_skipped": [tenant_pseudo[t] for t in skipped_tenants],
            "per_tenant_thresholds": {tenant_pseudo[t]: list(v) for t, v in per_tenant_thr.items()},
        },
        "stop_reason_x_hour": {"good_hours_utc": sorted(good_hours)},
        "logreg": {"tuned_threshold": best_thr, "rows_fit": n_fit, "band_events": n_events},
        "fold_results": fold_results,
        "pooled": pooled,
        "per_tenant": per_tenant_report,
        "feature_importance": {
            "logreg_standardized_coefs": lr_std_coefs,
            "gbm_importances": gbm_importance,
        },
    }, coefs_json


def _logreg_standardized_importance(pipe, tenant_pseudo: dict[str, str]) -> list[dict]:
    """Standardized-space coefficients, ranked by magnitude. Category names for `user_id`
    are pseudonymized here, the same way as flatten_logreg and gbm_feature_importance — a
    real tenant_id must never reach a feature name that leaves this process."""
    pre = pipe.named_steps["pre"]
    clf = pipe.named_steps["clf"]
    coefs = clf.coef_.ravel()
    names = list(NUMERIC_FEATURES)
    onehot = pre.named_transformers_["cat"].named_steps["onehot"]
    for feat_name, categories in zip(CATEGORICAL_FEATURES, onehot.categories_):
        for cat in categories:
            label = tenant_pseudo.get(cat, cat) if feat_name == "user_id" else cat
            names.append(f"{feat_name}={label}")
    ranked = sorted(zip(names, coefs), key=lambda kv: -abs(kv[1]))
    return [{"feature": n, "standardized_coef": float(c)} for n, c in ranked]


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--db", default=DEFAULT_DB)
    ap.add_argument("--prices", default=DEFAULT_PRICES)
    ap.add_argument("--out", required=True)
    ap.add_argument("--coefs-out", required=True)
    ap.add_argument("--train-frac", type=float, default=0.6)
    ap.add_argument("--folds", type=int, default=3)
    ap.add_argument("--bootstrap", type=int, default=400)
    ap.add_argument("--seed", type=int, default=0)
    args = ap.parse_args()

    result, coefs = run_all(args.db, args.prices, train_frac=args.train_frac,
                            n_folds=args.folds, bootstrap_reps=args.bootstrap, seed=args.seed)

    fd = os.open(args.out, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o644)
    with os.fdopen(fd, "w", encoding="utf-8") as fh:
        json.dump(result, fh, indent=2, default=str)
    os.chmod(args.out, 0o644)  # the umask under `cg` strips the world-read bit the mode= asked for
    fd2 = os.open(args.coefs_out, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o644)
    with os.fdopen(fd2, "w", encoding="utf-8") as fh:
        json.dump(coefs, fh, indent=2, default=str)
    os.chmod(args.coefs_out, 0o644)
    print(f"wrote {args.out} and {args.coefs_out}", file=sys.stderr)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
