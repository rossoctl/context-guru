#!/usr/bin/env python3
"""Was there a FREE moment to mutate the cached prefix? Measured, not inferred.

The break-even inequality coref and extract_llm's prefix path both obey --

    S x T  >  11.5 x W

-- prices a cache-WRITE that the mutation causes. But if the provider's cache has already
expired, the prefix is re-written on the next turn REGARDLESS. At that moment a prefix
mutation costs nothing incremental: W is already being paid. No break-even test is needed,
and a component that normally cannot justify a rewrite could act for free.

Such moments arise whenever the gap between turns exceeds the cache TTL (Anthropic:
5 minutes for ephemeral_5m, 1 hour for ephemeral_1h) -- a slow tool, a user thinking, a
queue. The question this answers is whether they actually occur, and how much mass they
would have made free.

DIRECT EVIDENCE, NOT TIMING. LOCA records per-step usage in trajectory.json, so we do not
have to guess from wall-clock: a step with cache_read_input_tokens == 0 while
cache_creation_input_tokens is large means the provider found nothing cached and wrote the
prefix from scratch. Step 1 is excluded -- nothing is cached before the first call, so it is
not an opportunity, it is the beginning.

Usage: cache_opportunity.py <loca-run-dir> [more-dirs...]
"""
import glob
import json
import os
import sys


def analyse(run_dir):
    steps = cold = 0
    cold_mass = warm_mass = 0
    per_task = []
    for f in sorted(glob.glob(os.path.join(run_dir, "tasks/*/state0/trajectory.json"))):
        try:
            ut = json.load(open(f)).get("usage_tracking") or []
        except Exception:
            continue
        t_cold = t_steps = t_mass = 0
        for e in ut:
            if not isinstance(e, dict):
                continue
            steps += 1
            t_steps += 1
            created = e.get("cache_creation_input_tokens") or 0
            read = e.get("cache_read_input_tokens") or 0
            if e.get("step", 0) <= 1:
                warm_mass += read          # first call: nothing to be cold about
                continue
            if read == 0 and created > 0:
                cold += 1
                t_cold += 1
                cold_mass += created       # W that was paid anyway => free to mutate
                t_mass += created
            else:
                warm_mass += read
        if t_steps:
            per_task.append((os.path.basename(os.path.dirname(os.path.dirname(f))),
                             t_steps, t_cold, t_mass))
    return steps, cold, cold_mass, warm_mass, per_task


def main():
    if len(sys.argv) < 2:
        print(__doc__)
        return 2
    for d in sys.argv[1:]:
        steps, cold, cold_mass, warm_mass, per_task = analyse(d)
        if not steps:
            print(f"{os.path.basename(d)}: no usage data")
            continue
        print(f"\n=== {os.path.basename(d)} ===")
        print(f"  steps {steps}  cold-cache steps (beyond step 1): {cold} "
              f"({100*cold/steps:.1f}%)")
        print(f"  prefix mass re-created at those steps: {cold_mass:,} tok")
        print(f"  prefix mass served from cache elsewhere: {warm_mass:,} tok")
        if cold_mass + warm_mass:
            print(f"  => {100*cold_mass/(cold_mass+warm_mass):.1f}% of prefix traffic was "
                  f"ALREADY being re-written, i.e. free to mutate")
        hot = [p for p in per_task if p[2]]
        if hot:
            print(f"  tasks with at least one free moment: {len(hot)}/{len(per_task)}")
            for name, ts, c, m in sorted(hot, key=lambda x: -x[3])[:5]:
                print(f"    {name:34s} {c}/{ts} steps cold, {m:,} tok")
    print("\nCAVEAT that cuts the other way: a run using several parallel workers spaces one")
    print("session's turns further apart than production would, so an expired cache here may")
    print("be an artifact of the harness rather than of agent behaviour. Treat the rate as an")
    print("UPPER bound; a single-worker run would give the honest one.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
