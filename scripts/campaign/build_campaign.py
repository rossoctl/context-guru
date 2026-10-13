"""Build a predictive keep-alive campaign (spec cg.cache.campaign/2) for one scope from its own history.
   export decision points -> causal features -> train (3 seeds) on the first 70% of the window -> replay the held-out last 30% against every baseline -> spec.json + model.json
usage: build_campaign.py --out DIR --scope user|tenant --id ID --since MS --until MS (--requests-db DB --prices JSON | --dp-db DP.db) [--objective net_usd] [--base-model model.json]
Writes DIR/status.json as it goes (the server polls it). Class of every dollar: counterfactual closed-form replay on this scope's own history (see cgpredict.plan_eval)."""
import argparse, datetime, hashlib, json, os, sqlite3, sys, time, traceback
import numpy as np, pandas as pd
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import cgpredict as C, dpbuild as D, gbsfeat as G, lgbsurv as L

MIN_TEST, MIN_SESS, MIN_EVENTS = 500, 20, 50
SPEC_SCHEMA = "cg.cache.campaign/2"

def status(out, **kw):
    kw["t"] = time.time(); tmp = os.path.join(out, "status.json.tmp"); json.dump(kw, open(tmp, "w")); os.replace(tmp, os.path.join(out, "status.json"))

def load_dp_db(path, tenant):
    """research dp.db (pseudonymised columns) -> the columns this module expects."""
    d = pd.read_sql("select * from dp where dp_ok = 1" + (f" and user_pseudo = '{tenant}'" if tenant else ""), sqlite3.connect(f"file:{path}?mode=ro", uri=True))
    return d.drop(columns=["tenant", "session", "thread_id"]).rename(columns={"user_pseudo": "tenant", "session_pseudo": "session", "thread_pseudo": "thread_id"})

def baselines(dp, ktune):
    """name -> K per row (uniform plans at 280 s unless noted)."""
    n = len(dp); done = dp.stop_reason.isin(["end_turn", "max_tokens", "refusal"]).values; merged = ((dp.prefix_tok >= 80000) & (dp.ping_cost <= 0.50)).values
    return {"never": np.zeros(n, int), "blanket-K1": np.ones(n, int), "blanket-K2": np.full(n, 2), "blanket-K3": np.full(n, 3), f"best-single-plan(K{ktune})": np.full(n, ktune),
            "merged-rule(K2,>=80k,<=$0.50)": np.where(merged, 2, 0), "stop-reason-gated(K2)": np.where(done, 2, 0)}

def evaluate(dp, K_model, ktune):
    """held-out table: every policy replayed on the same rows; paired session-bootstrap differences against the model."""
    sess = dp.session.values + "|" + dp.tenant.values
    plans = {"predictive(model)": np.asarray(K_model), **baselines(dp, ktune)}
    res = {k: C.plan_eval(dp, K) for k, K in plans.items()}
    orc = np.max([C.plan_eval(dp, np.full(len(dp), k))[0] for k in range(0, C.KMAX + 1)], 0).clip(0)     # class ORACLE: hindsight best uniform K per row
    rows = []
    for k, (net, fired, resc) in res.items():
        pc = C.CF_PING * (fired * dp.ping_cost.values).sum(); wasted = C.CF_PING * (fired * dp.ping_cost.values * (resc == 0)).sum()
        rows.append(dict(strategy=k, net_usd=round(float(net.sum()), 2), ci95=C.session_boot(net, sess), gap_captured=round(float(net.sum() / orc.sum()), 3) if orc.sum() > 0 else None,
                         pings=int(fired.sum()), credited_rate=round(float(resc.sum() / fired.sum()), 3) if fired.sum() else None, ping_usd=round(float(pc), 2), wasted_ping_usd=round(float(wasted), 2)))
    paired = [dict(vs=k, diff_usd=round(float((res["predictive(model)"][0] - r[0]).sum()), 2), ci95=C.session_boot(res["predictive(model)"][0] - r[0], sess, seed=1))
              for k, r in res.items() if k != "predictive(model)"]
    return rows, paired, float(orc.sum())

