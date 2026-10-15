"""LightGBM discrete-time hazard + the economic ping rule + a JSON tree export the Go runtime evaluates.
Person-period rows: one row per (request, 20 time bins up to its return or censoring); label 1 in the bin the return falls in; the bin and its right edge are features."""
import json, numpy as np, pandas as pd

EDGES = np.array([0, 1, 3, 10, 30, 100, 200, 280, 295, 420, 560, 575, 700, 840, 855, 1000, 1120, 1135, 1300, 1415, 1500.0])
H, K, X0, TC, LAM = 1500.0, 4, 280.0, 295.0, 1.0
HP = dict(leaves=127, lr=0.05, rounds=300, mcl=100)

def labels(tv):
    """(y, event): time to the return, capped at H (later = censored at H); the fold's censoring time otherwise."""
    ev = tv.compat.values == 1; y = np.where(ev, tv.dt_next_s.values, tv.cens_time_s.values).astype(float)
    y = np.nan_to_num(y, nan=0.0); late = y > H; ev = ev & ~late; return np.maximum(np.minimum(y, H), 0.05), ev

def fit(X, y, ev, hp=None, nmax=200_000, threads=2, seed=0):
    import lightgbm as lgb
    hp = {**HP, **(hp or {})}; rng = np.random.default_rng(seed)
    if len(X) > nmax: s = np.sort(rng.choice(len(X), nmax, replace=False)); X, y, ev = X.iloc[s], y[s], ev[s]
    b = np.clip(np.searchsorted(EDGES, y, side="left") - 1, 0, len(EDGES) - 2)
    last = np.where(ev, b, np.searchsorted(EDGES, y, side="right") - 2); cnt = last + 1; cnt = np.where(ev, cnt, np.maximum(cnt, 0))
    idx = np.repeat(np.arange(len(X)), cnt); pos = np.arange(cnt.sum()) - np.repeat(np.cumsum(cnt) - cnt, cnt); lab = (ev[idx] & (pos == b[idx])).astype(np.float32)
    XX = X.iloc[idx].reset_index(drop=True); XX["bin"] = pos.astype("float32"); XX["bin_edge"] = EDGES[pos].astype("float32")
    return lgb.LGBMClassifier(objective="binary", n_estimators=hp["rounds"], learning_rate=hp["lr"], num_leaves=hp["leaves"], min_child_samples=hp["mcl"], subsample=0.8, subsample_freq=1,
                              colsample_bytree=0.6, n_jobs=threads, verbose=-1, max_bin=127, random_state=seed).fit(XX, lab)

def hazard(model, X):
    n, B = len(X), len(EDGES) - 1; XX = X.iloc[np.repeat(np.arange(n), B)].reset_index(drop=True); bb = np.tile(np.arange(B), n)
    XX["bin"] = bb.astype("float32"); XX["bin_edge"] = EDGES[bb].astype("float32"); return model.predict_proba(XX)[:, 1].reshape(n, B)

def surv_edges(h): return np.concatenate([np.ones((len(h), 1)), np.cumprod(1 - h, 1)], 1)       # S at EDGES

def decide(S, stake, ping, lam=LAM, kmax=K):
    """S(t) is a function t -> (n,) survival. k = argmax over 0..kmax of  sum_{j<=k} stake*(S(280(j-1)+295) - S(280j+295)) - lam*ping*S(280j)  (running maximum, ties keep fewer pings)."""
    net = np.zeros(len(stake)); best = np.zeros(len(stake)); k = np.zeros(len(stake), int)
    for j in range(1, kmax + 1):
        net = net + stake * (S(X0 * (j - 1) + TC) - S(X0 * j + TC)) - lam * ping * S(X0 * j)
        up = net > best + 1e-12; k = np.where(up, j, k); best = np.where(up, net, best)
    return k

def edge_surv(Se):
    def f(t):
        i = int(np.searchsorted(EDGES, t)); assert abs(EDGES[i] - t) < 1e-9, t
        return Se[:, i]
    return f

# ---------------------------------------------------------------- export
def export(model, feature_names, enc, windows, path, meta):
    """trees as flat node arrays: f (feature index or -1 for a leaf), t threshold, l/r children, d default-left, m missing type (0 none, 1 zero, 2 nan), v leaf value (raw score)."""
    d = model.booster_.dump_model(); names = d["feature_names"]; assert names == list(feature_names) + ["bin", "bin_edge"], names
    trees = []
    for ti in d["tree_info"]:
        f, t, l, r, dl, mt, v = [], [], [], [], [], [], []
        def walk(n):
            i = len(f); f.append(-1); t.append(0.0); l.append(-1); r.append(-1); dl.append(0); mt.append(0); v.append(0.0)
            if "leaf_value" in n: v[i] = n["leaf_value"]; return i
            assert n["decision_type"] == "<=", n["decision_type"]
            f[i] = n["split_feature"]; t[i] = n["threshold"]; dl[i] = int(bool(n["default_left"])); mt[i] = {"None": 0, "Zero": 1, "NaN": 2}[n["missing_type"]]
            l[i] = walk(n["left_child"]); r[i] = walk(n["right_child"]); return i
        walk(ti["tree_structure"]); trees.append(dict(f=f, t=t, l=l, r=r, d=dl, m=mt, v=v))
    doc = dict(schema="cg.cache.predictor/1", kind="lgb-discrete-hazard", names=list(feature_names), categories=enc, windows=windows, edges=EDGES.tolist(),
               rule=dict(lam=LAM, x_s=X0, kmax=K, tc_s=TC), trees=trees, meta=meta)
    json.dump(doc, open(path, "w"), separators=(",", ":"))

def ref_hazard(doc, X):
    """pure-python evaluation of the exported trees (the reference the Go runtime is tested against); X: (n, 72) float array, categoricals already coded. Returns (n, 20) hazards."""
    n, B = len(X), len(doc["edges"]) - 1; out = np.zeros((n, B)); edges = np.array(doc["edges"])
    for i in range(n):
        for b in range(B):
            x = np.append(X[i], [b, edges[b]]); s = 0.0
            for tr in doc["trees"]:
                k = 0
                while tr["f"][k] >= 0:
                    xv = x[tr["f"][k]]; m = tr["m"][k]
                    if np.isnan(xv) and m != 2: xv = 0.0
                    if (m == 2 and np.isnan(xv)) or (m == 1 and abs(xv) <= 1e-35): k = tr["l"][k] if tr["d"][k] else tr["r"][k]
                    else: k = tr["l"][k] if xv <= tr["t"][k] else tr["r"][k]
                s += tr["v"][k]
            out[i, b] = 1 / (1 + np.exp(-s))
    return out
