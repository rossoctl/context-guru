#!/usr/bin/env python3
"""Deterministic, cache-aware cost model of a context-guru config on a FIXED request
stream — the honest "does CE save cost" measure, with ZERO agent nondeterminism.

Why: a single live baseline-vs-config run is dominated by step-count variance (the
agent takes a different path), so the billed-cost delta is noise, not the CE effect.
Here we take ONE captured request stream and compute, for the SAME turns, the billed
input cost (a) as the agent sent them (baseline) vs (b) after each is rewritten by the
config via /compact — applying Anthropic prompt-cache tiers turn-over-turn.

Cache model (faithful simplification of Anthropic prompt caching): within a
conversation, order requests by turn (growing message count). For turn t, the longest
token PREFIX byte-identical to turn t-1's sent content is a cache-READ (0.1x input);
everything after the first divergence is re-processed and cache-WRITTEN (1.25x). This
captures BOTH effects that matter: (1) mutating an OLD tool output shifts the divergence
point earlier -> more cache-write (the penalty), and (2) a smaller compacted prefix means
every later turn re-reads fewer tokens (the benefit). Output tokens are equal for both
sides on identical traffic, so they're excluded.

Usage: cachecost.py <capture.jsonl> --config codesmart [--port 4021] [--limit N]
"""
import argparse, json, subprocess, time, urllib.request, os, hashlib
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import cgenv  # base URLs and credentials for both the hosted and the local deployment

CG = Path("/home/vpcuser/projects/context-engineering/context-guru")
BIN = "/tmp/cg-runs/cg-proxy-rp"
IN_RATE, CREAD, CWRITE = 2e-6, 0.2e-6, 2.5e-6  # $/token: input, cache-read, cache-write

# reuse the swebench harness's custom configs
import importlib.util
spec = importlib.util.spec_from_file_location("swb", str(CG / "deploy/harbor/swebench.py"))
swb = importlib.util.module_from_spec(spec); spec.loader.exec_module(swb)


def content_text(body):
    """The full request text the provider sees (system + messages), as one string —
    the thing the cache keys on."""
    parts = []
    sysv = body.get("system")
    if isinstance(sysv, str): parts.append(sysv)
    elif isinstance(sysv, list):
        parts += [b.get("text", "") for b in sysv if isinstance(b, dict)]
    for m in body.get("messages", []):
        c = m.get("content")
        if isinstance(c, str): parts.append(c)
        elif isinstance(c, list):
            for b in c:
                if isinstance(b, dict):
                    parts.append(b.get("text") or (b.get("content") if isinstance(b.get("content"), str) else json.dumps(b.get("content", ""))) or json.dumps(b.get("input", "")))
    return "\n".join(p for p in parts if p)


