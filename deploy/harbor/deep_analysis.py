#!/usr/bin/env python3
"""Deep three-way analysis: baseline vs context-guru vs headroom on SWE-bench Verified.
Computes EVERY dimension per-task, aggregated, and per-component, and emits one JSON
(deep_analysis.json) that the plotting + doc scripts consume.

Dimensions: reward, steps, cache_read, cache_write, fresh_input, output, cache-aware
billed cost (+ decomposition), cache-hit rate, agent wall, tool's own LLM cost, added
latency, tokens removed (cumulative + unique), per-component / per-strategy savings.

Usage: deep_analysis.py --out /tmp/cg-runs/deep
"""
import argparse, json, subprocess
from pathlib import Path
from statistics import mean

IN, OUT, CREAD, CWRITE = 2e-6, 10e-6, 0.2e-6, 2.5e-6
CG = Path(__file__).resolve().parents[2]   # this checkout

SRC = {
    "baseline":     ("/tmp/cg-runs/final50/rows-off.json",            None),
    "context-guru": ("/tmp/cg-runs/final50-v6/rows-codesmart.json",   "/tmp/cg-runs/final50-v6/summary.json"),
    "headroom":     ("/tmp/hd-runs/swe50/rows-hd-cache.json",         None),
    # rtk (Rust Token Killer): in-container bash-output compression via a Claude
    # Code PreToolUse hook. A 4th arm; its rows carry an extra `rtk` sub-dict with
    # rtk's own bash-output savings ledger. Optional — only included if present, so
    # the published 3-way matched set is unchanged unless the rtk run is supplied.
    "rtk":          ("/tmp/rtk-runs/swe50/rows-rtk.json",             "/tmp/rtk-runs/swe50/summary.json"),
}
# Drop arms whose rows file is missing so the script still runs pre-rtk-run.
SRC = {k: v for k, v in SRC.items() if Path(v[0]).exists()}


def billed(r):
    return (r.get("fresh_input", 0) * IN + r.get("cache_read", 0) * CREAD +
            r.get("cache_write", 0) * CWRITE + r.get("completion_tokens", 0) * OUT)


