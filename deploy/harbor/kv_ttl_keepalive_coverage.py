#!/usr/bin/env python3
"""Decompose the 3.4x gap between the orchestrator's modelled max_pings=N ceiling (P2-1c/e,
all 238,079 decision points, no gating) and the real, measured keep-alive net ($200.86).

Three candidate causes, named in P2-1e: COVERAGE (only 30 of ~18 tenants x hours are ever
targeted by an active strategy), GATES (idle_seconds=280, min_prefix_tokens=20000), and
MODEL OPTIMISM (every ping assumed to succeed). This script sizes the first two exactly,
from the real control-plane config; the third is not sizeable offline (see report).

Reuses kv_ttl_cost_model.Request/derive/load_trajectories so gap/decision-point counting is
IDENTICAL to the rest of this study rather than a second, possibly-divergent definition.
"""
from __future__ import annotations
import argparse, json, os, sqlite3, sys
from datetime import datetime
from zoneinfo import ZoneInfo
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from kv_ttl_cost_model import (  # noqa: E402
    load_trajectories, PriceBook, Semantics, PingSchedule, evaluate, compare,
    EXPIRE, WRITE_5M, PING_5M, DEFAULT_DB, DEFAULT_PRICES,
)

TZ = ZoneInfo("Asia/Jerusalem")  # tenant.DefaultStrategyTZ; empty Window.TZ resolves here

def load_active_strategies(control_db: str):
    con = sqlite3.connect(f"file:{control_db}?mode=ro", uri=True)
    rows = con.execute(
        "SELECT id, max_pings, idle_seconds, min_prefix_tokens, target_json, windows_json "
        "FROM keepalive_strategies WHERE active=1").fetchall()
    con.close()
    strategies = []
    for sid, max_pings, idle_s, min_prefix, target_json, windows_json in rows:
        target = json.loads(target_json)
        windows = json.loads(windows_json)
        assert target["mode"] == "list", "coverage math below assumes single-tenant campaigns"
        for tid in target["tenant_ids"]:
            strategies.append({
                "id": sid, "tenant_id": tid, "max_pings": max_pings,
                "idle_seconds": idle_s, "min_prefix_tokens": min_prefix,
                "windows": windows,
            })
    return strategies

def covered(ts_ms: int, tenant_id: str, strategies_by_tenant: dict) -> tuple[bool, int, int]:
    """Is this tenant, at this instant, inside an active strategy's window? Returns
    (covered, max_pings, min_prefix) of the FIRST matching strategy (no tenant in this
    corpus has more than one active strategy at overlapping windows; not re-checked here)."""
    cands = strategies_by_tenant.get(tenant_id)
    if not cands:
        return False, 0, 0
    local = datetime.fromtimestamp(ts_ms / 1000, tz=TZ)
    # Go's time.Weekday: Sunday=0 .. Saturday=6. Python's .weekday(): Monday=0 .. Sunday=6.
    py_to_go_dow = (local.weekday() + 1) % 7
    hm = local.strftime("%H:%M")
    for s in cands:
        for w in s["windows"]:
            if py_to_go_dow in w.get("days", list(range(7))) and w["start"] <= hm < w["end"]:
                return True, s["max_pings"], s["min_prefix_tokens"]
    return False, 0, 0

