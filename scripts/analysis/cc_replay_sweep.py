"""Replay Claude Code transcripts under every zero-cost component and preset, price each run, and search for the best custom pipeline.
usage: python3 -I cc_replay_sweep.py --bin cg-transcript-replay --req REQ_DIR --work WORK_DIR [--rates IN,OUT,READ,WRITE] [--json]
REQ_DIR holds the files cc_transcript_requests.py wrote. Every run is offline: no component here calls a model.

Pricing, per call k with d_k tokens removed from the request: the cached prefix shrinks by d_{k-1}, so cache read drops by
d_{k-1} and cache write by d_k - d_{k-1}. A call whose forwarded prefix differs from the previous forwarded body re-writes
the changed part: those tokens move from cache read to cache write. Output tokens are unchanged, because an offline replay
cannot show how the agent would have behaved. Expand round-trips (the agent asking for an offloaded original) are not counted."""
import argparse, collections, glob, json, os, subprocess, sys
from concurrent.futures import ThreadPoolExecutor

# Components that never call a model. The ones left out call one (agentdiet, summarize, cache_aware_summarizer,
# extract_llm, extract_llm_sweep), need a build tag (skeleton), or act only on `system`/`tools`, which a transcript
# does not carry (cachesplit, cacheinject, toolschema, toolfilter).
FREE = ["format", "textclean", "searchfold", "dedup", "toon", "cmdfilter", "failed_run", "extract", "linecap",
        "smartcrush", "collapse", "mask", "coref", "readlifecycle"]
# Non-default settings worth pricing: the knobs whose default is off on cache grounds, and the size threshold of the
# one lossy component that touches whole files. Reported next to the single components; not used in the search.
VARIANTS = {
    "readlifecycle stale_at_depth": "pipeline: [readlifecycle]\ncomponents:\n  readlifecycle:\n    stale_at_depth: true\n",
    "readlifecycle bash_edits": "pipeline: [readlifecycle]\ncomponents:\n  readlifecycle:\n    bash_edits: true\n",
    "mask cold_cache off": "pipeline: [mask]\ncomponents:\n  mask:\n    cold_cache: false\n",
    "collapse max_tokens 4000": "pipeline: [collapse]\ncomponents:\n  collapse:\n    max_tokens: 4000\n",
    "collapse max_tokens 8000": "pipeline: [collapse]\ncomponents:\n  collapse:\n    max_tokens: 8000\n",
}
PRESETS = ["conservative", "safe", "balanced", "coding", "codesafe"]
# The order components run in when combined: lossless repacking first, then noise removal, then the age and
# relevance offloaders, as the shipped presets order them.
ORDER = {c: i for i, c in enumerate(FREE)}


def run(args, label, flag, value):
    out = os.path.join(args.work, label)
    reqs = sorted(glob.glob(os.path.join(args.req, "*.json")))
    if os.path.isfile(os.path.join(out, "done")):  # a finished run is reused, so a re-run only pays for new pipelines
        return price(args, out)
    os.makedirs(out, mode=0o700, exist_ok=True)
    def one(r):
        subprocess.run([args.bin, flag, value, "-out", out, r], check=True, stdout=subprocess.DEVNULL)
    with ThreadPoolExecutor(args.jobs) as ex:
        list(ex.map(one, reqs))
    open(os.path.join(out, "done"), "w").close()
    return price(args, out)


def price(args, out):
    ri, ro, rr, rw = args.rates
    usd = lambda c: (c["i"] * ri + c["o"] * ro + c["r"] * rr + c["w"] * rw) / 1e6
    tot = collections.Counter()
    comp, kind, edited = collections.Counter(), collections.Counter(), collections.Counter()
    rounds = []
    for f in sorted(glob.glob(os.path.join(out, "*.jsonl"))):
        name = os.path.basename(f)[:-6]
        req = json.load(open(os.path.join(args.req, name + ".json")))
        before, after = collections.Counter(), collections.Counter()
        pd = breaks = 0
        brk_usd = 0.0
        for c, line in zip(req["calls"], open(f)):
            x = json.loads(line)
            u = c["usage"]
            I, O = u.get("input_tokens", 0), u.get("output_tokens", 0)
            R, W = u.get("cache_read_input_tokens", 0), u.get("cache_creation_input_tokens", 0)
            d = x["orig_tok"] - x["out_tok"]
            lost = x["prev_out_tok"] - x["common_tok"] if x["broken"] else 0
            if x["broken"]:
                breaks += 1
                brk_usd += lost * (rw - rr) / 1e6
            r2 = max(R - pd - lost, 0)
            w2 = max(W + lost - (d - pd), 0)
            before.update(i=I, o=O, r=R, w=W)
            after.update(i=I, o=O, r=r2, w=w2)
            pd = d
            for k, v in (x.get("by_comp") or {}).items():
                comp[k] += v
            for tid, v in (x.get("by_tool") or {}).items():
                t = req["tools"].get(tid, {})
                kind[t.get("kind", "other")] += v
                if t.get("edited_later"):
                    edited[t.get("kind", "other")] += v
        last = json.loads(line)
        ub, ua = usd(before), usd(after)
        rounds.append(dict(round=name, removed_end=last["orig_tok"] - last["out_tok"], read_before=before["r"], read_after=after["r"],
                           write_before=before["w"], write_after=after["w"], usd_before=ub, usd_after=ua, breaks=breaks, break_usd=brk_usd))
        tot.update(usd_before=ub, usd_after=ua, read_before=before["r"], read_after=after["r"], write_before=before["w"],
                   write_after=after["w"], breaks=breaks, break_usd=brk_usd, removed_end=last["orig_tok"] - last["out_tok"])
    tot = dict(tot)
    tot["saving_usd"] = tot["usd_before"] - tot["usd_after"]
    tot["saving_pct"] = 100 * tot["saving_usd"] / tot["usd_before"] if tot["usd_before"] else 0.0
    removed = sum(kind.values())
    tot["edited_later_pct"] = max(100 * sum(edited.values()) / removed, 0.0) if removed > 0 else 0.0
    return dict(total=tot, rounds=rounds, by_component=dict(comp), by_tool_kind=dict(kind), edited_later=dict(edited))


