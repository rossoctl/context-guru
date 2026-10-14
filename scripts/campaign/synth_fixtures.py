"""Regenerate every fixture under predictive/testdata/ from SYNTHETIC traffic (fixed seeds; no real requests).
usage: synth_fixtures.py [OUT_DIR]      (default predictive/testdata; needs numpy, pandas, lightgbm, scikit-learn, torch)

1. a synthetic dashboard `requests` table: several tenants with different habits, Claude Code style tool loops and user turns, subagent threads,
   overlapping requests on one thread, compactions, a model mix, streamed and buffered responses;
2. lgb.json.gz / lgb.model.json  via export_fixture_lgb.py, parity.json.gz / parity.model.json via export_fixture_mlp.py, on that table;
3. autotune_fixture.json.gz: synthetic per-scope plan gains and the answer of the reference tuner below (the Go AutoTune must agree)."""
import gzip, json, os, sqlite3, subprocess, sys, tempfile
import numpy as np

HERE = os.path.dirname(os.path.abspath(__file__))
EPOCH = 1_893_456_000_000          # 2030-01-01T00:00Z: a synthetic epoch, so no timestamp is a real one
DAY = 86400e3
PRICES = {  # $ per token
    "aws/claude-opus-4": {"in": 15e-6, "out": 75e-6, "cache_read": 1.5e-6, "cache_write_5m": 18.75e-6},
    "aws/claude-sonnet-4": {"in": 3e-6, "out": 15e-6, "cache_read": 3e-7, "cache_write_5m": 3.75e-6},
    "aws/claude-haiku-4-5": {"in": 1e-6, "out": 5e-6, "cache_read": 1e-7, "cache_write_5m": 1.25e-6},
}
COLS = ("ts,tenant_id,session_id,model,agent,messages,tools,system_blocks,stop_reason,thinking_mode,stream,fresh_input,cache_read,cache_write,output_tokens,"
        "cost_usd,cg_latency_ms,upstream_ms,token_accounting,keepalive,status,provider,cache_ttl,ttfb_ms,reasoning_effort,max_tokens").split(",")


