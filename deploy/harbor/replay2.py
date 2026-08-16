#!/usr/bin/env python3
"""Replay a captured request stream through each component config and report, per
component, the metrics the user asked for — token savings, DOLLAR cost saved,
per-request LATENCY added, and message-format integrity — deterministically and
with NO repeated agent/LLM spend (uses the proxy /compact endpoint, which runs the
pipeline and returns the rewritten body without forwarding upstream).

Beyond replay.py this adds:
  * cost: $ before/after/saved using the live litellm price for the agent model
    (input-token driven — the growing context is re-sent every turn, so input
    tokens dominate request cost; cache-read pricing noted separately).
  * time: wall-clock ms/request for the pipeline, and the delta vs `off` — the
    real latency each component adds on the hot path.
  * message diffs: for the first few MUTATED requests per config, writes the
    before/after bodies to /tmp/cg-runs/diffs/<config>/ so the applied changes can
    be inspected for correctness/over-aggression.

Usage: replay2.py <capture.jsonl> [--configs off mask ...] [--model aws/claude-sonnet-5] [--diffs 4]
"""
import argparse, json, os, subprocess, sys, time, urllib.request
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import cgenv  # base URLs and credentials for both the hosted and the local deployment

CG = Path("/home/vpcuser/projects/context-engineering/context-guru")
BIN = "/tmp/cg-runs/cg-proxy-rp"
PORT = 4021
PRICES_URL = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"

DET_CONFIGS = ["off", "format", "toon", "cacheinject", "dedup", "failed_run",
               "cmdfilter", "mask", "collapse", "extract", "smartcrush", "skeleton",
               "phi_evict", "general", "agent", "balanced"]
PIPE = {"off": None, "agent": ("preset", "agent"), "balanced": ("preset", "balanced"),
        "general": ("preset", "general"), "coding": ("preset", "coding"), "mcp": ("preset", "mcp")}


def get(url):
    with urllib.request.urlopen(url, timeout=5) as r:
        return json.load(r)


def post(url, body_bytes):
    req = urllib.request.Request(url, data=body_bytes, headers={"content-type": "application/json"})
    with urllib.request.urlopen(req, timeout=120) as r:
        return r.read()


def price_for(model):
    """Return (input_$/tok, output_$/tok, cache_read_$/tok) for model via the live
    litellm map; exact match then tail-substring; safe Sonnet-class fallback."""
    fallback = (2e-6, 1e-5, 2e-7)
    try:
        d = json.load(urllib.request.urlopen(PRICES_URL, timeout=15))
    except Exception as e:
        print(f"[price] fetch failed ({e}); using fallback {fallback}", file=sys.stderr)
        return fallback
    m = model.split("/")[-1].strip()
    cand = d.get(model) or d.get(m)
    if not cand:
        for k, v in d.items():
            if k.split("/")[-1] == m:
                cand = v
                break
    if not cand:
        for k, v in d.items():
            if m and m in k:
                cand = v
                break
    if not cand:
        print(f"[price] {model!r} not found; using fallback {fallback}", file=sys.stderr)
        return fallback
    return (cand.get("input_cost_per_token") or fallback[0],
            cand.get("output_cost_per_token") or fallback[1],
            cand.get("cache_read_input_token_cost") or fallback[2])


def start_proxy(config, base, token):
    subprocess.run("pkill -x cg-proxy-rp", shell=True)
    time.sleep(1)
    env = dict(os.environ, ANTHROPIC_UPSTREAM=base, ANTHROPIC_API_KEY=token,
               OPENAI_UPSTREAM=base, OPENAI_API_KEY=token, LISTEN_ADDR=f":{PORT}",
               CHEAP_MODEL="aws/claude-sonnet-5", CHEAP_MODEL_PROVIDER="anthropic",
               CHEAP_MODEL_BASE=base, CHEAP_MODEL_KEY=token, CHEAP_MODEL_AUTH="bearer")
    spec = PIPE.get(config, ("pipe", config))
    if config == "off":
        args = [BIN, "--preset", "off"]
    elif spec[0] == "preset":
        args = [BIN, "--preset", spec[1]]
    else:
        cfg = Path(f"/tmp/cg-runs/replay-{config}.yaml")
        cfg.write_text(f"pipeline: [{config}]\n")
        args = [BIN, "--config", str(cfg)]
    p = subprocess.Popen(args, cwd=str(CG), stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                         env=env, preexec_fn=os.setsid)
    for _ in range(30):
        try:
            get(f"http://localhost:{PORT}/healthz")
            return p
        except Exception:
            time.sleep(0.5)
    return p


