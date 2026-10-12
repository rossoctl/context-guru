"""turn_gate.py — what keep-alive pings cost and save, by the turn they follow.

Question it answers: would a "no ping before turn K" gate (K = 5 or 10) lose
real value? Reuses wait_causes.py's transcript parser and its cost model
(sim_cost_x: a ping is 0.1x the prefix, a miss 1.15x more than a hit), under
the shipped keep-alive defaults (2 pings, 20k-token prefix floor).

  turn  = index of the call that STARTS the pause (0 = the thread's first
          call; the keeper already refuses turn 0).
  ends  = threads whose last call is followed by pings that buy nothing
          (charged N pings; skipped when the last call is < 20 min old).

Units are Mtok x the model's base input price.

Transcript mode only for now. A DB-only (production) mode would group rows
the way wait_causes.py's DB mode does, by (session_id, model), and use the
row order within that group as the turn index.

  python3 -I scripts/analysis/turn_gate.py ~/.claude/projects/<project> [...]
"""

import collections
import os
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import wait_causes as w  # noqa: E402

N_PINGS, PREFIX_FLOOR = 2, 20000
BUCKETS = ((0, 0, "0"), (1, 1, "1"), (2, 4, "2-4"), (5, 9, "5-9"), (10, 10**9, "10+"))


def bucket(i: int) -> str:
    return next(name for lo, hi, name in BUCKETS if lo <= i <= hi)


def main(dirs: list[str]) -> None:
    now = time.time()
    # bucket -> [ping cost, miss cost avoided, pauses, thread ends]
    acc = collections.defaultdict(lambda: [0.0, 0.0, 0, 0])
    threads = w.find_threads(dirs)
    for project, tid, kind, path in threads:
        try:
            calls, gaps, _, _ = w.iter_calls_and_gaps(path, tid, kind, project)
        except Exception:
            continue
        index = {c.message_id: i for i, c in enumerate(calls)}
        for g in gaps:
            i = index.get(g.start.message_id)
            p = g.start.prompt_tokens_total
            if i is None or p < PREFIX_FLOOR:
                continue
            pings = w.PING_COST_X * min(N_PINGS, g.gap_min // w.PING_INTERVAL_MIN) * p
            avoided = w.sim_cost_x(g.gap_min, 0) * p - (w.sim_cost_x(g.gap_min, N_PINGS) * p - pings)
            b = acc[bucket(i)]
            b[0] += pings
            b[1] += avoided
            b[2] += 1
        if calls:
            last = calls[-1]
            if not last.is_error and last.prompt_tokens_total >= PREFIX_FLOOR and now - last.ts > 20 * 60:
                b = acc[bucket(len(calls) - 1)]
                b[0] += w.PING_COST_X * N_PINGS * last.prompt_tokens_total
                b[3] += 1

    print(f"threads={len(threads)} pings={N_PINGS} floor={PREFIX_FLOOR}  units: Mtok x base input price")
    print(f"{'turn':>5} {'pauses':>7} {'ends':>6} {'ping cost':>10} {'avoided':>9} {'net':>9}")
    for _, _, name in BUCKETS:
        cost, avoided, n, ends = acc[name]
        print(f"{name:>5} {n:>7} {ends:>6} {cost / 1e6:>10.2f} {avoided / 1e6:>9.2f} {(avoided - cost) / 1e6:>9.2f}")


if __name__ == "__main__":
    main(sys.argv[1:])
