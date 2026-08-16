#!/usr/bin/env python3
"""One-command iteration scorecard: start the proxy with a config, replay every
captured stream through /compact, and report the metrics that matter for ALL the
levers — per-component UNIQUE savings, total savings%, CG-added latency, LLM calls,
and the deterministic cache-aware cost delta (cachecost model) on the same stream.

Fast + cheap (no agent). Run after each code/config change to see the effect.

Usage: measure.py [--config codesmart] [--label iterN]
"""
import argparse, json, os, subprocess, time, urllib.request, signal, hashlib, importlib.util
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import cgenv  # base URLs and credentials for both the hosted and the local deployment

CG = Path("/home/vpcuser/projects/context-engineering/context-guru")
BIN = "/tmp/cg-runs/cg-proxy-d1"
PORT = 4066
CAPS = {"swe": "/tmp/cg-runs/capture-swebench.jsonl",
        "tb": "/tmp/cg-runs/capture-tb.jsonl",
        "tau": "/tmp/cg-runs/capture-tau.jsonl"}
IN_RATE, CREAD, CWRITE = 2e-6, 0.2e-6, 2.5e-6

spec = importlib.util.spec_from_file_location("swb", str(CG / "deploy/harbor/swebench.py"))
swb = importlib.util.module_from_spec(spec); spec.loader.exec_module(swb)


def content_text(body):
    parts = []
    sysv = body.get("system")
    if isinstance(sysv, str): parts.append(sysv)
    elif isinstance(sysv, list): parts += [b.get("text", "") for b in sysv if isinstance(b, dict)]
    for m in body.get("messages", []):
        c = m.get("content")
        if isinstance(c, str): parts.append(c)
        elif isinstance(c, list):
            for b in c:
                if isinstance(b, dict):
                    parts.append(b.get("text") or (b.get("content") if isinstance(b.get("content"), str) else json.dumps(b.get("content", ""))) or json.dumps(b.get("input", "")))
    return "\n".join(p for p in parts if p)


def toks(s): return max(1, len(s) // 4)


def cpfx(a, b):
    n = min(len(a), len(b)); i = 0
    while i < n and a[i] == b[i]: i += 1
    return i // 4


def conv_key(body):
    for m in body.get("messages", []):
        if m.get("role") == "user":
            c = m.get("content"); t = c if isinstance(c, str) else json.dumps(c)
            return hashlib.sha1(t.encode()).hexdigest()[:12]
    return "none"


def stream_cost(by_conv):
    cr = cw = 0
    for texts in by_conv.values():
        prev = ""
        for t in texts:
            cp = cpfx(prev, t) if prev else 0
            cr += cp; cw += max(0, toks(t) - cp); prev = t
    return cr * CREAD + cw * CWRITE, cr, cw


def start(config, base, token):
    subprocess.run(f"pkill -x {os.path.basename(BIN)}", shell=True); time.sleep(1)
    env = dict(os.environ, ANTHROPIC_UPSTREAM=base, ANTHROPIC_API_KEY=token,
               OPENAI_UPSTREAM=base, OPENAI_API_KEY=token, LISTEN_ADDR=f":{PORT}",
               INJECT_EXPAND="auto", CACHE_MODE="auto",
               CHEAP_MODEL="aws/claude-haiku-4-5", CHEAP_MODEL_PROVIDER="anthropic",
               CHEAP_MODEL_BASE=base, CHEAP_MODEL_KEY=token, CHEAP_MODEL_AUTH="bearer")
    cfgp = f"/tmp/cg-runs/cfg-{config}.yaml"
    Path(cfgp).write_text(swb.CUSTOM_CONFIGS[config] if config in swb.CUSTOM_CONFIGS else f"pipeline: [{config}]\n")
    p = subprocess.Popen([BIN, "--config", cfgp], cwd=str(CG),
                         stdout=open("/tmp/cg-runs/measure-proxy.log", "w"), stderr=subprocess.STDOUT,
                         env=env, preexec_fn=os.setsid)
    for _ in range(30):
        try: urllib.request.urlopen(f"http://localhost:{PORT}/healthz", timeout=3).read(); return p
        except Exception: time.sleep(0.5)
    raise RuntimeError("proxy did not come up")


CACHE = "auto"  # set from --cache; passed to /compact ?cache= to A/B cache-aware vs legacy


def rewrite(body, prov):
    data = json.dumps(body).encode()
    req = urllib.request.Request(f"http://localhost:{PORT}/compact?provider={prov}&cache={CACHE}", data=data, headers={"content-type": "application/json"})
    return json.loads(urllib.request.urlopen(req, timeout=180).read())


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--config", default="codesmart")
    ap.add_argument("--label", default="")
    ap.add_argument("--cache", default="auto", help="auto|on|off — cache-aware mode for /compact")
    a = ap.parse_args()
    global CACHE
    CACHE = a.cache
    base, token = cgenv.gateway()
    p = start(a.config, base, token)
    t0 = time.time()
    # baseline (as-sent) and config (rewritten) cost per capture, and replay for /stats
    base_by, cfg_by = {}, {}
    for name, path in CAPS.items():
        recs = [json.loads(l) for l in open(path)]
        # group by conv, turn order
        from collections import defaultdict
        g = defaultdict(list)
        for r in recs: g[(name, conv_key(r["body"]))].append(r)
        for k in g: g[k].sort(key=lambda r: len(r["body"].get("messages", [])))
        for k, rs in g.items():
            base_by[k] = [content_text(r["body"]) for r in rs]
            outs = []
            for r in rs:
                try: outs.append(content_text(rewrite(r["body"], r.get("provider", "anthropic"))))
                except Exception: outs.append(content_text(r["body"]))
            cfg_by[k] = outs
    replay_s = time.time() - t0
    st = json.loads(urllib.request.urlopen(f"http://localhost:{PORT}/stats", timeout=5).read())
    subprocess.run(f"pkill -x {os.path.basename(BIN)}", shell=True)

    bcost, bcr, bcw = stream_cost(base_by)
    ccost, ccr, ccw = stream_cost(cfg_by)
    dpct = 100 * (ccost - bcost) / bcost if bcost else 0
    content_pct = 100 * (1 - (ccr + ccw) / max(bcr + bcw, 1))

    out = {
        "label": a.label, "config": a.config,
        "requests": st["requests"], "savings_pct_perreq": round(st["savings_pct"], 2),
        "cost_delta_pct_cached": round(dpct, 2), "content_reduction_pct": round(content_pct, 2),
        "llm_calls": st["llm_calls"], "llm_in": st["llm_input_tokens"], "llm_out": st["llm_output_tokens"],
        "cg_added_ms_avg": round(st.get("cg_added_ms_avg", 0), 2),
        "components": {k: {"runs": v["runs"], "acted": v["acted"], "cum": v["saved_tokens"],
                            "uniq": v.get("saved_tokens_unique", 0), "ratio": round(v.get("overcount_ratio", 0), 2),
                            "ms": round(v["duration_ms"], 0)}
                       for k, v in sorted(st["components"].items(), key=lambda kv: -kv[1].get("saved_tokens_unique", 0))
                       if v["saved_tokens"] or v["acted"]},
    }
    print(json.dumps(out, indent=1))
    Path(f"/tmp/cg-runs/measure-{a.label or a.config}.json").write_text(json.dumps(out, indent=1))
    print(f"\n[replay {replay_s:.0f}s]  content -{content_pct:.1f}%  cost(cached) {dpct:+.1f}%  llm_calls={st['llm_calls']}")


if __name__ == "__main__":
    main()