def struct_key(body):
    """Structural signature to detect format corruption: (#messages, #tool_result
    blocks, #tool_use/tool_call blocks). Offloaders must not drop tool_result or
    tool_call blocks or orphan a tool_use (that breaks the next LLM call)."""
    d = json.loads(body)
    msgs = d.get("messages", [])
    tr = tu = 0
    for m in msgs:
        c = m.get("content")
        if isinstance(c, list):
            tr += sum(1 for b in c if isinstance(b, dict) and b.get("type") == "tool_result")
            tu += sum(1 for b in c if isinstance(b, dict) and b.get("type") == "tool_use")
        if isinstance(m.get("tool_calls"), list):
            tu += len(m["tool_calls"])
    return len(msgs), tr, tu


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("capture")
    ap.add_argument("--configs", nargs="*", default=DET_CONFIGS)
    ap.add_argument("--model", default="aws/claude-sonnet-5")
    ap.add_argument("--diffs", type=int, default=4, help="save N before/after diffs per config")
    ap.add_argument("--out", default="/tmp/cg-runs/replay2-results.json")
    a = ap.parse_args()
    base, token = cgenv.gateway()
    in_p, out_p, cache_p = price_for(a.model)
    recs = [json.loads(l) for l in open(a.capture) if l.strip()]
    tag = Path(a.capture).stem
    print(f"replaying {len(recs)} reqs ({tag}) x {len(a.configs)} configs | model={a.model} "
          f"in=${in_p*1e6:.2f}/M out=${out_p*1e6:.2f}/M\n")
    hdr = f"{'config':<11}{'saved%':>7}{'tok_saved':>10}{'$saved':>9}{'$/1k-req':>9}{'ms/req':>8}{'+ms':>7}{'fmt':>9}  per-component"
    print(hdr)
    print("-" * len(hdr))
    rows = []
    off_ms = None
    for config in a.configs:
        start_proxy(config, base, token)
        fmt_ok, fmt_bad = 0, []
        t_total, diffs_saved = 0.0, 0
        ddir = Path(f"/tmp/cg-runs/diffs/{tag}/{config}")
        for rec in recs:
            body = json.dumps(rec["body"]).encode() if isinstance(rec["body"], (dict, list)) else rec["body"].encode()
            prov = rec.get("provider", "anthropic")
            t0 = time.perf_counter()
            try:
                out = post(f"http://localhost:{PORT}/compact?provider={prov}", body)
            except Exception as e:
                fmt_bad.append(f"post-err:{e}")
                continue
            t_total += time.perf_counter() - t0
            try:
                bk, ok = struct_key(body), struct_key(out)
                # tool_result & tool_use counts must never drop; #msgs equal unless summarize
                if ok[1] <= bk[1] and ok[2] == bk[2] and (config == "summarize" or ok[0] == bk[0]):
                    fmt_ok += 1
                else:
                    fmt_bad.append(f"struct {bk}->{ok}")
                # save before/after for mutated requests (for message-quality review)
                if config != "off" and a.diffs and diffs_saved < a.diffs and len(out) != len(body):
                    ddir.mkdir(parents=True, exist_ok=True)
                    (ddir / f"req{diffs_saved}.before.json").write_bytes(body)
                    (ddir / f"req{diffs_saved}.after.json").write_bytes(out)
                    diffs_saved += 1
            except Exception as e:
                fmt_bad.append(f"json-err:{e}")
        st = get(f"http://localhost:{PORT}/stats")
        pc = {k: v["saved_tokens"] for k, v in st.get("components", {}).items() if v.get("saved_tokens")}
        n = len(recs)
        saved = st.get("saved_tokens", 0)
        ms = 1000.0 * t_total / max(n, 1)
        if config == "off":
            off_ms = ms
        dollars_saved = saved * in_p
        # projected $ per 1000 requests at this stream's mean savings
        before = st.get("tokens_before", 0)
        after = st.get("tokens_after", 0)
        per1k = ((before - after) / max(n, 1)) * in_p * 1000
        row = dict(config=config, pct=round(st.get("savings_pct", 0), 2), tok_saved=saved,
                   dollars_saved=round(dollars_saved, 5), per1k=round(per1k, 4),
                   ms_req=round(ms, 2), add_ms=round(ms - (off_ms or ms), 2),
                   before=before, after=after, reqs=st.get("requests", 0),
                   fmt_ok=fmt_ok, n=n, per_component=pc, fmt_bad=fmt_bad[:3])
        rows.append(row)
        print(f"{config:<11}{row['pct']:>7}{saved:>10}{'$'+format(dollars_saved,'.4f'):>9}"
              f"{'$'+format(per1k,'.2f'):>9}{row['ms_req']:>8}{row['add_ms']:>7}"
              f"{str(fmt_ok)+'/'+str(n):>9}  {pc}", flush=True)
        if fmt_bad:
            print(f"           !! FORMAT ISSUES ({len(fmt_bad)}): {fmt_bad[:3]}", flush=True)
    subprocess.run("pkill -x cg-proxy-rp", shell=True)
    Path(a.out).write_text(json.dumps(dict(model=a.model, prices=dict(input=in_p, output=out_p, cache_read=cache_p),
                                            capture=a.capture, n=len(recs), rows=rows), indent=1))
    print(f"\nwrote {a.out}  (diffs under /tmp/cg-runs/diffs/{tag}/)")


if __name__ == "__main__":
    main()
