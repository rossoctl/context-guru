#!/usr/bin/env python3
"""Iteration 028 readout: the cumulative mean difference, and the futility check.

WHAT THIS IS FOR. The run is stopped after every seed and a person decides whether to continue. This
prints what that decision needs and nothing else, in the order the preregistration fixed: validity
first, then the primary endpoint, then irreversible loss, then cost.

THE RULE, from PREREGISTRATION.md section 4, restated here so the two cannot drift:

  - Read the CUMULATIVE mean difference (arm A - baseline) over completed seeds, not the latest seed
    alone. A single seed's difference has a standard error of ~12.3 accuracy points and can only take
    values in multiples of 6.7 (one task of fifteen), so reacting to one seed reacts mostly to noise.
  - STOP FOR FUTILITY if the cumulative mean is below -6.7 points, i.e. behind by a whole task.
    Simulated at the measured noise: this abandons a true +13.3-point effect 3.2% of the time and
    catches a truly harmful arm 93.3% of the time, stopping at seed 1.6 on average.
  - The rule is FUTILITY-ONLY. It may end the run. It may NOT declare a win. Stopping early for lack of
    benefit does not inflate type-I error; looking, continuing because it looked good, and then testing
    at 0.05 does. The confirmatory two-sided test happens at seed 5 against an 11-point margin.
  - A pass that FAILED is not a futility signal. One of iteration 024's 19 passes scored 0.000. An
    invalid pass is excluded here and must be re-run, never pooled.
"""
import json, glob, os, re, sys, math, statistics as st

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import locaterm

H = os.path.expanduser("~/cg-loca")
FUTILITY = -100.0 / 15          # one task, in accuracy points
MARGIN = 11.0                   # the preregistered confirmatory margin
SE1 = 12.3                      # measured one-seed difference SE, accuracy points
# The three environments solved 0 times in 18 iteration-024 passes. Declared in the preregistration
# BEFORE this run, so the 12-environment secondary is not a post-hoc subgroup.
# Shared with perenv028.py via locaterm, because the same pass must not yield two different
# non-degenerate counts depending on which script read it.
DEGENERATE = locaterm.DEGENERATE


def pass_dir(tag):
    """The outputs directory for a tag, resolved from THAT PASS'S OWN LOG.

    NOT a glob over outputs/. LOCA names its directory after the TASK CONFIG and the agent model --
    `inf_claude_api_i024-64k-s1_aws-claude-sonnet-5_CTU_<ts>` -- and it carries no trace of the arm tag.
    So `outputs/*i028-s1-A*` matches nothing, which is exactly how seed 1's first readout announced "No
    iteration 028 passes on disk yet" while both passes sat there complete, validated and paid for.

    Globbing the TASK CONFIG name instead would be worse than the bug it fixes: `*i024-64k-s1*` matches
    every pass ever run on that seed -- baseline and arm A, sonnet and haiku, and iteration 024's own
    five passes on the same instances -- and a newest-wins tiebreak would then silently hand back
    whichever pass finished last. Both arms would resolve to the SAME directory and the difference would
    read as an exact zero, with every counter healthy.

    The pass's own log is the only artifact that ties a tag to a directory, because stage022.sh writes it
    per tag and LOCA prints the path it saved to. Absent or unparseable, this returns None and the caller
    reports the pass as missing -- it does not fall back to a guess.
    """
    log = os.path.join(H, "i022loca-%s.log" % tag)
    if not os.path.exists(log):
        return None
    with open(log, errors="replace") as fh:
        m = re.search(r"outputs/([^/\s]+)/tasks/", fh.read())
    if not m:
        return None
    d = os.path.join(H, "outputs", m.group(1))
    return d if os.path.isdir(d) else None


def read_pass(tag):
    d = pass_dir(tag)
    if not d:
        return None
    p = os.path.join(d, "results.json")
    if not os.path.exists(p):
        return None
    r = json.load(open(p))
    s = r.get("summary") or {}
    per = {}
    for env, v in (r.get("per_config") or {}).items():
        if isinstance(v, dict):
            a = v.get("accuracy", v.get("avg_accuracy"))
            if a is not None:
                per[env] = float(a)
    st_ = json.load(open(os.path.join(H, "st-i022-%s.json" % tag))) if \
        os.path.exists(os.path.join(H, "st-i022-%s.json" % tag)) else {}
    comp = (st_.get("components") or {}).get("extract_llm_sweep") or {}
    ev = comp.get("events") or {}
    return {
        "dir": os.path.basename(d),
        "accuracy": s.get("avg_accuracy"),
        "steps": s.get("avg_steps"),
        "cost": s.get("total_cost_usd"),
        "per": per,
        "acted": comp.get("acted") or 0,
        "adjudicated": ev.get("sweep_adjudicated", 0),
        "offered": ev.get("sweep_offered", 0),
        "saved": comp.get("saved_tokens") or 0,
        "unresolved": st_.get("model_info_unresolved"),
        "expand_missing": st_.get("expand_unresolved_missing"),
    }


