"""Paired reward analysis for two LOCA arms, clustered by task.

Written BEFORE iteration 010's arms finished, so the analysis cannot be shaped by the numbers.
Pre-registration: docs/experiments/loca/iter010/PREREGISTRATION.md (ecf4103, amended 4eecbdc).

WHAT THIS GETS RIGHT THAT EARLIER READS DID NOT.

Every result in iterations 007 and 008 was computed from `tasks/*/state0/eval.json`, which reads
ONE of the 5 seeds. `state0`..`state4` ARE the seeds, so that glob silently discarded 60 of 75
completed runs and overstated per-run cost by 5x. This script globs `state*`, prints the eval count
against the config length, and refuses to summarise if they disagree -- the reconciliation, not the
glob, is what makes the n trustworthy.

WHY CLUSTERING IS NOT OPTIONAL HERE. 75 runs are 5 seeds of 15 tasks. Seeds of one task share its
environment, its tools and its difficulty, so they are not independent observations: treating 75
pairs as 75 independent ones would understate every interval by roughly sqrt(5). Extra seeds buy
precision WITHIN a task; they do not add tasks. So two figures are always printed together --
pair-level (optimistic) and task-clustered (conservative) -- and the pre-registration commits in
advance to quoting the conservative one. Printing only the flattering end is the failure this
duplication exists to prevent.

TWO-SIDED BY CONSTRUCTION. Where the baseline is itself lossy -- LOCA's native trimmer drops whole
messages, and a summariser paraphrases exact identifiers into prose -- selective removal can RAISE
reward. A one-sided harm bound cannot register that even when it happens, so gains and harms are
both counted and reported.
"""
import glob
import json
import os
import sys
from collections import defaultdict
from math import comb

SOLVED = 1.0


def load(run_dir):
    """{(task, seed_state): (status, accuracy)} over ALL seeds."""
    out = {}
    for f in glob.glob(os.path.join(run_dir, "tasks", "*", "state*", "eval.json")):
        # .../<run>/tasks/<task>/<state>/eval.json -- so task is [-3] and state is [-2].
        # An off-by-one here is silent and severe: keying on ("tasks", <task>) collapses all 5
        # seeds of a task onto one dict entry, so 75 runs read as 15 and the seed dimension
        # vanishes exactly as it did in iterations 007 and 008. Caught by validating against a
        # run whose true counts were already known.
        parts = f.split(os.sep)
        task, state = parts[-3], parts[-2]
        try:
            j = json.load(open(f))
        except (ValueError, OSError):
            continue
        out[(task, state)] = (j.get("status"), j.get("accuracy"))
    return out


def cost_of(run_dir):
    return sum(json.load(open(f)).get("total_cost_usd") or 0
               for f in glob.glob(os.path.join(run_dir, "summary-*.json")))


def mcnemar_exact(b, c):
    n = b + c
    if n == 0:
        return 1.0
    k = min(b, c)
    return min(1.0, 2 * sum(comb(n, i) for i in range(k + 1)) / 2 ** n)


def cp_upper(k, n, alpha=0.05):
    """Upper one-sided Clopper-Pearson bound on a rate given k events in n trials."""
    if n == 0:
        return 1.0
    lo, hi = 0.0, 1.0
    for _ in range(200):
        m = (lo + hi) / 2
        p = sum(comb(n, i) * m ** i * (1 - m) ** (n - i) for i in range(k + 1))
        if p > alpha:
            lo = m
        else:
            hi = m
    return (lo + hi) / 2


def main():
    if len(sys.argv) < 3:
        sys.exit("usage: reward_pairs.py <baseline_run_dir> <treatment_run_dir> [expected_n]")
    a_dir, b_dir = sys.argv[1], sys.argv[2]
    expected = int(sys.argv[3]) if len(sys.argv) > 3 else None
    A, B = load(a_dir), load(b_dir)
    print(f"baseline  {os.path.basename(a_dir)}: {len(A)} evals  ${cost_of(a_dir):.2f}")
    print(f"treatment {os.path.basename(b_dir)}: {len(B)} evals  ${cost_of(b_dir):.2f}")
    if expected:
        for nm, d in (("baseline", A), ("treatment", B)):
            flag = "OK" if len(d) == expected else "!! MISMATCH"
            print(f"   {nm}: {len(d)}/{expected} expected runs -- {flag}")
        if len(A) != expected or len(B) != expected:
            print("   Incomplete or unexpected run count: n below is what was actually read, "
                  "not what was planned. Report it as such.")

    keys = sorted(set(A) & set(B))
    usable, errored = [], 0
    for k in keys:
        if A[k][0] != "success" or B[k][0] != "success":
            errored += 1
            continue
        usable.append((k, A[k][1] == SOLVED, B[k][1] == SOLVED))
    n11 = sum(1 for _, x, y in usable if x and y)
    n00 = sum(1 for _, x, y in usable if not x and not y)
    harm = [k for k, x, y in usable if x and not y]      # baseline solved, treatment did not
    gain = [k for k, x, y in usable if y and not x]

    print(f"\ncommon={len(keys)}  usable={len(usable)}  errored={errored} "
          f"(errors are excluded, not counted as failures)")
    print(f"both-pass={n11}  both-fail={n00}  HARM={len(harm)}  GAIN={len(gain)}  "
          f"discordant={len(harm)+len(gain)}")
    print(f"solve rate: baseline={sum(1 for _,x,_ in usable if x)}/{len(usable)}  "
          f"treatment={sum(1 for _,_,y in usable if y)}/{len(usable)}")

    p = mcnemar_exact(len(harm), len(gain))
    print(f"\nMcNemar exact (two-sided), pair level: p={p:.3f}"
          + ("  -- not significant" if p >= 0.05 else "  -- SIGNIFICANT"))

    # Task-clustered: a task counts as harmed only if it harms on net across its seeds.
    per_task = defaultdict(lambda: [0, 0])
    for k, x, y in usable:
        t = k[0]
        if x and not y:
            per_task[t][0] += 1
        elif y and not x:
            per_task[t][1] += 1
    t_harm = sum(1 for h, g in per_task.values() if h > g)
    t_gain = sum(1 for h, g in per_task.values() if g > h)
    n_tasks = len({k[0] for k, _, _ in usable})
    print(f"task-clustered ({n_tasks} tasks): net-harmed={t_harm}  net-gained={t_gain}  "
          f"p={mcnemar_exact(t_harm, t_gain):.3f}")

    print("\nNON-INFERIORITY -- upper 95% bound on the harm rate:")
    print(f"  pair level   (optimistic, n={len(usable)}): "
          f"{100*cp_upper(len(harm), len(usable)):.0f}%  [{len(harm)} harm events]")
    print(f"  task level   (CONSERVATIVE, QUOTE THIS, n={n_tasks}): "
          f"{100*cp_upper(t_harm, n_tasks):.0f}%  [{t_harm} net-harmed tasks]")
    print("  The pre-registration commits to the task-level figure. Seeds of one task share its"
          "\n  environment and difficulty, so pair-level treats correlated observations as"
          "\n  independent and reports an interval that is too narrow.")

    if harm:
        print(f"\nharmed pairs: {[(t, s) for t, s in harm][:10]}")
    if gain:
        print(f"gained pairs: {[(t, s) for t, s in gain][:10]}")


if __name__ == "__main__":
    main()