def load(p):
    return {r["task"]: r for r in json.load(open(p))}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", default="/tmp/cg-runs/deep")
    a = ap.parse_args()
    Path(a.out).mkdir(parents=True, exist_ok=True)

    rows = {k: load(v[0]) for k, v in SRC.items()}
    # tasks scored (no exception, reward not None) in ALL three
    common = sorted(t for t in rows["baseline"]
                    if all(t in rows[c] and not rows[c][t].get("exception")
                           and rows[c][t].get("reward") is not None for c in rows))
    configs = list(SRC)

    # per-task records
    per_task = []
    for t in common:
        rec = {"task": t.split("__")[-1]}
        for c in configs:
            r = rows[c][t]
            rec[c] = dict(reward=int(r.get("reward") or 0), steps=r.get("steps", 0),
                          cache_read=r.get("cache_read", 0), cache_write=r.get("cache_write", 0),
                          fresh=r.get("fresh_input", 0), out=r.get("completion_tokens", 0),
                          cost=round(billed(r), 4), wall=r.get("agent_wall_s") or 0)
        per_task.append(rec)

    def agg(c, key):
        return sum(rows[c][t][{"cache_read": "cache_read", "cache_write": "cache_write",
                               "fresh": "fresh_input", "out": "completion_tokens",
                               "steps": "steps"}[key]] or 0 for t in common)

    # per-config aggregate over matched tasks
    aggregate = {}
    for c in configs:
        solved = sum(1 for t in common if (rows[c][t].get("reward") or 0) >= 1)
        cr, cw, fr = agg(c, "cache_read"), agg(c, "cache_write"), agg(c, "fresh")
        cost = sum(billed(rows[c][t]) for t in common)
        cachecost = cr * CREAD + cw * CWRITE + fr * IN
        aggregate[c] = dict(
            n=len(common), solved=solved, rate=round(solved / len(common), 3),
            mean_steps=round(mean(rows[c][t]["steps"] for t in common), 1),
            cache_read=cr, cache_write=cw, fresh=fr, out=agg(c, "out"),
            cache_hit=round(100 * cr / max(cr + cw + fr, 1), 2),
            billed_cost=round(cost, 2), input_cache_cost=round(cachecost, 2),
            cache_read_cost=round(cr * CREAD, 2), cache_write_cost=round(cw * CWRITE, 2),
            mean_wall_s=round(mean((rows[c][t].get("agent_wall_s") or 0) for t in common), 1),
        )

    # tool's own LLM cost + added latency + content% + per-component (context-guru)
    tool = {c: dict(llm_cost=0.0, added_ms=0.0, content_pct=0.0, bounces=0, per_component={}) for c in configs}
    cs = json.load(open(SRC["context-guru"][1]))["configs"]
    cg = next((x for x in cs if x.get("per_component")), cs[0])
    tool["context-guru"] = dict(
        llm_cost=cg.get("cg_llm_cost", 0), added_ms=round(cg.get("cg_added_ms_avg") or 0, 1),
        content_pct=cg.get("proxy_savings_pct", 0), bounces=cg.get("proxy_bounces", 0),
        per_component={k: {"cum": v.get("saved_tokens"), "runs": v.get("runs"),
                            "ms": round(v.get("duration_ms") or 0, 1)}
                       for k, v in (cg.get("per_component") or {}).items()})
    # unique per-component (context-guru) from the dump
    try:
        subprocess.run(["python3", str(CG / "deploy/harbor/dump_unique.py"),
                        "/tmp/cg-runs/dump-swebench-codesmart.jsonl", "--json", f"{a.out}/cg_unique.json"], check=False)
        uq = json.load(open(f"{a.out}/cg_unique.json"))
        tool["context-guru"]["unique"] = uq
    except Exception:
        pass
    # headroom per-strategy + tool metrics from its RESULTS.json
    try:
        hr = json.load(open("/tmp/hd-runs/RESULTS.json"))["headroom_hd_cache"]
        tool["headroom"] = dict(llm_cost=hr.get("added_llm_cost", 0),
                                added_ms=hr.get("proxy_overhead_avg_ms", 0),
                                content_pct=hr.get("content_savings_pct", 0), bounces=0,
                                per_strategy=hr.get("per_strategy_saved", {}),
                                saved_tokens=hr.get("saved_tokens"))
    except Exception:
        pass

    # rtk tool metrics: $0 own-LLM cost + deterministic; its native metric is
    # bash-OUTPUT bytes saved (bytes/4 estimate), a DIFFERENT denominator than the
    # proxies' whole-request content%, so it is reported separately. Reversibility
    # is a tee-file on failure (no expand/retrieve bounce), so bounces=0.
    if "rtk" in configs:
        rrows = json.load(open(SRC["rtk"][0]))
        bycmd = {}
        for r in rrows:
            for c in ((r.get("rtk") or {}).get("by_command") or []):
                name = c.get("command") or c.get("name") or "?"
                key = " ".join(str(name).split()[:2])  # e.g. "rtk pytest"
                e = bycmd.setdefault(key, {"count": 0, "saved": 0})
                e["count"] += c.get("count") or 0
                e["saved"] += c.get("saved") or c.get("saved_tokens") or 0
        try:
            rs = next(x for x in json.load(open(SRC["rtk"][1]))["configs"])
        except Exception:
            rs = {}
        # Prefer the per-command aggregate built from the rtk `gain --history` "By
        # Command" tables (the --format json output has no per-command array); fall
        # back to whatever the rows carried.
        try:
            bc = json.load(open("/tmp/rtk-runs/rtk_by_command.json"))
            bycmd = {k: {"count": v["count"], "saved": v["saved"]} for k, v in bc.items()}
        except Exception:
            pass
        tool["rtk"] = dict(
            llm_cost=0.0, added_ms=0.0, bounces=0,
            content_pct=rs.get("rtk_bash_savings_pct"),  # NB: bash-output denom, not whole-request
            bash_tokens_before=rs.get("rtk_bash_tokens_before"),
            bash_tokens_after=rs.get("rtk_bash_tokens_after"),
            bash_tokens_saved=rs.get("rtk_bash_tokens_saved"),
            commands=rs.get("rtk_commands"),
            per_command=dict(sorted(bycmd.items(), key=lambda x: -x[1]["saved"])))

    out = dict(matched_tasks=len(common), configs=configs, aggregate=aggregate,
               tool=tool, per_task=per_task)
    Path(f"{a.out}/deep_analysis.json").write_text(json.dumps(out, indent=1))
    # console summary
    print(f"matched {len(common)} tasks (scored+no-exception in all 3)\n")
    hdr = f"{'metric':<22}" + "".join(f"{c:>16}" for c in configs)
    print(hdr); print("-" * len(hdr))
    for m, fn in [("solved", lambda x: f"{x['solved']}/{x['n']}"), ("rate", lambda x: f"{x['rate']:.0%}"),
                  ("mean_steps", lambda x: x["mean_steps"]), ("billed_cost $", lambda x: f"${x['billed_cost']}"),
                  ("cache_read (M)", lambda x: f"{x['cache_read']/1e6:.1f}"),
                  ("cache_write (M)", lambda x: f"{x['cache_write']/1e6:.2f}"),
                  ("cache_read_cost $", lambda x: f"${x['cache_read_cost']}"),
                  ("cache_write_cost $", lambda x: f"${x['cache_write_cost']}"),
                  ("cache_hit %", lambda x: x["cache_hit"]), ("mean_wall_s", lambda x: x["mean_wall_s"])]:
        print(f"{m:<22}" + "".join(f"{str(fn(aggregate[c])):>16}" for c in configs))
    print(f"\n{'tool llm $':<22}" + "".join(f"{str(tool[c]['llm_cost']):>16}" for c in configs))
    print(f"{'added ms/req':<22}" + "".join(f"{str(tool[c]['added_ms']):>16}" for c in configs))
    print(f"{'content %':<22}" + "".join(f"{str(tool[c]['content_pct']):>16}" for c in configs))
    print(f"\nwrote {a.out}/deep_analysis.json")


if __name__ == "__main__":
    main()