def main():
    a = argparse.ArgumentParser()
    a.add_argument("--out", required=True); a.add_argument("--scope", default="tenant"); a.add_argument("--id", required=True)
    a.add_argument("--since", type=int, default=0); a.add_argument("--until", type=int, default=2 ** 62)
    a.add_argument("--requests-db"); a.add_argument("--prices"); a.add_argument("--dp-db"); a.add_argument("--objective", default="net_usd")
    a.add_argument("--base-model", help="evaluate-only fallback when the scope has too little history to train"); a.add_argument("--model", default="lgb", choices=["lgb", "mlp"], help="lgb: LightGBM discrete-time hazard (default); mlp: Weibull-mixture MLP"); a.add_argument("--rounds", type=int, default=300)
    a.add_argument("--drop", default="", help="comma list of feature names held constant (ablation: what a live input is worth)"); a.add_argument("--seeds", default="0,1,2"); a.add_argument("--epochs", type=int, default=12)
    a = a.parse_args(); os.makedirs(a.out, exist_ok=True)
    try: build(a)
    except Exception as e:
        status(a.out, state="failed", error=f"{type(e).__name__}: {e}"); traceback.print_exc(); sys.exit(1)

def build(a):
    t0 = time.time()
    if a.objective != "net_usd": raise ValueError("only objective net_usd is supported")
    status(a.out, state="running", stage="export")
    if a.dp_db and a.model == "lgb": raise ValueError("--dp-db carries no request metadata; the lgb model needs --requests-db")
    if a.dp_db: dp = load_dp_db(a.dp_db, a.id if a.id != "*" else None); dp = dp[(dp.ts_ms >= a.since) & (dp.ts_ms < a.until)].reset_index(drop=True)
    else: dp = D.build_dp(D.load_requests(a.requests_db, a.since, a.until, None if a.id == "*" else a.id), json.load(open(a.prices)))
    dp = dp.sort_values(["ts_ms", "dp_id"]).reset_index(drop=True); ok = dp.dp_ok.values == 1
    n_sess = int(dp[ok].groupby(["tenant", "session"]).ngroups) if ok.any() else 0
    window = dict(start_ms=int(dp.ts_ms.min()) if len(dp) else a.since, end_ms=int(dp.t_end_ms.max()) if len(dp) else a.until, n_decision_points=int(ok.sum()), n_sessions=n_sess)
    spec = dict(schema=SPEC_SCHEMA, version="2.0.0", built_at=datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
                builder=dict(engine="predictive-keepalive", backend="closed-form", code="scripts/campaign"), scope=dict(type=a.scope, id=a.id), window=window,
                objective=a.objective, pricing="the deployment's per-model rates (cache read, 5m write, input, output) at build time")
    cut = float(np.quantile(dp.t_end_ms.values[ok], 0.70)) if ok.sum() else 0
    tr_all = dp[ok & (dp.t_end_ms.values < cut)]; te = dp[ok & (dp.t_end_ms.values >= cut)].reset_index(drop=True)
    n_events = int((tr_all.compat == 1).sum())
    if len(te) < MIN_TEST or n_sess < MIN_SESS or n_events < MIN_EVENTS:
        spec["evidence"] = dict(recommendation="insufficient_data", reason=f"held-out rows {len(te)} (need {MIN_TEST}), sessions {n_sess} (need {MIN_SESS}), train returns {n_events} (need {MIN_EVENTS})")
        spec["policy"] = dict(kind="rules", segments=[dict(id="all", when=[], rule=dict(kind="none"))]); json.dump(spec, open(os.path.join(a.out, "spec.json"), "w"), indent=1)
        return status(a.out, state="done", recommendation="insufficient_data", seconds=round(time.time() - t0, 1))
    status(a.out, state="running", stage="features")
    itr = np.where(ok & (dp.t_end_ms.values < cut))[0]; ite = np.where(ok & (dp.t_end_ms.values >= cut))[0]; te = dp.iloc[ite].reset_index(drop=True)
    model_path = os.path.join(a.out, "model.json"); meta = dict(trained_rows=int(len(itr)), window=window, scope=dict(type=a.scope, id=a.id), model=a.model)
    drop = [n for n in a.drop.split(",") if n]
    if a.model == "lgb":
        windows = G.window_table(dp.iloc[itr]);  # training rows only: the served-window table must not see the held-out period
        Xr = G.raw_frame(dp, windows); enc = G.fit_encoders(Xr.iloc[itr]); X = G.encode(Xr, enc)
        for n in drop: X[n] = np.float32(0)
        status(a.out, state="running", stage="train")
        v = C.train_view(dp.iloc[itr], cut); y, ev = L.labels(v)
        mdl = L.fit(X.iloc[itr].reset_index(drop=True), y, ev, threads=2, hp=dict(rounds=a.rounds))
        status(a.out, state="running", stage="replay")
        Se = L.surv_edges(L.hazard(mdl, X.iloc[ite].reset_index(drop=True))); Kte = L.decide(L.edge_surv(Se), (te.rebuild_cost - te.read_cost).values, te.ping_cost.values)
        L.export(mdl, G.NAMES, enc, windows, model_path, meta); rule = dict(lam=L.LAM, x_s=L.X0, kmax=L.K, tc_s=L.TC); kind, nfeat = "lgb-discrete-hazard", len(G.NAMES)
    else:
        Xraw = C.raw_features(dp); mu = Xraw[itr].mean(0); sd = Xraw[itr].std(0) + 1e-6
        Xs = np.nan_to_num((Xraw - mu) / sd).astype(np.float32)
        for n in drop: Xs[:, C.NAMES.index(n)] = 0.0
        status(a.out, state="running", stage="train")
        v = C.train_view(dp.iloc[itr], cut); y, ev = C.labels(v)
        nets = C.train(Xs[itr], y, ev, v.t_end_ms.values, seeds=tuple(int(s) for s in a.seeds.split(",")), epochs=a.epochs)
        status(a.out, state="running", stage="replay")
        ref = [C.Net(n) for n in nets]
        Kte = C.decide([r(Xs[ite].astype(float)) for r in ref], dp.prefix_tok.values[ite], dp.p_write5m.values[ite], dp.p_read.values[ite], dp.ping_cost.values[ite])
        C.export_model(nets, mu, sd, model_path, meta); rule = dict(lam=C.LAM, x_s=C.X_S, kmax=C.KMAX, tc_s=C.TC); kind, nfeat = "weibull3-mixture-mlp", len(C.NAMES)
    trk = {k: C.plan_eval(tr_all.reset_index(drop=True), np.full(len(tr_all), k))[0].sum() for k in range(C.KMAX + 1)}; ktune = int(max(trk, key=trk.get))
    table, paired, orc = evaluate(te, Kte, ktune)
    # one segment: the model decides everything. (Per-segment rules are supported by the spec and the runtime; the builder does not pick them on the held-out slice, which would bias the table it reports.)
    seg = [dict(id=f"family-{f}", n_test=int((te.family.values == f).sum()), test_net_usd=round(float(C.plan_eval(te, np.where(te.family.values == f, Kte, 0))[0].sum()), 2)) for f in sorted(te.family.unique())]
    table_final, paired = table, paired
    sha = hashlib.sha256(open(model_path, "rb").read()).hexdigest()
    best_simple = max((r for r in table_final if r["strategy"] not in ("predictive(model)", "never")), key=lambda r: r["net_usd"])
    me = next(r for r in table_final if r["strategy"] == "predictive(model)"); pv = next(p for p in paired if p["vs"] == best_simple["strategy"])
    # ship only if positive AND shown better than the best simple rule (paired 95% CI above 0); anything else is baseline. Shadow mode stays available to collect live evidence.
    rec = "ship" if (me["net_usd"] > 0 and pv["ci95"][0] > 0) else "baseline"
    spec["menu"] = {"none": dict(gaps_s=[], max_pings=0, tier="5m"), "model": dict(gaps_s=[rule["x_s"]] * rule["kmax"], max_pings=rule["kmax"], tier="5m", stop_on_miss=True, note="K chosen per decision point by the model")}
    spec["policy"] = dict(kind="model+segments", model=dict(ref="model.json", sha256=sha, schema="cg.cache.predictor/1", kind=kind, n_features=nfeat, rule=rule),
                          segments=[dict(id="all", when=[], rule=dict(kind="model"))], fallback=dict(kind="none"))
    spec["segments"] = seg
    spec["evidence"] = dict(klass="counterfactual: closed-form replay on this scope's own history; oracle row = class oracle (hindsight, never deployable)",
                            split=f"model trained on rows ending before {int(cut)} (first 70% by response end), scored on the last 30%: {len(te)} decision points, {te.groupby(['tenant','session']).ngroups} sessions",
                            table=table_final, oracle_net_usd=round(orc, 2), paired_vs_baselines=paired, recommendation=rec, best_simple_baseline=best_simple["strategy"],
                            engine=C.CALIBRATION, note="dollars are closed-form units calibrated to the pinned replay engine (evidence.engine); compare strategies, not absolute levels")
    spec["caveats"] = ["history of ONE scope; a tenant with few returns gets a weak model, see recommendation", "returns not yet observed at the window end count as no-return (pings wasted), a conservative convention",
                       "closed-form replay: pings are assumed to land on schedule and not fail"]
    json.dump(spec, open(os.path.join(a.out, "spec.json"), "w"), indent=1)
    status(a.out, state="done", recommendation=rec, seconds=round(time.time() - t0, 1), net_usd=me["net_usd"])

if __name__ == "__main__": main()
