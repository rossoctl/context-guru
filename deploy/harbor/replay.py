#!/usr/bin/env python3
"""Replay a captured request stream through each component config and report
per-component savings + format integrity — deterministically, with NO repeated
agent/LLM spend (uses the proxy's /compact endpoint, which runs the pipeline and
returns the rewritten body without forwarding upstream).

Input: a CONTEXT_GURU_CAPTURE jsonl ({provider, model, body}) recorded from a real
baseline agent run. Output: a per-config table (savings%, per-component saved, and a
format-integrity check that every rewritten body is still valid JSON with the same
messages/tool_result structure the next LLM call needs).

Usage: replay.py <capture.jsonl> [--configs off mask extract ...]
"""
import argparse, json, subprocess, sys, time, urllib.request
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import cgenv  # base URLs and credentials for both the hosted and the local deployment

CG = Path("/home/vpcuser/projects/context-engineering/context-guru")
BIN = "/tmp/cg-runs/cg-proxy-d1"
PORT = 4020

# deterministic components (no model needed); LLM ones (extract-code, summarize)
# are handled with a cheap model separately.
DET_CONFIGS = ["off", "format", "toon", "cacheinject", "dedup", "failed_run",
               "cmdfilter", "mask", "collapse", "extract", "smartcrush", "skeleton",
               "phi_evict", "agent", "balanced"]
PIPE = {
    "off": None, "agent": ("preset", "agent"), "balanced": ("preset", "balanced"),
    "general": ("preset", "general"), "coding": ("preset", "coding"), "mcp": ("preset", "mcp"),
    # The shipped defaults. They need the preset path, not a bare pipeline name-list:
    # their tuned per-component blocks live in config.presetConfigs, so replaying
    # `pipeline: [codesmart]` would fail to resolve a component and measure nothing.
    "codesmart": ("preset", "codesmart"), "codesafe": ("preset", "codesafe"),
}


def get(url):
    with urllib.request.urlopen(url, timeout=5) as r:
        return json.load(r)


def post(url, body_bytes):
    req = urllib.request.Request(url, data=body_bytes, headers={"content-type": "application/json"})
    with urllib.request.urlopen(req, timeout=60) as r:
        return r.read()


def start_proxy(config, base, token):
    subprocess.run(f"pkill -x {Path(BIN).name}", shell=True); time.sleep(1)
    import os
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
                         env=env, preexec_fn=__import__("os").setsid)
    for _ in range(30):
        try:
            get(f"http://localhost:{PORT}/healthz"); return p
        except Exception:
            time.sleep(0.5)
    return p


def struct_key(body):
    """A structural signature to detect format corruption across the rewrite:
    (#messages, #tool_result blocks). Offloaders must preserve both (only summarize
    changes message count)."""
    d = json.loads(body)
    msgs = d.get("messages", [])
    tr = 0
    for m in msgs:
        c = m.get("content")
        if isinstance(c, list):
            tr += sum(1 for b in c if isinstance(b, dict) and b.get("type") == "tool_result")
    return len(msgs), tr


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("capture")
    ap.add_argument("--configs", nargs="*", default=DET_CONFIGS)
    a = ap.parse_args()
    base, token = cgenv.gateway()
    recs = [json.loads(l) for l in open(a.capture) if l.strip()]
    print(f"replaying {len(recs)} captured requests through {len(a.configs)} configs\n")
    print(f"{'config':<12} {'saved%':>7} {'before':>9} {'after':>9} {'reqs_mut':>8} {'fmt_ok':>7}  per-component(saved_tokens)")
    rows = []
    for config in a.configs:
        start_proxy(config, base, token)
        fmt_ok = 0
        fmt_bad = []
        for rec in recs:
            body = json.dumps(rec["body"]).encode() if isinstance(rec["body"], (dict, list)) else rec["body"].encode()
            prov = rec.get("provider", "anthropic")
            try:
                out = post(f"http://localhost:{PORT}/compact?provider={prov}", body)
            except Exception as e:
                fmt_bad.append(f"post-err:{e}"); continue
            # format integrity: valid JSON + structure preserved (summarize may shrink #msgs)
            try:
                bk, ok = struct_key(body), struct_key(out)
                # tool_result count must never increase; for non-summarize, #msgs equal
                if ok[1] <= bk[1] and (config == "summarize" or ok[0] == bk[0]):
                    fmt_ok += 1
                else:
                    fmt_bad.append(f"struct {bk}->{ok}")
            except Exception as e:
                fmt_bad.append(f"json-err:{e}")
        st = get(f"http://localhost:{PORT}/stats")
        pc = {k: v["saved_tokens"] for k, v in st.get("components", {}).items() if v.get("saved_tokens")}
        mutated = sum(1 for v in st.get("components", {}).values() if v.get("mutated"))
        row = dict(config=config, pct=round(st.get("savings_pct", 0), 2),
                   before=st.get("tokens_before", 0), after=st.get("tokens_after", 0),
                   reqs=st.get("requests", 0), mutated=mutated, fmt_ok=fmt_ok,
                   n=len(recs), per_component=pc, fmt_bad=fmt_bad[:3])
        rows.append(row)
        print(f"{config:<12} {row['pct']:>7} {row['before']:>9} {row['after']:>9} "
              f"{'':>8} {fmt_ok}/{len(recs):<5}  {pc}", flush=True)
        if fmt_bad:
            print(f"             FORMAT ISSUES ({len(fmt_bad)}): {fmt_bad[:3]}", flush=True)
    subprocess.run(f"pkill -x {Path(BIN).name}", shell=True)
    Path("/tmp/cg-runs/replay-results.json").write_text(json.dumps(rows, indent=1))
    print("\nwrote /tmp/cg-runs/replay-results.json")


if __name__ == "__main__":
    main()