def synth_requests(path, n_tenants=6, sessions_per_tenant=70, seed=7, overlap=0.08):
    r = np.random.RandomState(seed); rows = []
    for ti in range(n_tenants):
        think = r.uniform(20, 400)                     # this tenant's typical pause before the next user turn (s)
        away = r.uniform(0.05, 0.35)                   # chance a user turn is followed by a long absence
        models = list(PRICES)[:2] if ti % 3 else list(PRICES)
        agent = "claude-cli" if ti != 4 else "openai"
        for si in range(sessions_per_tenant):
            day = r.randint(0, 14); t = EPOCH + day * DAY + r.uniform(7, 19) * 3600e3 + r.uniform(0, 3600e3)
            model = models[r.randint(len(models))]; tools = int(r.choice([8, 15, 30])); thinking = r.choice(["", "adaptive", "disabled"])
            effort = r.choice(["", "high", "medium"]); stream = int(r.rand() < 0.8); maxt = int(r.choice([8192, 32000, 64000]))
            sess = f"t{ti}-s{si}"; prefix = int(r.uniform(8e3, 40e3)); msgs = 1; quiet = 0

            def emit(ts, mdl, tl, sb, m, pre, wr, stop, up):
                fresh, out = int(r.randint(3, 400)), int(r.randint(20, 4000))
                p = PRICES[mdl]; cost = fresh * p["in"] + out * p["out"] + (pre - wr) * p["cache_read"] + wr * p["cache_write_5m"]
                ok = r.rand() > 0.01
                rows.append((int(ts), f"tenant-{ti}", sess, mdl, agent, m, tl, sb, stop, thinking, stream, fresh, pre - wr, wr, out, cost,
                             float(r.randint(5, 60)), float(int(up)), "complete" if r.rand() > 0.02 else "partial", 0, 200 if ok else 529, "anthropic",
                             "ephemeral_5m", float(r.uniform(300, 3000)) if stream else 0.0, effort, maxt))

            for turn in range(int(r.randint(2, 14))):
                for step in range(int(r.geometric(0.25))):                   # a tool loop, then the turn's answer
                    up = r.uniform(2e3, 40e3); stop = "tool_use" if step else r.choice(["tool_use", "end_turn"], p=[.9, .1])
                    wr = prefix if (turn == 0 and step == 0) else int(r.uniform(300, 3000))
                    emit(t, model, tools, 2, msgs, prefix, min(wr, prefix), stop, up)
                    quiet -= 1
                    # an overlapping request on the same thread (a retry or a fork); at most one per 15 requests of a thread,
                    # because the live tracker's history inputs need about 10 requests to forget one (Row.Skipped)
                    if quiet <= 0 and r.rand() < overlap:
                        quiet = 15
                        off = r.uniform(300, 0.8 * up)                   # it starts during the first one and ends before or after it
                        emit(t + off, model, tools, 2, msgs, prefix, int(r.uniform(300, 1500)), "tool_use", (up - off) * r.uniform(0.2, 1.8))
                    if r.rand() < 0.1:                                   # a subagent thread running beside the main one
                        st, sp = t + r.uniform(500, 3000), int(r.uniform(4e3, 15e3))
                        for j in range(int(r.randint(2, 6))):
                            sup = r.uniform(2e3, 15e3); emit(st, "aws/claude-haiku-4-5", 6, 1, 1 + 2 * j, sp, sp if j == 0 else 600, "tool_use" if j < 3 else "end_turn", sup)
                            st += sup + r.uniform(200, 4000); sp += 900
                    t += up + r.uniform(0.2e3, 20e3); prefix += int(r.uniform(400, 6000)); msgs += 2
                emit(t, model, tools, 2, msgs, prefix, int(r.uniform(300, 3000)), r.choice(["end_turn", "max_tokens", "stop_sequence"], p=[.9, .05, .05]), r.uniform(2e3, 30e3))
                t += 30e3; msgs += 2; prefix += 2000
                gap = r.exponential(think) if r.rand() > away else r.uniform(600, 4 * 3600)
                t += gap * 1e3
                if prefix > 150e3 and r.rand() < 0.5:                         # compaction: the conversation restarts small
                    prefix, msgs = int(r.uniform(10e3, 25e3)), 3
    c = sqlite3.connect(path)
    c.execute("create table requests (id integer primary key autoincrement, ts integer, tenant_id text, session_id text, model text, agent text, messages integer, "
              "tools integer, system_blocks integer, stop_reason text, thinking_mode text, stream integer, fresh_input integer, cache_read integer, cache_write integer, "
              "output_tokens integer, cost_usd real, cg_latency_ms real, upstream_ms real, token_accounting text, keepalive integer, status integer, provider text, "
              "cache_ttl text, ttfb_ms real, reasoning_effort text, max_tokens integer)")
    rows.sort(key=lambda x: x[0])
    c.executemany(f"insert into requests ({','.join(COLS)}) values ({','.join('?' * len(COLS))})", rows)
    c.commit(); c.close()
    return len(rows)


# ---------------------------------------------------------------- the auto tuner, reference implementation (predictive/autotune.go must agree)
HL = (1.0, 3.0, 10.0, 30.0, np.inf); KAP = (0.0, 30.0, 100.0, 300.0, 1000.0, 3000.0, 10000.0, 30000.0, np.inf); TMAX_MS = 2700e3


def fit(G, scope, t, rows, tref, hl, kap, ns):
    wt = np.exp2(-(tref - t[rows]) / (hl * DAY)) if np.isfinite(hl) else np.ones(rows.sum())
    g = (G[rows] * wt[:, None]).sum(0) / wt.sum()
    S = np.zeros((ns, G.shape[1])); np.add.at(S, scope[rows], G[rows] * wt[:, None]); N = np.bincount(scope[rows], weights=wt, minlength=ns)
    post = np.broadcast_to(g, S.shape) if not np.isfinite(kap) else (S + kap * g) / np.maximum(N + kap, 1e-12)[:, None]
    post = np.where((N > 0)[:, None], post, g)
    return post.argmax(1), int(g.argmax())


