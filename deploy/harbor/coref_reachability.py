#!/usr/bin/env python3
"""Is the deferral prize reachable? Reframed so it does not depend on measuring usage.

A Claude Code transcript does NOT record request boundaries, and its parentUuid graph is too
fragmented here to reconstruct the active branch (chains collapse to 5-78 entries out of
thousands). So absolute pre-compaction request size is NOT measurable from this corpus, and
any figure derived from a linear read spans multiple context windows. Measured duplicate
tool-output mass is only 3% pooled / 2% median, so SHARE-based results are unaffected --
but absolute sizes are not recoverable.

The reframe removes the dependency. At the moment the agent compacts, usage IS the threshold
by definition -- that is what triggered it. So if the pass fires AT the crossing rather than
after overshooting it:

    required cut  ~=  growth_per_turn * headroom_turns        (the deficit term is ~0)

which needs only growth per turn, and that IS robustly measurable. The deficit term in
docs/results/coref-density.md (7.3% of the request at H=0) was an artifact of firing late --
specifically of cc_capture.py segmenting at 180k, which puts the measurement point past the
crossing by construction.
"""
import json, glob, os, sys
TOK = lambda s: max(1, len(s) // 4)
WINDOW, MAXOUT = 200_000, 20_000
THRESH = WINDOW - MAXOUT - 13_000
# Measured available cut as a share of the request, from docs/results/coref-density.md
# (interactive corpus, opaque class and opportunity floor both in force).
AVAIL = {"unreferenced": 4.4, "unreferenced+closed": 9.6}


def load(path):
    out = []
    for l in open(path, errors="replace"):
        try:
            e = json.loads(l)
        except ValueError:
            continue
        if (e.get("type") in ("user", "assistant") and not e.get("isSidechain")
                and (e.get("message") or {}).get("content") is not None):
            out.append(e)
    return out


def sz(e):
    c = (e.get("message") or {}).get("content")
    bs = ([{"type": "text", "text": c}] if isinstance(c, str)
          else [b for b in c if isinstance(b, dict)] if isinstance(c, list) else [])
    n = 0
    for b in bs:
        t = b.get("type")
        if t == "text":
            n += TOK(b.get("text", ""))
        elif t == "thinking":
            n += TOK(b.get("thinking", ""))
        elif t == "tool_use":
            n += TOK(b.get("name", "") + json.dumps(b.get("input", {})))
        elif t == "tool_result":
            rc = b.get("content")
            s = (rc if isinstance(rc, str) else
                 "".join(x.get("text", "") for x in rc if isinstance(x, dict))
                 if isinstance(rc, list) else (json.dumps(rc) if rc is not None else ""))
            n += TOK(s)
    return n


def main():
    rows = []
    for p in sorted(glob.glob(os.path.expanduser("~/.claude/projects/*/*.jsonl"))):
        E = load(p)
        if not E:
            continue
        turns = sum(1 for e in E if e.get("type") == "assistant")
        rows.append(dict(name=os.path.basename(p)[:8], turns=turns,
                         tok=sum(sz(e) for e in E),
                         events=sum(1 for e in E if e.get("isCompactSummary") is True)))
    comp = [r for r in rows if r["events"]]
    print(f"main-conversation transcripts: {len(rows)}  (subagent transcripts excluded: "
          f"separate conversations with their own context)")
    print(f"\nSESSION-LEVEL REACHABILITY -- did the agent ever compact itself?")
    for lo, label in ((0, "all sessions"), (50, ">=50 model turns"),
                      (100, ">=100 model turns"), (200, ">=200 model turns")):
        s = [r for r in rows if r["turns"] >= lo]
        c = [r for r in s if r["events"]]
        if s:
            print(f"  {label:22s} {len(c):3d}/{len(s):<3d} ({100*len(c)//len(s):3d}%)   "
                  f"events={sum(r['events'] for r in c)}")
    print(f"\n  => the prize does not exist in the great majority of sessions. It is a")
    print(f"     long-session feature, and even among the longest it is under a third.")

    g = sorted(r["tok"] / max(1, r["turns"]) for r in comp if r["turns"] >= 10)
    if not g:
        return 1
    med = g[len(g) // 2]
    print(f"\nGROWTH per model turn, in the {len(g)} sessions that actually compacted:")
    print(f"  min {g[0]:.0f}   median {med:.0f}   max {g[-1]:.0f} tok/turn")
    print(f"  (robust to the branch problem: it is a ratio of two quantities inflated alike)")

    print(f"\nREQUIRED CUT if the pass fires AT the crossing (deficit ~0), against a "
          f"{THRESH:,}-token request:")
    print(f"  {'headroom':>10s}  {'required':>10s}  {'as share':>9s}   "
          f"{'unref 4.4%':>11s}  {'+closed 9.6%':>13s}")
    for H in (0, 20, 40, 60, 80):
        req = med * H
        share = 100 * req / THRESH
        u = "yes" if AVAIL["unreferenced"] >= share else "no"
        c = "yes" if AVAIL["unreferenced+closed"] >= share else "no"
        print(f"    {('H = '+str(H)):>10s}  {req:9,.0f}  {share:8.1f}%   {u:>11s}  {c:>13s}")
    # SENSITIVITY, and it is load-bearing. docs/results/coref-density.md reports ~514
    # tok/turn from a different estimator (request-size deltas inside a 180k segment) where
    # this uses total content / model turns. The verdict at H=40 FLIPS between them, so the
    # honest answer is that it depends on an estimator not yet pinned down.
    print(f"\nSENSITIVITY to the growth estimator (the conclusion depends on it):")
    print(f"  {'growth':>22s}  {'H=20':>7s}  {'H=40':>7s}  {'H=60':>7s}")
    for gv, label in ((med, f"{med:.0f} (this script)"), (514, "514 (density doc)")):
        cells = []
        for H in (20, 40, 60):
            share = 100 * gv * H / THRESH
            cells.append(("u+c" if AVAIL["unreferenced+closed"] >= share
                          else "no") if AVAIL["unreferenced"] < share else "unref")
        print(f"  {label:>22s}  " + "  ".join(f"{c:>7s}" for c in cells))
    print(f"  ('unref' = the shipped default suffices; 'u+c' = needs cut_closed, which ships")
    print(f"   OFF; 'no' = neither can supply it.)")

    print(f"\nWHAT IS ROBUST vs WHAT IS NOT:")
    print(f"  ROBUST  -- the prize exists in only {100*len(comp)//len(rows)}% of sessions "
          f"({len(comp)}/{len(rows)}), and under a third of the longest.")
    print(f"  ROBUST  -- firing AT the crossing removes the deficit term entirely, worth")
    print(f"            7.3 percentage points of required cut vs the density doc's late-fire")
    print(f"            measurement. That is arithmetic, not an estimate.")
    print(f"  NOT     -- whether 40 turns of headroom is affordable. It flips on the growth")
    print(f"            estimator above. Pinning that down needs request-level data this")
    print(f"            corpus cannot give (no recorded request boundaries).")
    print(f"  UNCHANGED -- the 11% false-drop from the selection experiment applies to every")
    print(f"            'yes' in these tables.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
