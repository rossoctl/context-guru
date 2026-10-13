"""Predictive keep-alive: causal features, Weibull-mixture MLP, the economic ping rule, closed-form replay, JSON export.
The numpy forward here is the reference the Go runtime (predictive/) is tested against."""
import json, numpy as np, pandas as pd

KMAX, TC, X_S, LAM = 6, 295.0, 280.0, 1.0
H1, H2, NO = 48, 64, 10
NAMES = (["l_prefix", "l_out", "l_fresh", "cw_frac", "l_msgs", "l_tools", "l_turn", "l_sturn", "l_dur", "l_sage", "pg_first", "pg_pos", "pg_neg", "pg2",
          "hs", "hc", "ds", "dc", "kv_p5", "kv_p1", "kv_ln", "gq_nan", "l_gq", "think", "stream", "l_ping"]
         + ["sr_" + s for s in ("tool_use", "stop_sequence", "end_turn", "stop", "tool_calls")] + ["fam_" + s for s in ("opus", "sonnet", "haiku")]
         + ["ag_" + s for s in ("claude-cli", "openai", "asyncopenai")])
L = lambda v: np.log1p(np.maximum(np.asarray(v, float), 0))

def bucket(h): return np.where(h >= 18, 3, np.where(h >= 12, 2, np.where(h >= 6, 1, 0)))

