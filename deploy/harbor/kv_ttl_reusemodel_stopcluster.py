#!/usr/bin/env python3
"""Issue #326: does `stop_cluster` add anything to ReuseModelV1's hazard model?

Scope, stated up front because it is narrower than a full re-ship of the model: this
answers the FEATURE-VALUE question (does knowing the stop_reason of the request that
opened an idle span improve the model's hazard/survival AUC and Brier score) on a held-out
split, using the SAME person-period design kv_ttl_keepalive_policy.person_periods uses. It
does NOT re-run the backward induction that turns a survival curve into a ping BUDGET
(kv_ttl_keepalive_policy.windows/budget_for) or re-derive a net-dollar figure for a
stop_cluster-aware policy -- that needs the induction wired to a feature the model
containing `kvcache.Observation` does not carry (stop_reason is not in Observation, so a
model that used it structurally could not be the SHIPPED model regardless of what this
finds; see kv_ttl_keepalive_policy.py's own FEATURES docstring, which measured the cost of
staying inside Observation's 13 fields as 0.04pp). This script is the honest, bounded
answer to "would it be worth widening that seam" -- not a shipped-model replacement.

Why a near-duplicate of `load_spans`'s loop rather than importing it: `load_spans` reads
`kv_ttl_cost_model.load_trajectories`, which does not select `stop_reason`. This script
reads `kv_ttl_predictor_arms.load_ext_trajectories` instead (same filter, same row set,
verified below) and carries one extra field through the same walk. Everything else --
`_History`, `_bucket_at`, `_FULL_MISS`, `span_from_raw`, `person_periods`'s target
definition -- is imported and reused, not reimplemented.
"""
from __future__ import annotations
import argparse, json, os, sys, warnings
warnings.filterwarnings("ignore")
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import numpy as np
import pandas as pd

from kv_ttl_cost_model import PriceBook, DEFAULT_DB, DEFAULT_PRICES  # noqa: E402
from kv_ttl_predictor_arms import load_ext_trajectories, stop_cluster  # noqa: E402
from kv_ttl_keepalive_policy import (  # noqa: E402
    _History, _bucket_at, _FULL_MISS, span_from_raw,
    SPAN_CAT, SPAN_NUM, STATE_NUM, STATS_NUM,
    DEFAULT_INTERVAL_S, DEFAULT_LIFE_S, DEFAULT_MAX_K,
)

FEATURES_BASE = list(SPAN_CAT) + list(SPAN_NUM) + list(STATE_NUM) + list(STATS_NUM)
FEATURES_EXT = list(SPAN_CAT) + ["stop_cluster"] + list(SPAN_NUM) + list(STATE_NUM) + list(STATS_NUM)


def load_spans_with_stop_cluster(db_path: str, prices_path: str, *, horizon_s: float = 300.0
                                 ) -> pd.DataFrame:
    """Same walk as kv_ttl_keepalive_policy.load_spans, plus `stop_cluster` of the request
    that OPENED the span (the predecessor, not the successor -- the decision instant is at
    the predecessor's timestamp, so only its own stop_reason was knowable then)."""
    rows = load_ext_trajectories(db_path)
    book = PriceBook.from_operator_file(prices_path)

    by_conv: dict[tuple, list] = {}
    for r in rows:
        by_conv.setdefault(r.key, []).append(r)
    for g in by_conv.values():
        g.sort(key=lambda r: (r.ts_ms, r.request_id))

    hist = _History()
    prev_ts: dict[tuple, int] = {}
    prev_gap: dict[tuple, float] = {}
    turn: dict[tuple, int] = {}
    nxt: dict[int, object] = {}
    for g in by_conv.values():
        for a, b in zip(g, g[1:]):
            nxt[a.request_id] = b

    out = []
    for r in sorted(rows, key=lambda r: (r.ts_ms, r.request_id)):
        key = r.key
        if key in prev_ts:
            gap_s = max(0.0, (r.ts_ms - prev_ts[key]) / 1000.0)
            hist.observe(r.user, r.model, _bucket_at(prev_ts[key]), gap_s)
            prev_gap[key] = gap_s
        turn[key] = turn.get(key, 0) + 1
        stat_p, stat_n = hist.reuse_within(r.user, r.model, _bucket_at(r.ts_ms), horizon_s)
        prefix = int(r.cached_context or 0)
        price = book.for_model(r.model)
        succ = nxt.get(r.request_id)
        if succ is None or (succ.miss_reason or "") in _FULL_MISS:
            gap = float("inf")
        else:
            gap = max(0.0, (succ.ts_ms - r.ts_ms) / 1000.0)
        rescue_rate = (price.write_5m - price.cache_read) if price.known else 0.0
        row = span_from_raw(ts_ms=r.ts_ms, cached_tokens=prefix,
                            since_last_ms=int(1000.0 * prev_gap.get(key, 0.0)),
                            turn=turn[key], stat_p=stat_p, stat_n=stat_n, model=r.model)
        row.update({"gap_s": gap, "stake": prefix * rescue_rate, "user_id": r.user,
                    "cache_ttl": (r.ttl_recorded or ""), "prefix": prefix,
                    "request_id": r.request_id,
                    "stop_cluster": stop_cluster(r.stop_reason)})  # the one addition
        out.append(row)
        prev_ts[key] = r.ts_ms
    return pd.DataFrame(out)


