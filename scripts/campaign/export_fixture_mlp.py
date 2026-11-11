"""Parity fixture for the Go runtime (Weibull-mixture MLP): a short stream of requests, the Python feature vectors for it, a small model, and the Python decisions.
usage: export_fixture_mlp.py REQUESTS.db PRICES.json OUT.json.gz     (run by synth_fixtures.py on synthetic traffic; the output is committed under predictive/testdata/)"""
import gzip, json, os, sys
import numpy as np, pandas as pd
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import cgpredict as C, dpbuild as D

db, prices, out = sys.argv[1:4]; NROWS = 1200
full = D.build_dp(D.load_requests(db, 0, 2 ** 62, None), json.load(open(prices)))
full = full.sort_values(["ts_ms", "dp_id"]).reset_index(drop=True)
# model: trained on the traffic before the fixture window, deliberately independent of the fixture rows
tr = full.iloc[:len(full) // 2].reset_index(drop=True)
X = C.raw_features(tr); mu, sd = X.mean(0), X.std(0) + 1e-6; Xs = np.nan_to_num((X - mu) / sd).astype(np.float32)
v = C.train_view(tr, float(tr.t_end_ms.max())); y, ev = C.labels(v)
nets = C.train(Xs, y, ev, v.t_end_ms.values, seeds=(0, 1), epochs=4)
mpath = out.replace(".json.gz", "") + ".model.json"; C.export_model(nets, mu, sd, mpath, dict(note="parity fixture model"))
doc = json.load(open(mpath)); ref = [C.Net(n) for n in doc["nets"]]; mu, sd = np.array(doc["mu"]), np.array(doc["sd"])
# the fixture window: 1,200 consecutive requests (all tenants) from the middle of the stream; session/thread counters are recomputed inside it
sub = full.iloc[len(full) // 2:len(full) // 2 + NROWS].copy().reset_index(drop=True)
# a row decided after the last request of the window could have a successor we cut off: it stays in the stream (its arrival matters) but is not checked
sub["check"] = sub.t_dec_ms <= sub.ts_ms.max()
sub["turn_in_thread"] = sub.groupby(["tenant", "session", "thread_id"]).cumcount(); sub["session_turn"] = sub.groupby(["tenant", "session"]).cumcount()
Xr = C.raw_features(sub); Xs = (Xr - mu) / sd
ts = [295.0] + [k * C.X_S + C.TC for k in range(1, C.KMAX + 1)] + [k * C.X_S for k in range(1, C.KMAX + 1)]
probs = np.stack([C.mean_surv([r(Xs) for r in ref], t) for t in ts], 1)
K = C.decide([r(Xs) for r in ref], sub.prefix_tok.values.astype(float), sub.p_write5m.values, sub.p_read.values, sub.ping_cost.values)
ids = {k: {u: i for i, u in enumerate(sub[k].unique())} for k in ("tenant", "session", "thread_id")}
ev_ = []
for i, r in sub.iterrows():
    ev_.append(dict(id=int(r.dp_id), tenant=ids["tenant"][r.tenant], session=ids["session"][r.session], thread=ids["thread_id"][r.thread_id], model=r.model, ts=int(r.ts_ms), end=int(r.t_end_ms), dec=int(r.t_dec_ms),
                    prefix=int(r.prefix_tok), cache_write=int(r.cache_write), fresh=int(r.fresh_input), out=int(r.output_tok), messages=int(r.messages), tools=int(r.tools),
                    stop=r.stop_reason, agent=r.agent_type, thinking=r.thinking if isinstance(r.thinking, str) else "", stream=bool(r.stream),
                    p_in=r.p_in, p_out=r.p_out, p_read=r.p_read, p_write5m=r.p_write5m, feat=[float(x) for x in Xr[i]], probs=[float(x) for x in probs[i]], k=int(K[i]), check=bool(r.check)))
json.dump(dict(times=ts, events=ev_), gzip.open(out, "wt"), separators=(",", ":"))
print(len(ev_), "events; k histogram", np.bincount(K, minlength=7).tolist(), "model", os.path.getsize(mpath), "bytes")