def _causal_stats(dp, horizons=(300.0, 3600.0), minc=6):
    """kvcache.History.ReuseWithin per row: share of already-closed same-cell gaps <= h (cell chain user+model+bucket -> user+model -> user -> model -> global; first cell with >=6 gaps)."""
    ev = dp[dp.compat == 1]
    gap = (ev.t_end_ms.values + ev.dt_next_s.values * 1000 - ev.ts_ms.values) / 1000.0; obs = ev.ts_ms.values.astype(float) + gap * 1000.0
    bk = bucket(((ev.ts_ms.values // 3600000) % 24).astype(int)); mdl = ev.model.str.lower().values; usr = ev.tenant.values
    qb = bucket(((dp.ts_ms.values // 3600000) % 24).astype(int)); qm = dp.model.str.lower().values; qu = dp.tenant.values; tq = dp.t_dec_ms.values.astype(float)
    ekeys = dict(ubm=(usr, mdl, bk), um=(usr, mdl), u=(usr,), m=(mdl,), g=()); qkeys = dict(ubm=(qu, qm, qb), um=(qu, qm), u=(qu,), m=(qm,), g=())
    cells = {}
    for k in ekeys:
        N = np.zeros(len(dp)); Hh = {h: np.zeros(len(dp)) for h in horizons}
        e = pd.DataFrame({"k": [hash(t) for t in zip(*ekeys[k])] if ekeys[k] else np.zeros(len(ev), int), "o": obs, **{f"h{int(h)}": (gap <= h).astype(float) for h in horizons}})
        qk = np.array([hash(t) for t in zip(*qkeys[k])]) if qkeys[k] else np.zeros(len(dp), int)
        for kv, g in e.groupby("k"):
            qi = np.where(qk == kv)[0]
            if len(qi) == 0: continue
            g = g.sort_values("o"); pos = np.searchsorted(g.o.values, tq[qi], side="right"); N[qi] = pos
            for h in horizons: Hh[h][qi] = np.r_[0, np.cumsum(g[f"h{int(h)}"].values)][pos]
        cells[k] = (N, Hh)
    chosen = np.full(len(dp), 4)
    for i in range(3, -1, -1):
        chosen[cells[list(ekeys)[i]][0] >= minc] = i
    N = np.zeros(len(dp)); P = {h: np.zeros(len(dp)) for h in horizons}
    for i, k in enumerate(ekeys):
        s = chosen == i; N[s] = cells[k][0][s]
        for h in horizons: P[h][s] = cells[k][1][h][s]
    return {h: (np.where(N > 0, P[h] / np.maximum(N, 1), 0.0), N) for h in horizons}

def _key_gap_q(dp, q=0.8, nwin=50, minn=10):
    ev = dp[dp.compat == 1]; gap = (ev.t_end_ms.values + ev.dt_next_s.values * 1000 - ev.ts_ms.values) / 1000.0; obs = ev.ts_ms.values + gap * 1000
    out = np.full(len(dp), np.nan)
    for u in dp.tenant.unique():
        m = ev.tenant.values == u; o, g = obs[m], gap[m]; od = np.argsort(o, kind="stable"); o, g = o[od], g[od]
        qi = np.where(dp.tenant.values == u)[0]; pos = np.searchsorted(o, dp.t_dec_ms.values[qi], side="right")
        rq = pd.Series(g).rolling(nwin, min_periods=minn).quantile(q).values
        out[qi] = np.where(pos >= minn, rq[np.maximum(pos - 1, 0)] if len(rq) else np.nan, np.nan)
    return out

def raw_features(dp):
    """(N, 38) UNSTANDARDISED causal inputs, in dp row order. Everything is knowable when the response has ended."""
    dp = dp.reset_index(drop=True); st = _causal_stats(dp); p5, n5 = st[300.0]; p1, _ = st[3600.0]; gq = _key_gap_q(dp)
    key = dp.tenant.astype(str) + "|" + dp.session.astype(str) + "|" + dp.thread_id.astype(str)
    o = np.lexsort((dp.dp_id.values, dp.ts_ms.values, key.values)); inv = np.empty(len(dp), int); inv[o] = np.arange(len(dp))
    s = dp.iloc[o].reset_index(drop=True); k = key.values[o]
    prev_end = s.groupby(k).t_end_ms.shift(1); pg = (s.ts_ms - prev_end) / 1000.0; first = prev_end.isna().values; pg2 = pg.groupby(k).shift(1)
    sstart = s.groupby([s.tenant, s.session]).ts_ms.transform("min")
    c = {"l_prefix": L(s.prefix_tok), "l_out": L(s.output_tok), "l_fresh": L(s.fresh_input), "cw_frac": s.cache_write / np.maximum(s.prefix_tok, 1),
         "l_msgs": L(s.messages), "l_tools": L(s.tools), "l_turn": L(s.turn_in_thread), "l_sturn": L(s.session_turn),
         "l_dur": L((s.t_end_ms - s.ts_ms) / 1000.0), "l_sage": L((s.ts_ms - sstart) / 1000.0),
         "pg_first": first.astype(float), "pg_pos": np.where(first, 0, L(pg.fillna(0))), "pg_neg": np.where(first, 0, (pg.fillna(0) < 0).astype(float)),
         "pg2": np.where(pg2.isna(), 0, L(pg2.fillna(0))),
         "hs": np.sin(2 * np.pi * s.hour_utc / 24), "hc": np.cos(2 * np.pi * s.hour_utc / 24), "ds": np.sin(2 * np.pi * s.dow / 7), "dc": np.cos(2 * np.pi * s.dow / 7)}
    c.update(kv_p5=p5[o], kv_p1=p1[o], kv_ln=L(n5[o]), gq_nan=np.isnan(gq[o]).astype(float), l_gq=np.where(np.isnan(gq[o]), 0, L(gq[o])),
             think=s.thinking.isin(["adaptive", "enabled"]).astype(float), stream=s.stream.astype(float), l_ping=L(s.ping_cost * 1000))
    for r in ("tool_use", "stop_sequence", "end_turn", "stop", "tool_calls"): c["sr_" + r] = (s.stop_reason == r).astype(float).values
    for r in ("opus", "sonnet", "haiku"): c["fam_" + r] = (s.family == r).astype(float).values
    for r in ("claude-cli", "openai", "asyncopenai"): c["ag_" + r] = (s.agent_type == r).astype(float).values
    X = np.stack([np.asarray(c[n], float) for n in NAMES], 1)
    return X[inv]

# ---------------------------------------------------------------- model
class Net:
    """x -> tanh(48) -> tanh(64) -> 10 Weibull-mixture parameters; float64 numpy, weights are float32-exact."""
    def __init__(s, p): s.W1, s.b1, s.W2, s.b2, s.W3, s.b3 = (np.asarray(p[k], float) for k in ("W1", "b1", "W2", "b2", "W3", "b3"))
    def __call__(s, x): return np.tanh(np.tanh(x @ s.W1 + s.b1) @ s.W2 + s.b2) @ s.W3 + s.b3

def surv(o, t):
    """S(t) of the censored 3-Weibull mixture with a never-returns atom. o = [4 logits | 3 log-scales | 3 raw shapes]."""
    lg = o[:, :4]; pi = np.exp(lg - lg.max(1, keepdims=True)); pi /= pi.sum(1, keepdims=True); la = o[:, 4:7]; b = 0.3 + 3.7 / (1 + np.exp(-o[:, 7:10]))
    u = np.exp(np.clip(b * (np.log(max(float(t), 0.05)) - la), -50, 50))
    return pi[:, 0] + (pi[:, 1:] * np.exp(-u)).sum(1)

def mean_surv(outs, t): return np.mean([surv(o, t) for o in outs], 0)

def decide(outs, prefix_tok, p_write5m, p_read, ping_cost, lam=LAM, x=X_S, kmax=KMAX):
    """k* = argmax_k lam*stake*(S(295) - S(k x + 295)) - ping_cost * sum_{j<=k} S(j x),  stake = prefix*(p_write5m - p_read).  S = mean over seeds."""
    n = len(ping_cost); S295 = mean_surv(outs, TC); stake = prefix_tok * (p_write5m - p_read); B = np.zeros((n, kmax + 1)); c = np.zeros(n)
    for k in range(1, kmax + 1):
        c = c + mean_surv(outs, k * x); B[:, k] = lam * stake * (S295 - mean_surv(outs, k * x + TC)) - ping_cost * c
    return B.argmax(1)

def export_model(nets, mu, sd, path, meta):
    r9 = lambda a: np.vectorize(lambda v: float("%.9g" % v))(np.asarray(a, float)).tolist()      # float32-exact precision, short text
    doc = dict(schema="cg.cache.predictor/1", kind="weibull3-mixture-mlp", names=NAMES, mu=r9(mu), sd=r9(sd),
               rule=dict(lam=LAM, x_s=X_S, kmax=KMAX, tc_s=TC), nets=[{k: r9(v) for k, v in n.items()} for n in nets], meta=meta)
    json.dump(doc, open(path, "w"))

# ---------------------------------------------------------------- training (torch; build-time only)
def train(X, y, ev, tend, seeds=(0, 1, 2), epochs=12, nthreads=2):
    """X standardised float32 (n,38); y = time to return (event) or censoring time (s); ev = 1 if the return was observed. The last 15% of rows by tend are the early-stopping slice."""
    import torch, torch.nn as nn
    torch.set_num_threads(nthreads); cut = np.quantile(tend, 0.85); va = tend >= cut; tr = ~va
    Xt, yt, et = torch.tensor(X), torch.tensor(y, dtype=torch.float32), torch.tensor(ev, dtype=torch.float32)
    def nll(o, yy, ee):
        lg = o[..., :4]; la = o[..., 4:7]; b = 0.3 + 3.7 * torch.sigmoid(o[..., 7:10]); lp = torch.log_softmax(lg, -1); ly = torch.log(yy.clamp_min(0.05)).unsqueeze(-1)
        u = torch.exp(b * (ly - la)); logf = torch.log(b) - la + (b - 1) * (ly - la) - u
        ll_e = torch.logsumexp(lp[..., 1:] + logf, -1); ll_c = torch.logsumexp(torch.cat([lp[..., :1], lp[..., 1:] - u], -1), -1)
        return -torch.where(ee > 0.5, ll_e, ll_c)
    nets = []
    for sd in seeds:
        torch.manual_seed(sd); rng = np.random.RandomState(sd)
        net = nn.Sequential(nn.Linear(X.shape[1], H1), nn.Tanh(), nn.Linear(H1, H2), nn.Tanh(), nn.Linear(H2, NO))
        with torch.no_grad(): net[4].weight.mul_(0.1); net[4].bias.zero_(); net[4].bias[4:7] = torch.log(torch.tensor([3.0, 60.0, 1500.0])); net[4].bias[0] = -1.0
        opt = torch.optim.Adam(net.parameters(), 3e-3, weight_decay=1e-5); itr = np.where(tr)[0]; iva = torch.tensor(np.where(va)[0]); best = (1e18, None, 0)
        for ep in range(epochs):
            net.train(); rng.shuffle(itr)
            for i in range(0, len(itr), 256):
                b = torch.tensor(itr[i:i + 256]); loss = nll(net(Xt[b]), yt[b], et[b]).mean(); opt.zero_grad(); loss.backward(); nn.utils.clip_grad_norm_(net.parameters(), 2.0); opt.step()
            net.eval()
            with torch.no_grad(): vl = float(nll(net(Xt[iva]), yt[iva], et[iva]).mean()) if len(iva) else 0.0
            if best[1] is None or vl < best[0] - 1e-4: best = (vl, {k: v.clone() for k, v in net.state_dict().items()}, ep)
            elif ep - best[2] >= 2: break
            for g in opt.param_groups: g["lr"] *= 0.8
        net.load_state_dict(best[1]); sdict = {k: v.double().numpy() for k, v in net.state_dict().items()}
        nets.append(dict(W1=sdict["0.weight"].T, b1=sdict["0.bias"], W2=sdict["2.weight"].T, b2=sdict["2.bias"], W3=sdict["4.weight"].T, b3=sdict["4.bias"]))
    return nets

def labels(v):
    """(y, event) from a train_view frame: time to return if observed, else the censoring time."""
    ev = (v.compat == 1).values; y = np.where(ev, v.dt_next_s.values, v.cens_time_s.values); return np.nan_to_num(y, nan=1.0), ev.astype(float)

def train_view(dp, horizon_ms):
    """rows with every outcome resolving after horizon_ms hidden (right-censored there)."""
    v = dp.copy(); nts = v.t_end_ms + v.dt_next_s * 1000.0; late = (v.compat == 1) & (nts > horizon_ms)
    v.loc[late, "compat"] = 0; v.loc[late, "dt_next_s"] = np.nan
    v["cens_time_s"] = np.where(v.compat == 0, np.maximum((horizon_ms - v.t_end_ms) / 1000.0, 0), np.nan); return v

# ---------------------------------------------------------------- closed-form replay (engine-free)
import os
CALIBRATION = json.load(open(os.path.join(os.path.dirname(os.path.abspath(__file__)), "engine_calibration.json")))      # fitted to the pinned replay engine
CF_TC, CF_LATE, CF_PING_DUR = 296.5, 1.0, 2.0
CF_SCALE, CF_PING = CALIBRATION["benefit_scale"], CALIBRATION["ping_scale"]

def plan_eval(dp, K, x=X_S, stop=None):
    """Per-row (net $, pings fired, rescued) of 'ping up to K[i] times, x s apart, stop at the first return'.
    benefit = CF_SCALE * stake_next if the plan keeps an entry alive that would have died; cost = CF_PING * fired * ping_cost.
    stop (optional, seconds after the response end, per row): a client SessionEnd that arrives then cancels every ping that would start later.
    The two constants are the least-squares fit of this closed form to the pinned replay engine over 40 uniform plans (engine_calibration.json); dollars are calibrated closed-form units."""
    K = np.asarray(K); g = np.where(dp.compat.values == 1, dp.dt_next_s.values, np.inf)
    ga = np.fmin(g, np.where(np.isnan(dp.dt_next_any_s.values), np.inf, dp.dt_next_any_s.values)); u = (dp.t_end_ms.values - dp.ts_ms.values) / 1000.0
    stake = CF_SCALE * np.nan_to_num(dp.stake_next.values.astype(float)) * (dp.compat.values == 1); ok = dp.dp_ok.values == 1
    live = ok.copy(); fired = np.zeros(len(dp)); touch = np.zeros(len(dp)); cum = 0.0
    for j in range(1, (int(K.max()) if len(K) else 0) + 1):
        cum += x; s = cum - u + CF_LATE; f = live & (s < ga) & (K >= j) & (True if stop is None else s < stop); fired += f; touch = np.where(f, s + CF_PING_DUR, touch); live = f
    resc = ok & (K > 0) & ((g - touch) < CF_TC) & ~(g < CF_TC)
    return np.asarray(stake * resc - CF_PING * fired * dp.ping_cost.values.astype(float) * ok, float), fired, resc.astype(float)

def session_boot(per_row, sess, B=1000, seed=0):
    """95% CI of sum(per_row) by bootstrap over sessions."""
    codes, uniq = pd.factorize(sess); tot = np.bincount(codes, weights=per_row, minlength=len(uniq)); r = np.random.RandomState(seed)
    s = tot[r.randint(0, len(uniq), (B, len(uniq)))].sum(1); return [round(float(np.percentile(s, 2.5)), 2), round(float(np.percentile(s, 97.5)), 2)]