def pipeline(comps): return ",".join(sorted(comps, key=ORDER.get))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--bin", required=True)
    ap.add_argument("--req", required=True)
    ap.add_argument("--work", required=True)
    ap.add_argument("--rates", default="5,25,0.5,6.25", help="USD per million tokens: input,output,cache read,cache write")
    ap.add_argument("--jobs", type=int, default=8)
    ap.add_argument("--json", action="store_true")
    args = ap.parse_args()
    args.rates = [float(v) for v in args.rates.split(",")]
    res = {"presets": {}, "single": {}, "greedy": []}
    for p in PRESETS:
        res["presets"][p] = run(args, "preset-" + p, "-preset", p)
    for c in FREE:
        res["single"][c] = run(args, "single-" + c, "-pipeline", c)
    res["variants"] = {}
    for name, doc in VARIANTS.items():
        label = "variant-" + name.replace(" ", "-")
        path = os.path.join(args.work, label + ".yaml")
        with open(path, "w") as f:
            f.write(doc)
        res["variants"][name] = run(args, label, "-config", path)
    # Greedy forward search: keep adding the component that raises the saving most; stop when none adds
    # at least 0.01 percentage point. A component that breaks the cache shows up as a lower saving here.
    def greedy(pool, ok):
        chosen, best, steps = [], 0.0, []
        while pool:
            trials = {c: run(args, "greedy-" + pipeline(chosen + [c]).replace(",", "+"), "-pipeline", pipeline(chosen + [c])) for c in pool}
            trials = {c: r for c, r in trials.items() if ok(r)}
            if not trials:
                break
            c, r = max(trials.items(), key=lambda kv: kv[1]["total"]["saving_pct"])
            if r["total"]["saving_pct"] - best < 0.01:
                break
            chosen.append(c); pool.remove(c); best = r["total"]["saving_pct"]
            steps.append(dict(add=c, pipeline=pipeline(chosen), result=r))
        return steps
    helps = [c for c in FREE if res["single"][c]["total"]["saving_pct"] > 0]
    res["greedy"] = greedy(list(helps), lambda r: True)
    res["best"] = res["greedy"][-1] if res["greedy"] else None
    # Low risk: no cache break, and nothing removed that the agent edits later. An offline replay cannot show a
    # behaviour change, so this is the pipeline whose saving the replay can actually vouch for.
    # edited_later_pct is a percentage: the limit is 0.05% of removed tokens, which rounds to 0.0% in the report.
    LOW_RISK_EDITED_PCT = 0.05
    safe = lambda r: r["total"]["breaks"] == 0 and r["total"]["edited_later_pct"] < LOW_RISK_EDITED_PCT
    res["greedy_low_risk"] = greedy([c for c in helps if safe(res["single"][c])], safe)
    res["best_low_risk"] = res["greedy_low_risk"][-1] if res["greedy_low_risk"] else None
    if args.json:
        json.dump(res, sys.stdout, indent=1)
        return
    show = lambda label, r: print(f"{label:<44} {r['total']['saving_pct']:6.2f}%  ${r['total']['saving_usd']:8.2f}  "
                                  f"breaks {r['total']['breaks']:4d} (${r['total']['break_usd']:.2f})  "
                                  f"edited-later {r['total']['edited_later_pct']:5.1f}%")
    print("== shipped presets"); [show(p, r) for p, r in res["presets"].items()]
    print("== one component alone"); [show(c, r) for c, r in sorted(res["single"].items(), key=lambda kv: -kv[1]["total"]["saving_pct"])]
    print("== non-default settings"); [show(n, r) for n, r in res["variants"].items()]
    print("== greedy search"); [show("+ " + g["add"], g["result"]) for g in res["greedy"]]
    for key, title in (("best", "best custom pipeline"), ("best_low_risk", "best low-risk pipeline (0 breaks, <0.05% of removed tokens edited later)")):
        b = res[key]
        if not b:
            continue
        print(f"== {title}: [{b['pipeline']}]")
        show("result", b["result"])
        print("   by component:", b["result"]["by_component"])
        print("   by tool kind:", b["result"]["by_tool_kind"])


if __name__ == "__main__":
    main()