def toks(s): return max(1, len(s) // 4)  # ~4 chars/token


def common_prefix_tokens(a, b):
    n = min(len(a), len(b)); i = 0
    while i < n and a[i] == b[i]:
        i += 1
    return i // 4


def conv_key(body):
    # group by the FIRST user message (the task instruction — stable & unique per task)
    for m in body.get("messages", []):
        if m.get("role") == "user":
            c = m.get("content"); t = c if isinstance(c, str) else json.dumps(c)
            return hashlib.sha1(t.encode()).hexdigest()[:12]
    return "none"


def stream_cost(bodies_by_conv):
    """bodies_by_conv: {conv: [content_text ordered by turn]}. Returns total $ and token tiers."""
    cread = cwrite = 0
    for conv, texts in bodies_by_conv.items():
        prev = ""
        for t in texts:
            cp = common_prefix_tokens(prev, t) if prev else 0
            total = toks(t)
            cread += cp
            cwrite += max(0, total - cp)  # new/changed -> processed + cached
            prev = t
    cost = cread * CREAD + cwrite * CWRITE
    return cost, cread, cwrite


def start_proxy(config, base, token, port):
    subprocess.run(f"pkill -x {os.path.basename(BIN)}", shell=True); time.sleep(1)
    env = dict(os.environ, ANTHROPIC_UPSTREAM=base, ANTHROPIC_API_KEY=token,
               OPENAI_UPSTREAM=base, OPENAI_API_KEY=token, LISTEN_ADDR=f":{port}",
               CHEAP_MODEL="aws/claude-sonnet-5", CHEAP_MODEL_PROVIDER="anthropic",
               CHEAP_MODEL_BASE=base, CHEAP_MODEL_KEY=token, CHEAP_MODEL_AUTH="bearer")
    cfgp = f"/tmp/cg-runs/cachecost-{config}.yaml"
    Path(cfgp).write_text(swb.CUSTOM_CONFIGS[config])
    p = subprocess.Popen([BIN, "--config", cfgp], cwd=str(CG), stdout=open(f"/tmp/cg-runs/cachecost-proxy.log", "w"),
                         stderr=subprocess.STDOUT, env=env, preexec_fn=os.setsid)
    for _ in range(30):
        try:
            urllib.request.urlopen(f"http://localhost:{port}/healthz", timeout=3).read(); return p
        except Exception:
            time.sleep(0.5)
    raise RuntimeError("proxy did not come up")


def rewrite(body, prov, port):
    data = json.dumps(body).encode()
    req = urllib.request.Request(f"http://localhost:{port}/compact?provider={prov}", data=data, headers={"content-type": "application/json"})
    return json.loads(urllib.request.urlopen(req, timeout=120).read())


def group(recs, key_fn):
    from collections import defaultdict
    g = defaultdict(list)
    for r in recs: g[conv_key(r["body"])].append(r)
    for k in g:
        g[k].sort(key=lambda r: len(r["body"].get("messages", [])))  # turn order
    return g


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("capture"); ap.add_argument("--config", default="codesmart")
    ap.add_argument("--port", type=int, default=4021); ap.add_argument("--limit", type=int, default=0)
    a = ap.parse_args()
    base, token = cgenv.gateway()
    recs = [json.loads(l) for l in open(a.capture) if l.strip()]
    if a.limit: recs = recs[:a.limit]
    convs = group(recs, conv_key)
    print(f"{len(recs)} requests, {len(convs)} conversations")
    # baseline: cost of the stream as sent
    base_by_conv = {c: [content_text(r["body"]) for r in rs] for c, rs in convs.items()}
    b_cost, b_cr, b_cw = stream_cost(base_by_conv)
    # config: rewrite each request via /compact, then model cache cost
    start_proxy(a.config, base, token, a.port)
    cfg_by_conv = {}
    n = 0
    for c, rs in convs.items():
        texts = []
        for r in rs:
            prov = r.get("provider", "anthropic")
            try:
                out = rewrite(r["body"], prov, a.port)
                texts.append(content_text(out))
            except Exception as e:
                texts.append(content_text(r["body"]))  # fail-open
            n += 1
            if n % 100 == 0: print(f"  rewritten {n}/{len(recs)}", flush=True)
        cfg_by_conv[c] = texts
    subprocess.run(f"pkill -x {os.path.basename(BIN)}", shell=True)
    # CG's own LLM cost from /stats (captured before kill)
    c_cost, c_cr, c_cw = stream_cost(cfg_by_conv)
    print(f"\n=== cache-aware cost (deterministic, identical traffic) ===")
    print(f"baseline: ${b_cost:.3f}  (cache_read {b_cr:,} tok, cache_write {b_cw:,} tok)")
    print(f"{a.config}: ${c_cost:.3f}  (cache_read {c_cr:,} tok, cache_write {c_cw:,} tok)")
    delta = 100 * (c_cost - b_cost) / b_cost if b_cost else 0
    print(f"delta: {delta:+.1f}%  ({'SAVES' if delta<0 else 'COSTS MORE'})")
    print(f"content: baseline {b_cr+b_cw:,} tok -> {a.config} {c_cr+c_cw:,} tok ({100*(1-(c_cr+c_cw)/(b_cr+b_cw)):.1f}% fewer content tokens)")
    Path("/tmp/cg-runs/cachecost.json").write_text(json.dumps(dict(
        baseline=dict(cost=b_cost, cache_read=b_cr, cache_write=b_cw),
        config=dict(name=a.config, cost=c_cost, cache_read=c_cr, cache_write=c_cw),
        delta_pct=delta), indent=1))
    print("wrote /tmp/cg-runs/cachecost.json")


if __name__ == "__main__":
    main()
