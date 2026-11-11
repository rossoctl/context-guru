"""Parity fixture for the LightGBM model in the Go runtime: a stream of requests (tenant / session / thread ids replaced by small integers), the Python feature vectors for it, a
small model, and the Python survival values and decisions.   usage: export_fixture_lgb.py REQUESTS.db PRICES.json OUT_PREFIX    (run by synth_fixtures.py on synthetic traffic; the outputs are committed under predictive/testdata/)"""
import gzip, json, os, sys
import numpy as np, pandas as pd
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import dpbuild as D, gbsfeat as G, lgbsurv as L, cgpredict as C

db, prices, out = sys.argv[1:4]; NROWS = 1200
dp = D.build_dp(D.load_requests(db, 0, 2 ** 62, None), json.load(open(prices))).sort_values(["ts_ms", "dp_id"]).reset_index(drop=True)
# the window with the most parallel requests on a thread (a predecessor still running at this response's decision): the rows where live and offline definitions differ
prev_dec = dp.groupby(["tenant", "session", "thread_id"]).t_dec_ms.shift(1); par = (prev_dec > dp.t_dec_ms).astype(int).values; cs = np.r_[0, np.cumsum(par)]
starts = np.arange(len(dp) // 5, len(dp) - NROWS, 200); i0 = int(starts[np.argmax([cs[i + NROWS] - cs[i] for i in starts])]); print("parallel-request rows in the window:", int(cs[i0 + NROWS] - cs[i0]))
sub = dp.iloc[i0:i0 + NROWS].copy().reset_index(drop=True); tr = dp.iloc[:i0].reset_index(drop=True)
# the model is trained on the earlier rows, independent of the fixture window
windows = G.window_table(tr); Xr = G.raw_frame(tr, windows); enc = G.fit_encoders(Xr); ok = (tr.dp_ok == 1).values
v = C.train_view(tr, float(tr.t_end_ms.max())); y, ev = L.labels(v)
mdl = L.fit(G.encode(Xr, enc)[ok].reset_index(drop=True), y[ok], ev[ok], hp=dict(leaves=15, rounds=60, mcl=50), threads=2)
mpath = out + ".model.json"; L.export(mdl, G.NAMES, enc, windows, mpath, dict(note="parity fixture model"))
doc = json.load(open(mpath))
# counters inside the window are relative to it
sub["turn_in_thread"] = sub.groupby("thread_id").cumcount(); sub["session_turn"] = sub.groupby(["tenant", "session"]).cumcount()
sub["check"] = sub.t_dec_ms <= sub.ts_ms.max()
Xs = G.raw_frame(sub, windows); X = G.encode(Xs, enc)
Se = L.surv_edges(L.hazard(mdl, X.reset_index(drop=True)))
ts = [295.0] + [k * L.X0 + L.TC for k in range(1, L.K + 1)] + [k * L.X0 for k in range(1, L.K + 1)]
S = np.stack([L.edge_surv(Se)(t) for t in ts], 1)
K = L.decide(L.edge_surv(Se), (sub.rebuild_cost - sub.read_cost).values, sub.ping_cost.values)
stk = (sub.rebuild_cost - sub.read_cost).values
MULT = max((1.5, 2, 3, 4, 6, 8, 12), key=lambda m: len(set(L.decide(L.edge_surv(Se), stk, sub.ping_cost.values * m).tolist())) * 10 + min(np.bincount(L.decide(L.edge_surv(Se), stk, sub.ping_cost.values * m), minlength=5)[1:4]))
KH = L.decide(L.edge_surv(Se), stk, sub.ping_cost.values * MULT)       # a second cost regime, so more than one k is exercised
chk = L.ref_hazard(doc, X.values[:5].astype(float)); assert np.abs(chk - L.hazard(mdl, X.iloc[:5].reset_index(drop=True))).max() < 1e-9      # the exported trees == LightGBM
ids = {k: {u: i for i, u in enumerate(sub[k].unique())} for k in ("tenant", "session", "thread_id")}
nn = lambda v: None if (isinstance(v, float) and np.isnan(v)) else float(v)
ev_ = []
for i, r in sub.iterrows():
    ev_.append(dict(id=int(r.dp_id), tenant=ids["tenant"][r.tenant], session=ids["session"][r.session], thread=ids["thread_id"][r.thread_id], model=r.model, ts=int(r.ts_ms), end=int(r.t_end_ms), dec=int(r.t_dec_ms),
                    prefix=int(r.prefix_tok), cache_write=int(r.cache_write), fresh=int(r.fresh_input), out=int(r.output_tok), messages=int(r.messages), tools=int(r.tools), system_blocks=int(r.system_blocks),
                    max_tokens=int(r.max_tokens), stop=r.stop_reason, agent=r.agent if isinstance(r.agent, str) else "", provider=r.provider, thinking=r.thinking_mode, effort=r.reasoning_effort, cache_ttl=r.cache_ttl,
                    stream=bool(r.stream), ttfb_ms=float(r.ttfb_ms), upstream_ms=float(r.upstream_ms), cost=float(r.cost_usd), p_in=float(r.p_in), p_out=float(r.p_out), p_read=float(r.p_read), p_write5m=float(r.p_write5m),
                    feat=[nn(x) for x in Xs.iloc[i][[n for n in G.NAMES if n not in G.CATS]].values], cats=[str(Xs.iloc[i][c]) for c in G.CATS], probs=[float(x) for x in S[i]], k=int(K[i]), k_hi=int(KH[i]), check=bool(r.check)))
json.dump(dict(times=ts, hi_mult=MULT, numeric=[n for n in G.NAMES if n not in G.CATS], cat_order=G.CATS, events=ev_), gzip.open(out + ".json.gz", "wt"), separators=(",", ":"))
print(len(ev_), "events; k histogram", np.bincount(K, minlength=5).tolist(), np.bincount(KH, minlength=5).tolist(), "model bytes", os.path.getsize(mpath), "trees", len(doc["trees"]))
