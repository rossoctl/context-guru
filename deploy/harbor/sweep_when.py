#!/usr/bin/env python3
"""WHEN did the sweep fire? Distribution of context pressure at every sweep decision.

The open question is whether the econ trigger fires TOO EARLY -- paying to ask while the request is
still small -- and whether a pressure floor would help or would disable the component. That argument
turns entirely on this distribution, and n=1 is not evidence, so this reads every arm-seed of a run
rather than one.

Reads cg.sweep.econ (both triggers' econ decisions, fired AND declined) and cg.sweep.preexpiry
(trigger one's firings, which carry no econ row). Usage:
    sweep-when.py ~/cg-loca/i022log-*.jsonl
"""
import json, sys, glob, collections, statistics

BUCKETS = [(0, .1), (.1, .2), (.2, .3), (.3, .4), (.4, .5),
           (.5, .6), (.6, .7), (.7, .8), (.8, .9), (.9, 1.01)]


def bucket(p):
    for lo, hi in BUCKETS:
        if lo <= p < hi:
            return f"{int(lo*100):>3}-{int(hi*100):<3}%"
    return "  >=100%"


def main(paths):
    rows = []
    for path in paths:
        for line in open(path, errors="replace"):
            if not line.strip().startswith("{"):
                continue
            try:
                d = json.loads(line)
            except Exception:
                continue
            m = d.get("msg")
            if m == "cg.sweep.econ":
                rows.append(("econ_fire" if d.get("decision") else "econ_decline", d, path))
            elif m == "cg.sweep.preexpiry":
                rows.append(("preexpiry_fire", d, path))
    if not rows:
        print("no sweep decisions found in", len(paths), "file(s)")
        return 1

    # ABSENT IS NOT ZERO. A log written by a binary from before the pressure field existed has no
    # `pressure` key, and defaulting that to 0.0 reports every decision in the 0-10% bucket -- a
    # confident, entirely fabricated distribution that looks exactly like "it always fires early".
    # Refuse instead: the whole purpose of this script is that distribution.
    missing = [d for _, d, _ in rows if "pressure" not in d]
    if missing:
        print(f"REFUSING TO REPORT: {len(missing)} of {len(rows)} decisions carry no `pressure` "
              f"field.\nThese logs predate it -- rebuild the proxy and re-run. Reporting them as "
              f"0% would invent the answer this script exists to measure.")
        return 2

    print(f"{len(rows)} sweep decisions across {len(paths)} arm-seed log(s)\n")
    bykind = collections.Counter(k for k, _, _ in rows)
    for k, n in bykind.most_common():
        print(f"  {k:<16} {n}")

    print("\npressure at decision (share of the model window the request occupied)")
    print(f"  {'bucket':<12} {'fired':>7} {'declined':>9}")
    grid = collections.defaultdict(lambda: [0, 0])
    for kind, d, _ in rows:
        b = bucket(d["pressure"])
        g = grid[b]
        if kind == "econ_decline":
            g[1] += 1
        else:
            g[0] += 1
    for lo, hi in BUCKETS:
        b = f"{int(lo*100):>3}-{int(hi*100):<3}%"
        if b in grid:
            f, dec = grid[b]
            print(f"  {b:<12} {f:>7} {dec:>9}")

    fired = [d["pressure"] for k, d, _ in rows if k != "econ_decline"]
    if fired:
        fired.sort()
        print(f"\nfired at pressure: min={fired[0]:.3f} p50={statistics.median(fired):.3f} "
              f"p90={fired[int(len(fired)*.9)-1]:.3f} max={fired[-1]:.3f}")
        for thr in (0.3, 0.5, 0.7):
            blocked = sum(1 for p in fired if p < thr)
            print(f"  a {thr:.0%} floor would have blocked {blocked}/{len(fired)} firings "
                  f"({blocked/len(fired):.0%})")

    # WHY the declines happened, since "not enough turns" and "the ask cannot repay" are different
    # findings and the whole point of splitting them was to be able to read this.
    dec = [d for k, d, _ in rows if k == "econ_decline"]
    if dec:
        ask = sum(1 for d in dec if d.get("askDeclined"))
        print(f"\ndeclines: {len(dec)} total | ask could not repay: {ask} | "
              f"rewrite could not repay: {len(dec)-ask}")
    est = [d for k, d, _ in rows if k != "preexpiry_fire"]
    warm = sum(1 for d in est if not d.get("estFromMeasurement"))
    if est:
        print(f"econ decisions on the PRIOR (warm-up): {warm}/{len(est)} "
              f"({warm/len(est):.0%}) -- these are priced optimistically by construction")
    return 0


if __name__ == "__main__":
    args = sys.argv[1:]
    paths = [p for a in args for p in glob.glob(a)] or glob.glob(
        "/home/vpcuser/cg-loca/i022log-*.jsonl")
    sys.exit(main(paths))