def _state_block_ext(reached: pd.DataFrame, k: int, interval_s: float, life_s: float,
                     features: list[str]) -> pd.DataFrame:
    """Vectorized counterpart of kv_ttl_keepalive_policy._state_frame: same arithmetic, one
    whole sweep's slice at a time instead of a per-row DataFrame(dict) + concat, which is
    the dominant cost at max_k=8 on a quarter-million-span corpus (that per-row loop is the
    shipped model's own known cost profile too -- not reused here only because this script
    is not bound to the Go-port bit-parity constraint that keeps the shipped code doing it
    the slow way)."""
    carried = [c for c in features if c not in
              ("sweep_k", "log_elapsed", "log_secs_to_deadline", "hour_utc", "dow_utc")]
    f = reached[carried].reset_index(drop=True).copy()
    t_k = k * interval_s
    deadline = life_s if k == 1 else (k - 1) * interval_s + life_s
    f["sweep_k"] = k
    f["log_elapsed"] = np.log1p(t_k)
    f["log_secs_to_deadline"] = np.log1p(max(deadline - t_k, 0.0))
    secs = reached["ts_ms"].to_numpy() / 1000.0 + t_k
    f["hour_utc"] = (secs // 3600 % 24).astype(int)
    f["dow_utc"] = ((secs // 86400 + 4) % 7).astype(int)
    return f[features]


def person_periods_ext(spans: pd.DataFrame, features: list[str], *, interval_s: float,
                       life_s: float, max_k: int) -> pd.DataFrame:
    """Same target definition as kv_ttl_keepalive_policy.person_periods, parameterized by an
    explicit feature list instead of the module-global FEATURES -- the minimal change needed
    to add one column without mutating shared state another importer in this same process
    might rely on."""
    out = []
    for k in range(1, max_k + 1):
        t_k = k * interval_s
        reached = spans[spans.gap_s > t_k]
        if reached.empty:
            break
        rows = _state_block_ext(reached, k, interval_s, life_s, features)
        deadline = life_s if k == 1 else (k - 1) * interval_s + life_s
        g = reached.gap_s.to_numpy()
        rows["y_hazard"] = ((g > deadline) & (g <= t_k + life_s)).astype(int)
        rows["stake"] = reached.stake.to_numpy()
        out.append(rows)
    return pd.concat(out, ignore_index=True)


def fit_and_score(pp_train: pd.DataFrame, pp_test: pd.DataFrame, features: list[str], cat: list[str]):
    from sklearn.compose import ColumnTransformer
    from sklearn.ensemble import HistGradientBoostingClassifier
    from sklearn.pipeline import Pipeline
    from sklearn.preprocessing import OneHotEncoder
    from sklearn.metrics import roc_auc_score, brier_score_loss

    w = pp_train.stake.clip(lower=0)
    weights = (1e-9 + w / w.mean()).to_numpy()
    num = [c for c in features if c not in cat]
    pipe = Pipeline([
        ("prep", ColumnTransformer([
            ("cat", OneHotEncoder(handle_unknown="ignore", min_frequency=20, sparse_output=False), cat),
            ("num", "passthrough", num)])),
        ("gbm", HistGradientBoostingClassifier(max_iter=200, learning_rate=0.08, random_state=0)),
    ])
    pipe.fit(pp_train[features], pp_train.y_hazard, gbm__sample_weight=weights)
    p_test = pipe.predict_proba(pp_test[features])[:, 1]
    y_test = pp_test.y_hazard.to_numpy()
    out = {"n_train": int(len(pp_train)), "n_test": int(len(pp_test)),
          "base_rate_test": float(y_test.mean())}
    if 0 < y_test.mean() < 1:
        out["auc"] = float(roc_auc_score(y_test, p_test))
        # stake-weighted AUC too: this corpus's own budget-model finding is that an
        # unweighted AUC can look much better than it performs where the money actually is
        # (kv-ttl-keepalive-generalization-2026-09.md, "Not reached"), so both are reported.
        wt = pp_test.stake.clip(lower=0).to_numpy()
        if wt.sum() > 0:
            order = np.argsort(-p_test)
            # weighted AUC via the rank-sum identity over stake-weighted pos/neg pairs would
            # need O(n^2) without a library; report weighted average precision instead as
            # the money-aware ranking diagnostic actually available cheaply here.
            from sklearn.metrics import average_precision_score
            out["stake_weighted_average_precision"] = float(
                average_precision_score(y_test, p_test, sample_weight=wt))
    out["brier"] = float(brier_score_loss(y_test, p_test))
    return out


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--db", default=DEFAULT_DB)
    ap.add_argument("--prices", default=DEFAULT_PRICES)
    ap.add_argument("--split", type=float, default=0.6)
    ap.add_argument("--interval", type=float, default=DEFAULT_INTERVAL_S)
    ap.add_argument("--life", type=float, default=DEFAULT_LIFE_S)
    ap.add_argument("--max-k", type=int, default=DEFAULT_MAX_K)
    ap.add_argument("--out", default="reusemodel_stopcluster_result.json")
    args = ap.parse_args()

    spans = load_spans_with_stop_cluster(args.db, args.prices)
    if spans.empty:
        raise SystemExit("no spans")
    cut = spans.ts_ms.quantile(args.split)
    train, test = spans[spans.ts_ms <= cut], spans[spans.ts_ms > cut]
    print(f"{len(spans):,} spans; train {len(train):,} / test {len(test):,}", file=sys.stderr)

    pp_train_base = person_periods_ext(train, FEATURES_BASE, interval_s=args.interval,
                                       life_s=args.life, max_k=args.max_k)
    pp_test_base = person_periods_ext(test, FEATURES_BASE, interval_s=args.interval,
                                      life_s=args.life, max_k=args.max_k)
    pp_train_ext = person_periods_ext(train, FEATURES_EXT, interval_s=args.interval,
                                      life_s=args.life, max_k=args.max_k)
    pp_test_ext = person_periods_ext(test, FEATURES_EXT, interval_s=args.interval,
                                     life_s=args.life, max_k=args.max_k)

    base = fit_and_score(pp_train_base, pp_test_base, FEATURES_BASE, list(SPAN_CAT))
    ext = fit_and_score(pp_train_ext, pp_test_ext, FEATURES_EXT, list(SPAN_CAT) + ["stop_cluster"])

    stop_cluster_counts = spans["stop_cluster"].value_counts().to_dict()
    result = {
        "config": {"db": args.db, "prices": args.prices, "split": args.split,
                   "interval_s": args.interval, "life_s": args.life, "max_k": args.max_k,
                   "note": "feature-value question only; see module docstring for what this "
                           "does NOT re-derive (the $ budget policy)."},
        "n_spans_total": int(len(spans)),
        "stop_cluster_distribution": {str(k): int(v) for k, v in stop_cluster_counts.items()},
        "reference_13_feature": base,
        "plus_stop_cluster_14_feature": ext,
        "auc_delta": (ext.get("auc") - base.get("auc")) if ("auc" in ext and "auc" in base) else None,
        "brier_delta": ext["brier"] - base["brier"],
    }
    with open(args.out, "w") as fh:
        json.dump(result, fh, indent=2)
    print(json.dumps(result, indent=2))
    return 0


if __name__ == "__main__":
    sys.exit(main())
