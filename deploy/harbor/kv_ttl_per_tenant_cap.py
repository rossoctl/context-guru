#!/usr/bin/env python3
"""Per-tenant best-of-{off, N} max_pings cap, reproducibly, from kv_ttl_cost_model.py's own
by_user breakdown -- nothing hand-combined from JSON by hand, which is exactly the gap REV's
review of this line of work caught (a headline per-tenant table that could not be reproduced
from any committed script). Runs the existing, unmodified cost model at each candidate
max_pings, then picks each tenant's best option (including "off") against the SAME fixed-5m
baseline, floored at min_prefix like every other arm in this line of work.

Usage: kv_ttl_per_tenant_cap.py --db cg.db --prices prices.yaml [--candidates 1,2,6]
                                [--split 0.6] [--min-prefix 20000]
"""
from __future__ import annotations
import argparse
import json
import os
import sys
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from kv_ttl_cost_model import (  # noqa: E402
    PriceBook, PingSchedule, Semantics, derive, load_trajectories, evaluate, compare,
    DEFAULT_DB, DEFAULT_PRICES, WRITE_5M, PING_5M, EXPIRE,
)
from kv_ttl_predictor_arms import pseudonymize_map  # noqa: E402 -- t01..t18, never the raw id


def gate(cached_context: int, min_prefix: int, would_ping: bool) -> str:
    if cached_context < min_prefix:
        return EXPIRE
    return PING_5M if would_ping else WRITE_5M


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--db", default=DEFAULT_DB)
    ap.add_argument("--prices", default=DEFAULT_PRICES)
    ap.add_argument("--candidates", default="1,2,6", help="comma-separated max_pings values")
    ap.add_argument("--split", type=float, default=0.6,
                    help="score on the same test-window fraction as every other table in "
                         "this line of work (default 0.6, i.e. score the last 40%%)")
    ap.add_argument("--min-prefix", type=int, default=20_000)
    ap.add_argument("--json", help="also write the fleet-wide net-$ vs N table here, for the "
                                    "'net dollars vs ping budget' plot -- additive, changes no "
                                    "existing stdout output")
    args = ap.parse_args()

    prices = PriceBook.from_operator_file(args.prices)
    semantics = Semantics()
    rows = load_trajectories(args.db)
    lo, hi = rows[0].ts_ms, max(r.ts_ms for r in rows)
    cut = lo + int((hi - lo) * args.split)
    test = derive([r for r in rows if r.ts_ms > cut])
    window_end_ms = hi

    base_acts = {r.request_id: gate(r.cached_context, args.min_prefix, False) for r in test}
    base = evaluate(test, base_acts, prices, semantics=semantics,
                    schedule=PingSchedule(max_pings=1), window_end_ms=window_end_ms)

    candidates = [int(n) for n in args.candidates.split(",")]
    by_n = {}
    for n in candidates:
        acts = {r.request_id: gate(r.cached_context, args.min_prefix, True) for r in test}
        by_n[n] = evaluate(test, acts, prices, semantics=semantics,
                           schedule=PingSchedule(max_pings=n), window_end_ms=window_end_ms)

    print(f"baseline (fixed-5m, floored): ${base.total_usd:,.2f}\n")
    for n in candidates:
        d = compare(base, by_n[n])
        print(f"flat N={n}: {'+' if d.absolute_usd>=0 else ''}${d.absolute_usd:,.2f} "
              f"({d.percent_usd:+.2f}%)")

    tenants = sorted(base.by_user)
    total_best, harmed, unprofitable = 0.0, 0, 0
    print(f"\n{'tenant':<20}{'baseline':>12}" + "".join(f"{'N='+str(n):>12}" for n in candidates)
          + f"{'best':>12}")
    for t in tenants:
        b = base.by_user[t].total_usd
        deltas = {n: b - by_n[n].by_user.get(t, base.by_user[t]).total_usd for n in candidates}
        best_n = max(deltas, key=lambda n: deltas[n])
        best_d = max(0.0, deltas[best_n])
        total_best += best_d
        if best_d <= 0.005:
            unprofitable += 1
        if min(deltas.values()) < -0.005:
            pass  # a flat setting alone can harm even when the best-of-N doesn't -- see per-N counts below
        row = "".join(f"{deltas[n]:>12.2f}" for n in candidates)
        print(f"{t:<20}{b:>12.2f}{row}{best_d:>12.2f}")

    for n in candidates:
        harmed_n = sum(1 for t in tenants
                       if base.by_user[t].total_usd - by_n[n].by_user.get(t, base.by_user[t]).total_usd
                       < -0.005)
        print(f"flat N={n}: {harmed_n} tenant(s) harmed")
    print(f"\nper-tenant best-of-{{off,{args.candidates}}}: ${total_best:,.2f}, "
          f"{unprofitable} tenant(s) with no profitable setting")

    if args.json:
        # Pseudonymized only (t01..t18, sorted by RAW tenant_id -- this project's settled
        # convention): the stdout table above prints the raw id for a human reproducing this
        # script interactively, but nothing written to disk here may, so this block is a
        # second, independent tenant->pseudonym pass rather than reusing any raw-id string
        # already printed above.
        pseudo = pseudonymize_map(tenants)
        fleet = {str(n): {"delta_usd": compare(base, by_n[n]).absolute_usd,
                          "delta_pct": compare(base, by_n[n]).percent_usd,
                          "pings": by_n[n].pings}
                for n in candidates}
        per_tenant = {}
        for t in tenants:
            b = base.by_user[t].total_usd
            per_tenant[pseudo[t]] = {
                "baseline_usd": b,
                "delta_usd": {str(n): b - by_n[n].by_user.get(t, base.by_user[t]).total_usd
                             for n in candidates},
            }
        with open(args.json, "w") as fh:
            json.dump({"baseline_usd": base.total_usd, "candidates": candidates,
                      "fleet_wide": fleet, "per_tenant_pseudonymized": per_tenant,
                      "per_tenant_best_of_off_and_candidates_usd": total_best,
                      "n_tenants_unprofitable_at_every_setting": unprofitable}, fh, indent=2)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