def valid(p, arm):
    """Reasons a pass must be excluded rather than read. Returns a list of problems."""
    bad = []
    if p is None:
        return ["missing"]
    if p["accuracy"] in (None, 0):
        bad.append("accuracy=%s (wholesale pass failure -- re-run, do not pool)" % p["accuracy"])
    if p["unresolved"] not in (0, None) :
        bad.append("model_info_unresolved=%s (wrong window)" % p["unresolved"])
    if arm == "baseline" and p["acted"]:
        bad.append("baseline ACTED %d times (the arms are not what the configs claim)" % p["acted"])
    if arm == "A" and p["adjudicated"] < 5:
        bad.append("arm A produced %d verdicts (<5): INVALID, not futility" % p["adjudicated"])
    return bad


def main():
    seeds = []
    for s in range(1, 6):
        b, a = read_pass("i028-s%d-base" % s), read_pass("i028-s%d-A" % s)
        if b is None and a is None:
            continue
        seeds.append((s, b, a))
    if not seeds:
        print("No iteration 028 passes on disk yet. Run: ./run028.sh preflight")
        return

    print("=" * 78)
    print("ITERATION 028 READOUT  --  %d seed(s) on disk" % len(seeds))
    print("=" * 78)

    usable = []
    for s, b, a in seeds:
        pb, pa = valid(b, "baseline"), valid(a, "A")
        print("\nSEED %d" % s)
        for nm, p, probs in (("baseline", b, pb), ("arm A", a, pa)):
            if p is None:
                print("  %-9s MISSING" % nm)
                continue
            print("  %-9s accuracy %6.3f  steps %6.1f  cost $%7.2f  | acted %4d  verdicts %4d  saved %8d"
                  % (nm, p["accuracy"] or 0, p["steps"] or 0, p["cost"] or 0,
                     p["acted"], p["adjudicated"], p["saved"]))
        # THE KILL RATE, PER ARM, BEFORE ANY DIFFERENCE IS READ. Accuracy here is close to
        # `1 - kill_rate`: across iteration 024's ten passes, 63 of 150 episodes ended on a truncated
        # response and NONE of them scored, while 94% of survivors did. So a kill-rate gap BETWEEN the
        # arms is an alternative explanation for any accuracy difference, and iteration 024's arms
        # differed by 37/75 against 26/75 -- 14.7 points, available with no selection effect at all.
        eps = {}
        for nm, tag in (("baseline", "i028-s%d-base" % s), ("arm A", "i028-s%d-A" % s)):
            eps[nm] = locaterm.episodes(tag)
            line = locaterm.kill_line(tag, eps[nm])
            if line:
                print("  %-9s%s" % (nm, line))
        if eps.get("baseline") and eps.get("arm A"):
            kb = len(locaterm.kills(eps["baseline"])[0])
            ka = len(locaterm.kills(eps["arm A"])[0])
            if kb != ka:
                print("    !! KILL-RATE GAP: baseline killed %d, arm A killed %d. That is %+.1f accuracy"
                      % (kb, ka, 100.0 * (kb - ka) / max(len(eps["baseline"]), 1)) +
                      " points of headroom that owes nothing to content selection.")
                print("    !! Read the difference below as confounded, not as an effect.")
        for nm, probs in (("baseline", pb), ("arm A", pa)):
            for x in probs:
                print("    !! %s: %s" % (nm, x))
        if not pb and not pa:
            usable.append((s, b, a))
            d15 = 100 * (a["accuracy"] - b["accuracy"])
            k12 = [e for e in set(a["per"]) & set(b["per"]) if e not in DEGENERATE]
            d12 = 100 * (st.mean([a["per"][e] for e in k12]) - st.mean([b["per"][e] for e in k12])) if k12 else float("nan")
            print("    difference: %+.1f pts over 15 envs | %+.1f pts over the %d non-degenerate"
                  % (d15, d12, len(k12)))
        else:
            print("    EXCLUDED from the cumulative mean.")

    if not usable:
        print("\nNo usable seed pair yet. Nothing to decide.")
        return

    diffs = [100 * (a["accuracy"] - b["accuracy"]) for _, b, a in usable]
    run = st.mean(diffs)
    k = len(diffs)
    se = SE1 / math.sqrt(k)

    print("\n" + "=" * 78)
    print("PRIMARY: accuracy difference (arm A - baseline), cumulative over %d usable seed(s)" % k)
    print("  per seed        : %s" % ", ".join("%+.1f" % d for d in diffs))
    print("  cumulative mean : %+.1f points   (SE ~%.1f at k=%d)" % (run, se, k))
    print("  95%% interval    : %+.1f to %+.1f" % (run - 1.96 * se, run + 1.96 * se))

    # IRREVERSIBLE LOSS, reported next and not last, because an arm that wins accuracy while raising it
    # has not demonstrated a shippable mechanism -- iteration 024's 175 against 0 is the case in point.
    em = [(s, (b.get("expand_missing"), a.get("expand_missing"))) for s, b, a in usable]
    loss_rose = False
    if any(x is not None for _, pair in em for x in pair):
        print("\nexpand_unresolved_missing (baseline -> arm A), the direct measure of irreversible loss:")
        for s, (xb, xa) in em:
            worse = (xa or 0) > (xb or 0)
            loss_rose = loss_rose or worse
            print("  seed %d: %s -> %s%s" % (s, xb, xa, "   <-- ROSE" if worse else ""))
    else:
        print("\nexpand_unresolved_missing: not present in these stats. It is a FIRST-CLASS endpoint;")
        print("  find where it is recorded before reading the accuracy result as a win.")

    print("\ncost and steps (secondary):")
    for s, b, a in usable:
        print("  seed %d: cost $%.2f -> $%.2f (%+.1f%%) | steps %.1f -> %.1f (%+.1f%%)"
              % (s, b["cost"] or 0, a["cost"] or 0,
                 100 * ((a["cost"] or 0) / (b["cost"] or 1) - 1),
                 b["steps"] or 0, a["steps"] or 0,
                 100 * ((a["steps"] or 0) / (b["steps"] or 1) - 1)))

    print("\n" + "=" * 78)
    # ENFORCED, not merely mentioned. This is the endpoint that decides whether iteration 024's reward
    # was bought with content the agent asked for and did not get back -- 175 against 0 in its arm B.
    if loss_rose:
        print("!! expand_unresolved_missing ROSE in arm A. Whatever the accuracy says, this arm removed")
        print("!! content the agent asked for and did not get back. An accuracy win here is NOT a")
        print("!! shippable mechanism -- the reversibility invariant comes first. Preregistered as a")
        print("!! first-class endpoint for exactly this reading.")
        print("")
    if run < FUTILITY:
        print("DECISION: FUTILITY RULE FIRES.  cumulative %+.1f < %+.1f (one task)" % (run, FUTILITY))
        print("  The fixed configuration is not better and may be worse. STOP.")
        print("  ~$%d of the budget is unspent." % int(86 * (5 - k)))
        print("  THE RUN IS OVER. Do not run a further seed to see whether it recovers -- that would")
        print("  spend the saving to unwind the rule that produced it.")
    elif k >= 5:
        if abs(run) >= MARGIN:
            print("DECISION: seed 5 reached, |%+.1f| >= the %g-point margin. The result SEPARATES." % (run, MARGIN))
            print("  Read expand_unresolved_missing before calling it a win.")
        else:
            print("DECISION: seed 5 reached, |%+.1f| < the %g-point margin." % (run, MARGIN))
            print("  NOT SEPARATED at this n. This is NOT equivalence: 15 environments (12 usable)")
            print("  cannot resolve less than ~11 points, and more seeds do not lift that ceiling.")
        print("  The design is complete at five seeds. A sixth is a NEW experiment, not a continuation.")
    else:
        print("DECISION: continue is PERMITTED.  cumulative %+.1f >= %+.1f (one task)" % (run, FUTILITY))
        print("  This is NOT a win and cannot become one before seed 5 -- the rule is futility-only.")
        print("  %d seed(s) remain, ~$%d." % (5 - k, int(86 * (5 - k))))
    if run >= FUTILITY and k < 5:
        print("  A PERSON decides whether to run seed %d. This script does not." % (k + 1))
    print("=" * 78)


if __name__ == "__main__":
    main()
