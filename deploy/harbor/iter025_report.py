#!/usr/bin/env python3
"""Iteration 025 running report: per-seed gate funnel, and arm B paired against arm A.

Pairing follows Amendment 1: seed 1 uses iteration 025's own arm A (the drift-check pass); seeds 2-5
use iteration 024's recorded arm A, whose reuse the drift check licensed. Every pair says which
baseline it used, because "no large drift detected" is not "no drift".
"""
import json, os, glob, math

H = os.path.expanduser("~/cg-loca")
# Within each seed iteration 024 ran arm A first then arm B; verified against the st-*.json mtimes.
I024 = {1: "20260902_165256", 2: "20260902_180411", 3: "20260902_192455",
        4: "20260902_204737", 5: "20260902_212304"}
I025A_S1 = "20260908_111313"


def results_for(seed, stamp):
    d = glob.glob(os.path.join(H, "outputs", "*i024-64k-s%d*%s" % (seed, stamp)))
    if not d:
        return None
    p = os.path.join(d[0], "results.json")
    return json.load(open(p)) if os.path.exists(p) else None


def newest_b(seed):
    """iteration 025 arm B for a seed: today's dirs, excluding the seed-1 arm A pass."""
    ds = sorted(glob.glob(os.path.join(H, "outputs", "*i024-64k-s%d*2026090[78]_1[1-9]*" % seed)))
    ds = [x for x in ds if I025A_S1 not in x and "20260902" not in x]
    for x in reversed(ds):
        p = os.path.join(x, "results.json")
        if os.path.exists(p):
            return json.load(open(p))
    return None


def gate(tag):
    p = os.path.join(H, "st-i022-%s.json" % tag)
    if not os.path.exists(p):
        return None
    d = json.load(open(p))
    g = ((d.get("components") or {}).get("extract_llm_sweep") or {})
    ga, ev = g.get("gates") or {}, g.get("events") or {}
    sw = ((d.get("extract") or {}).get("by_component") or {}).get("extract_llm_sweep", {})
    f = ev.get("prefix_rewrite_repaid", 0)
    return dict(requests=d.get("requests"), fired=f,
                rw=ga.get("prefix_rewrite_not_repaid", 0), ask=ga.get("econ_ask_not_repaid", 0),
                inv=ga.get("sweep_inventory_below_min", 0), stash=d.get("stash_refused"),
                cost_source=sw.get("cost_source"), calls=sw.get("calls"),
                saved=sw.get("gross_saved_tokens"), lat=sw.get("avg_latency_ms"),
                spend=sw.get("extraction_cost_usd"))


def binom_two_sided(b, c):
    n = b + c
    if n == 0:
        return 1.0
    obs = abs(b - n / 2.0)
    return min(1.0, sum(math.comb(n, k) for k in range(n + 1)
                        if abs(k - n / 2.0) >= obs - 1e-12) / (2.0 ** n))


print("== gate funnel, arm B by seed ==")
print("| seed | reqs | decisions | fired | %fired | decl rewrite | decl ask | inv<min | cost_src | lat ms |")
print("|---|---|---|---|---|---|---|---|---|---|")
for s in range(1, 6):
    g = gate("i025Bs%d" % s)
    if not g:
        continue
    dec = g["fired"] + g["rw"] + g["ask"]
    print("| %d | %s | %d | %d | %.1f%% | %d | %d | %d | %s | %s |" % (
        s, g["requests"], dec, g["fired"], 100.0 * g["fired"] / dec if dec else 0,
        g["rw"], g["ask"], g["inv"], g["cost_source"],
        ("%.0f" % g["lat"]) if g["lat"] else "-"))
    if g["stash"]:
        print("   !! stash_refused = %s on seed %d — #188's path was NOT inert" % (g["stash"], s))

print("\n== reward: arm B paired against arm A ==")
b_tot = c_tot = 0
rows = []
for s in range(1, 6):
    B = newest_b(s)
    A = results_for(s, I025A_S1) if s == 1 else results_for(s, I024[s])
    src = "iter025 A (same-day)" if s == 1 else "iter024 A (reused)"
    if not (A and B):
        continue
    pa, pb = A["per_config"], B["per_config"]
    all_tasks = sorted(set(pa) & set(pb))
    # AN ERRORED TASK IS UNPAIRABLE, NOT A ZERO. iteration 024's arm A seed 2 errored on
    # NhlB2bAnalysisS2LEnv and carries avg_accuracy None; `or 0` would read that as "the baseline
    # scored nothing" and credit arm B with a win it did not earn. LOCA's own summary averages over
    # the 14 that ran (0.3571 = 5/14), so the harness already treats it as absent -- the pairing must
    # too. Dropped and counted, never defaulted.
    tasks = [t for t in all_tasks
             if pa[t].get("avg_accuracy") is not None and pb[t].get("avg_accuracy") is not None
             and not pa[t].get("error") and not pb[t].get("error")]
    dropped = len(all_tasks) - len(tasks)
    b = sum(1 for t in tasks if pa[t]["avg_accuracy"] > pb[t]["avg_accuracy"])
    c = sum(1 for t in tasks if pb[t]["avg_accuracy"] > pa[t]["avg_accuracy"])
    b_tot += b
    c_tot += c
    rows.append((s, src, len(tasks), dropped, A["summary"]["avg_accuracy"],
                 B["summary"]["avg_accuracy"], b, c,
                 A["summary"]["total_cost_usd"], B["summary"]["total_cost_usd"]))
print("| seed | baseline | n paired | dropped | acc A | acc B | B worse | B better | $A | $B |")
print("|---|---|---|---|---|---|---|---|---|---|")
for r in rows:
    print("| %d | %s | %d | %d | %.4f | %.4f | %d | %d | $%.2f | $%.2f |" % r)
tot_drop = sum(r[3] for r in rows)
if tot_drop:
    print("\n%d pair(s) dropped as unpairable (a task errored on one side). Counting an errored "
          "baseline task as 0 would credit arm B with wins it did not earn." % tot_drop)
if rows:
    n = b_tot + c_tot
    print("\npooled discordant pairs: B worse=%d  B better=%d  (n=%d)" % (b_tot, c_tot, n))
    print("exact two-sided sign test p = %.4f   [UNCLUSTERED — the registered test clusters by task]"
          % binom_two_sided(b_tot, c_tot))
    if n:
        hi = 1.0 - (0.025) ** (1.0 / n) if b_tot == 0 else None
        if hi:
            print("Clopper-Pearson 95%% upper bound on worsened proportion (0 of %d) = %.1f%%" % (n, hi * 100))
