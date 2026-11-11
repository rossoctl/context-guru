"""Self-check of the build pipeline on a synthetic dash `requests` table (the Go -> Python contract): python test_pipeline.py  (needs numpy, pandas, and lightgbm + scikit-learn or torch)."""
import json, os, sqlite3, subprocess, sys, tempfile
import numpy as np
HERE = os.path.dirname(os.path.abspath(__file__)); sys.path.insert(0, HERE)
import cgpredict as C

def synth(path, n_sessions=400, seed=0):
    r = np.random.RandomState(seed); c = sqlite3.connect(path)
    c.execute("""create table requests (id integer primary key autoincrement, ts integer, tenant_id text, session_id text, model text, agent text, messages integer, tools integer,
        system_blocks integer, stop_reason text, thinking_mode text, stream integer, fresh_input integer, cache_read integer, cache_write integer, output_tokens integer, cost_usd real,
        cg_latency_ms real, upstream_ms real, token_accounting text, keepalive integer, status integer, provider text, cache_ttl text, ttfb_ms real, reasoning_effort text, max_tokens integer)""")
    t0, rows = 1_800_000_000_000, []
    for s in range(n_sessions):
        t = t0 + int(r.uniform(0, 5 * 86400e3)); prefix = int(r.choice([5000, 40000, 120000])); msgs = 1
        for turn in range(int(r.randint(3, 25))):
            up = float(r.uniform(2000, 30000)); rd = prefix if turn else 0; wr = 0 if turn else prefix
            rows.append((t, "ten", f"s{s}", "aws/claude-sonnet-4", "claude-cli", msgs, 5, 2, "end_turn" if r.rand() < .3 else "tool_use", "", 1, 7, rd, wr + 800, 300, 0.1, 40.0, up, "complete", 0, 200, "anthropic", "ephemeral_5m", 0.0, "high", 8192))
            gap = float(r.choice([r.uniform(5, 60), r.uniform(280, 700)], p=[.8, .2])) * 1000; t += int(up + gap); prefix += 800; msgs += 2
    c.executemany("insert into requests (ts,tenant_id,session_id,model,agent,messages,tools,system_blocks,stop_reason,thinking_mode,stream,fresh_input,cache_read,cache_write,output_tokens,cost_usd,cg_latency_ms,upstream_ms,token_accounting,keepalive,status,provider,cache_ttl,ttfb_ms,reasoning_effort,max_tokens) values (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)", rows)
    c.commit(); c.close(); return len(rows)

def test_plan_eval_and_decide():
    import pandas as pd
    dp = pd.DataFrame(dict(compat=[1, 1, 0], dt_next_s=[500.0, 100.0, np.nan], dt_next_any_s=[500.0, 100.0, np.nan], t_end_ms=[20000.0] * 3, ts_ms=[10000.0] * 3,
                           stake_next=[100.0, 100.0, 0.0], ping_cost=[1.0] * 3, dp_ok=[1] * 3))
    net, fired, resc = C.plan_eval(dp, np.array([1, 1, 3]))
    assert fired.tolist() == [1, 0, 3] and resc.tolist() == [1, 0, 0], (fired, resc)        # row 0: one ping bridges a 500 s gap; row 1 returns first; row 2 never returns
    assert abs(net[0] - (C.CF_SCALE * 100 - C.CF_PING)) < 1e-9 and net[1] == 0 and abs(net[2] + 3 * C.CF_PING) < 1e-9
    # a net that predicts "returns at ~600 s, certainly" must buy pings when the stake is big and none when it is tiny
    o = np.tile(np.array([[-9, 9, -9, -9, np.log(600.0), 0, 0, 5, 0, 0]]), (2, 1)).astype(float)
    K = C.decide([o], np.array([1e6, 1.0]), np.array([3.75e-6] * 2), np.array([3e-7] * 2), np.array([0.3, 0.3]))
    assert K[0] > 0 and K[1] == 0, K

def test_build_end_to_end(model):
    with tempfile.TemporaryDirectory() as d:
        db = os.path.join(d, "r.db"); n = synth(db); out = os.path.join(d, "o")
        json.dump({"aws/claude-sonnet-4": {"in": 3e-6, "out": 15e-6, "cache_read": 3e-7, "cache_write_5m": 3.75e-6}}, open(os.path.join(d, "p.json"), "w"))
        cmd = [sys.executable, os.path.join(HERE, "build_campaign.py"), "--out", out, "--scope", "tenant", "--id", "ten", "--since", "0", "--until", str(2 ** 60),
               "--requests-db", db, "--prices", os.path.join(d, "p.json"), "--seeds", "0", "--epochs", "3", "--rounds", "30", "--model", model]
        subprocess.run(cmd, check=True, capture_output=True)
        st = json.load(open(os.path.join(out, "status.json"))); assert st["state"] == "done", st
        spec = json.load(open(os.path.join(out, "spec.json"))); assert spec["schema"] == "cg.cache.campaign/2"
        ev = spec["evidence"]; assert "table" in ev, ev        # synthetic returns are plentiful: enough history to evaluate
        names = {r["strategy"] for r in ev["table"]}
        assert {"predictive(model)", "never", "blanket-K2", "blanket-K3"} <= names, names
        assert ev["recommendation"] in ("ship", "baseline"), ev["recommendation"]
        m = json.load(open(os.path.join(out, "model.json")))
        assert m["kind"] == ("lgb-discrete-hazard" if model == "lgb" else "weibull3-mixture-mlp") and len(m["names"]) == (72 if model == "lgb" else 37)
        # too little history is said, not papered over
        cmd2 = cmd[:cmd.index("--out") + 1] + [out + "2"] + cmd[cmd.index("--out") + 2:]; cmd2[cmd2.index("--id") + 1] = "nobody"
        subprocess.run(cmd2, check=True, capture_output=True)
        assert json.load(open(os.path.join(out + "2", "spec.json")))["evidence"]["recommendation"] == "insufficient_data"
        print(model, "synthetic rows", n, "->", ev["recommendation"], "model net", ev["table"][0]["net_usd"])

if __name__ == "__main__":
    test_plan_eval_and_decide(); ran = 0
    for mod, model in (("lightgbm", "lgb"), ("torch", "mlp")):        # each model is tested where its library is installed
        try: __import__(mod)
        except ImportError: print(mod, "missing:", model, "build skipped"); continue
        test_build_end_to_end(model); ran += 1
    assert ran, "neither lightgbm nor torch is installed"
    print("ok")