def tune(G, scope, t, tr, ns):
    ic = np.quantile(t[tr], 0.7); f = tr & (t + TMAX_MS <= ic); s = tr & (t >= ic); best = None
    for kap in KAP[::-1]:                      # safest first: a later setting must be strictly better to win
        for hl in HL[::-1]:
            ch, _ = fit(G, scope, t, f, ic, hl, kap, ns); v = G[np.where(s)[0], ch[scope[s]]].sum()
            if best is None or v > best[2] + 1e-9: best = (hl, kap, v)
    return best[0], best[1]


def synth_autotune(n=20000, ns=19, seed=11, A=7, x_s=270.0, tc_s=283.0):
    """Rows of ns scopes over 28 days. Each scope has its own return-gap mixture that drifts over time, so the answer is neither the fleet plan for
    everyone nor each scope's own in-sample best. gain[k] = net $ of pinging k times, x_s apart: the rescue if the return falls in a window a ping kept
    open, minus the pings sent before the return."""
    r = np.random.default_rng(seed)
    w = r.dirichlet(np.ones(ns) * 0.6); scope = np.sort(r.choice(ns, n, p=w))
    t = EPOCH + np.sort(r.uniform(0, 28 * DAY, n)); r.shuffle(t)
    mid = r.uniform(200, 1400, ns); drift = r.normal(0, 300, ns); quick = r.uniform(0.3, 0.8, ns)
    age = (t - EPOCH) / (28 * DAY)
    m = np.clip(mid[scope] + drift[scope] * age, 150, 2500)
    gap = np.where(r.random(n) < quick[scope], r.exponential(60, n), r.lognormal(np.log(m), 0.5))
    gap = np.where(r.random(n) < 0.15, np.inf, gap)                    # never came back
    stake = r.lognormal(np.log(0.3), 0.8, n); ping = stake * r.uniform(0.05, 0.2, n)
    G = np.zeros((n, A))
    for k in range(1, A):
        sent = np.minimum(np.floor(np.where(np.isfinite(gap), gap, 1e18) / x_s), k)    # pings fired before the return
        rescued = (gap > tc_s) & (gap <= k * x_s + tc_s)
        G[:, k] = np.where(rescued, stake, 0) - sent * ping
    G = np.round(G, 9); now = float(t.max() + TMAX_MS); tr = t + TMAX_MS <= now
    fh, fk = tune(G, scope, t, tr, ns); fc, fg = fit(G, scope, t, tr, now, fh, fk, ns)
    nn = lambda v: None if not np.isfinite(v) else float(v)
    return dict(now_ms=int(now), scope=[f"u{int(x):02d}" for x in scope], end_ms=t.astype(np.int64).tolist(), gain=G.tolist(),
                want_half_life_days=nn(fh), want_kappa=nn(fk), want_plan={f"u{u:02d}": int(fc[u]) for u in np.unique(scope)}, want_fleet=fg)


if __name__ == "__main__":
    out = os.path.abspath(sys.argv[1] if len(sys.argv) > 1 else os.path.join(HERE, "..", "..", "predictive", "testdata"))
    with tempfile.TemporaryDirectory() as d:
        db, db1, pr = os.path.join(d, "requests.db"), os.path.join(d, "serial.db"), os.path.join(d, "prices.json")
        print("synthetic requests:", synth_requests(db), "+", synth_requests(db1, overlap=0)); json.dump(PRICES, open(pr, "w"))
        subprocess.run([sys.executable, os.path.join(HERE, "export_fixture_lgb.py"), db, pr, os.path.join(out, "lgb")], check=True)
        # the MLP feature parity has no allowance for overlapping requests on a thread (where live and offline definitions differ by
        # construction; the LightGBM fixture covers them), so its traffic has none
        subprocess.run([sys.executable, os.path.join(HERE, "export_fixture_mlp.py"), db1, pr, os.path.join(out, "parity.json.gz")], check=True)
    fx = synth_autotune()
    print("autotune: half-life", fx["want_half_life_days"], "kappa", fx["want_kappa"], "fleet K", fx["want_fleet"], "plans", sorted(set(fx["want_plan"].values())))
    json.dump(fx, gzip.open(os.path.join(out, "autotune_fixture.json.gz"), "wt"), separators=(",", ":"))