def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--db", default=DEFAULT_DB, help="dashboard store (requests)")
    ap.add_argument("--control-db", required=True,
                    help="control-plane store (cg-control.db, keepalive_strategies)")
    ap.add_argument("--prices", default=DEFAULT_PRICES)
    args = ap.parse_args()

    strategies = load_active_strategies(args.control_db)
    by_tenant: dict[str, list] = {}
    for s in strategies:
        by_tenant.setdefault(s["tenant_id"], []).append(s)
    print(f"active strategies: {len(strategies)} campaign-tenant pairs, "
          f"{len(by_tenant)} distinct tenants targeted", file=sys.stderr)

    rows = load_trajectories(args.db)  # real (non-ping) requests, chronological, derive()'d
    tenants_with_traffic = {r.user for r in rows}
    print(f"tenants with traffic in the snapshot: {len(tenants_with_traffic)}; "
          f"tenants ever targeted by an active strategy: {len(by_tenant & tenants_with_traffic) if isinstance(by_tenant, set) else len(set(by_tenant) & tenants_with_traffic)}",
          file=sys.stderr)

    decision_points = [r for r in rows if r.idle_ms is not None]
    n_dp = len(decision_points)
    n_gap300 = 0        # gap > idle_seconds default (280s, ~= the 300s figure P2-1a used)
    n_expired = 0       # of those, the next request forced a re-write (ttl_expiry-equivalent: cache_write >= cache_read at the NEXT row is not on this row, so approximate with miss_reason on the successor -- not tracked on Request; use gap>=300s as in P2-1a as the proxy population instead)
    n_covered = 0       # gap>280s AND tenant targeted AND inside that tenant's window AND prefix>=min_prefix_tokens
    n_covered_notgated = 0   # gap>280s AND tenant targeted AND inside window, prefix gate NOT applied (isolates the prefix gate's own bite)
    n_targeted_tenant = 0    # gap>280s AND tenant is targeted by SOME active strategy (any window)

    for r in decision_points:
        gap_s = r.idle_ms / 1000.0
        if gap_s <= 280:
            continue
        n_gap300 += 1
        if r.user in by_tenant:
            n_targeted_tenant += 1
        is_cov, max_pings, min_prefix = covered(r.ts_ms, r.user, by_tenant)
        if is_cov:
            n_covered_notgated += 1
            if r.cached_context >= min_prefix:
                n_covered += 1

    # ---- price the three populations on the SAME full-corpus denominator, so the $
    # figures are directly comparable to the real, measured $200.86 net (whole 41.4-day
    # snapshot, no train/test split -- coverage is a config-replay question, not a forecast).
    prices = PriceBook.from_operator_file(args.prices)
    semantics = Semantics()
    window_end_ms = max(r.ts_ms for r in rows)

    def gate(cached_context: int, would_ping: bool) -> str:
        if cached_context < 20_000:
            return EXPIRE
        return PING_5M if would_ping else WRITE_5M

    # Same min_prefix floor as every gated arm below: isolates keep-alive's OWN effect from
    # the separate "should we cache prefixes under 20k tokens at all" decision, which a plain
    # unconditional-WRITE_5M baseline would conflate (caught via a hit_delta sanity check:
    # an unfloored baseline caches small prefixes fixed-5m never denies, so it looks like
    # keep-alive LOSES hits, when the loss is actually the floor, applied asymmetrically).
    baseline_actions = {r.request_id: gate(r.cached_context, False) for r in rows}
    flat_ungated_actions = {r.request_id: gate(r.cached_context, True) for r in rows}
    flat_covered_actions = {}
    for r in rows:
        is_cov, max_pings, min_prefix = covered(r.ts_ms, r.user, by_tenant)
        flat_covered_actions[r.request_id] = gate(r.cached_context, is_cov)

    def cost(actions, max_pings):
        return evaluate(rows, actions, prices, semantics=semantics,
                        schedule=PingSchedule(max_pings=max_pings),
                        window_end_ms=window_end_ms)

    baseline_cost = cost(baseline_actions, 2)
    ungated_cost_mp2 = cost(flat_ungated_actions, 2)
    covered_cost_mp2 = cost(flat_covered_actions, 2)
    # mixed max_pings (1 for 15 strategies, 2 for 15) is not representable in one evaluate()
    # call (schedule is global); report mp=1 and mp=2 as the bracket the real 15/15 split sits in.
    covered_cost_mp1 = cost(flat_covered_actions, 1)

    money = {
        "baseline_fixed5m_usd": baseline_cost.total_usd,
        "flat_ping_ungated_mp2": compare(baseline_cost, ungated_cost_mp2).__dict__,
        "flat_ping_real_coverage_mp2": compare(baseline_cost, covered_cost_mp2).__dict__,
        "flat_ping_real_coverage_mp1": compare(baseline_cost, covered_cost_mp1).__dict__,
        "real_measured_net_usd": 200.86,
        "real_measured_ping_spend_usd": 242.67,
        "real_measured_credit_usd": 443.53,
    }

    result = {
        "n_decision_points": n_dp,
        "n_gap_over_280s": n_gap300,
        "n_tenant_ever_targeted": n_targeted_tenant,
        "pct_tenant_ever_targeted": 100 * n_targeted_tenant / n_gap300 if n_gap300 else 0,
        "n_covered_by_window_ignoring_prefix_gate": n_covered_notgated,
        "pct_covered_by_window_ignoring_prefix_gate": 100 * n_covered_notgated / n_gap300 if n_gap300 else 0,
        "n_covered_and_prefix_gated": n_covered,
        "pct_covered_and_prefix_gated": 100 * n_covered / n_gap300 if n_gap300 else 0,
        "distinct_tenants_targeted": len(by_tenant),
        "distinct_tenants_with_traffic": len(tenants_with_traffic),
        "money": money,
    }
    print(json.dumps(result, indent=2))

if __name__ == "__main__":
    main()
