#!/usr/bin/env python3
"""turn_gate.py — would a "no ping before turn K" gate lose real value?

Reuses wait_causes.py's transcript parser, DB reader, and cost model
(sim_cost_x: a ping costs 0.1x the prefix's base input price, a miss costs
1.15x more than a hit; x = that base input price). Two modes, same shape as
wait_causes.py:

  1. Transcript mode (--projects-dir, optionally with --db): exact per-thread
     turn index and gaps, from the session transcripts.
  2. DB-only mode (--db only, no --projects-dir): the production proxy has no
     transcripts, so there is no exact thread and no exact turn index. Rows
     are grouped by `thread_id` where the DB has that column (issue #423),
     else by (session_id, model) as an approximate thread proxy — flagged as
     such in the report.

  turn  = 1-based index, within its thread, of the call/row that STARTS a
          pause. Turn 1 is the thread's first call; the keeper never pings
          before any call has happened, so there is no turn 0.
  ends  = a thread's LAST call, once it has sat idle long enough (20 min)
          that the keeper is very likely still pinging a thread that is not
          coming back. Charged as N wasted pings, no miss ever avoided.

A ping is simulated only when it passes all 3 of the shipped keep-alive
gates (issue #444's current defaults, each a flag below): the prefix is at
least `--prefix-floor` tokens, the pause has already run at least
`--idle-sec` before the first ping would fire, and one ping's own cost does
not exceed `--ping-ceiling-usd`. A pause that fails any of these is still
counted (so the report shows how much traffic never even reaches a ping
decision), but contributes zero simulated ping cost and zero avoided-miss
value.

Separately, wherever a DB is given, REAL keep-alive ping rows
(`keepalive = 1`) are attributed to the pause/turn they actually fell in,
with their own real `cost_usd` — not simulated. Pings for a session that
never returned are reported as orphans. See wait_causes.py's DBIndex for
why this must read ping rows directly rather than inferring them from a
later request's `keepalive_pings` column.

Python 3 standard library only. Run with `python3 -I`.
See README.md in this directory for usage.
"""
from __future__ import annotations

import argparse
import json
import os
import sys
import time
from collections import defaultdict
from typing import Iterable

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import wait_causes as w  # noqa: E402

# Issue #444's current shipped keep-alive defaults.
DEFAULT_PINGS = 2
DEFAULT_PREFIX_FLOOR = 80_000
DEFAULT_IDLE_SEC = 270.0
DEFAULT_PING_CEILING_USD = 0.50

# A thread's last call, idle this long, is very likely never coming back —
# any pings simulated past this point are pure waste, no miss ever avoided.
ABANDONED_SEC = 20 * 60

BUCKETS = ((1, 1, "1"), (2, 4, "2-4"), (5, 9, "5-9"), (10, 10**9, "10+"))
GATES = (5, 10)  # issue #424: "the net change for a gate at 5 and at 10"


def bucket(turn: int) -> str:
    return next(name for lo, hi, name in BUCKETS if lo <= turn <= hi)


def new_bucket() -> dict:
    return {
        "pauses": 0,
        "ends": 0,
        "eligible_pauses": 0,
        "eligible_ends": 0,
        "ping_cost_x": 0.0,
        "avoided_x": 0.0,
        "ping_cost_usd": 0.0,
        "avoided_usd": 0.0,
        "real_pings": 0,
        "real_ping_cost_usd": 0.0,
    }


def ping_would_fire(prefix_tokens: int, gap_sec: float, model: str, prices: w.PriceTable, prefix_floor: int, idle_sec: float, ceiling_usd: float) -> bool:
    """Would the keeper's 3 live gates (issue #444) let a ping through?"""
    if prefix_tokens < prefix_floor:
        return False
    if gap_sec < idle_sec:
        return False
    one_ping_usd = w.PING_COST_X * prices.price(model) * prefix_tokens
    return one_ping_usd <= ceiling_usd


def accumulate(
    acc: dict,
    turn: int,
    gap_min: float,
    prefix_tokens: int,
    model: str,
    prices: w.PriceTable,
    pings: int,
    prefix_floor: int,
    idle_sec: float,
    ceiling_usd: float,
    is_end: bool = False,
    real_pings: int = 0,
    real_pings_usd: float = 0.0,
) -> None:
    """Record one pause (is_end=False) or one abandoned-thread tail (True)
    into its turn bucket, simulated cost gated by the 3 keeper checks."""
    b = acc[bucket(turn)]
    price = prices.price(model)
    fires = ping_would_fire(prefix_tokens, gap_min * 60.0, model, prices, prefix_floor, idle_sec, ceiling_usd)
    n = pings if fires else 0
    if is_end:
        b["ends"] += 1
        if fires:
            b["eligible_ends"] += 1
            wasted_x = w.PING_COST_X * n * prefix_tokens
            b["ping_cost_x"] += wasted_x
            b["ping_cost_usd"] += wasted_x * price
    else:
        b["pauses"] += 1
        if fires:
            b["eligible_pauses"] += 1
        pings_x = w.PING_COST_X * min(n, gap_min // w.PING_INTERVAL_MIN) * prefix_tokens if n else 0.0
        baseline_x = w.sim_cost_x(gap_min, 0) * prefix_tokens
        full_at_n_x = w.sim_cost_x(gap_min, n) * prefix_tokens if n else baseline_x
        # "avoided" = what pinging saved over paying nothing and eating the
        # miss, net of the ping spend itself — same shape as the original
        # turn_gate.py draft, just gated and also priced in USD now.
        avoided_x = baseline_x - (full_at_n_x - pings_x)
        b["ping_cost_x"] += pings_x
        b["avoided_x"] += avoided_x
        b["ping_cost_usd"] += pings_x * price
        b["avoided_usd"] += avoided_x * price
    b["real_pings"] += real_pings
    b["real_ping_cost_usd"] += real_pings_usd


def gate_comparison(acc: dict) -> dict:
    """For each gate K: the net change if pinging stopped for every turn
    below K, against today's "ping from turn 1" baseline. Positive means
    the gate would have saved money (today's early pinging is a net loss);
    negative means it would have given up value that pinging currently
    captures."""
    out = {}
    for k in GATES:
        excluded = [name for lo, hi, name in BUCKETS if hi < k]
        change_x = sum(acc[n]["ping_cost_x"] - acc[n]["avoided_x"] for n in excluded)
        change_usd = sum(acc[n]["ping_cost_usd"] - acc[n]["avoided_usd"] for n in excluded)
        out[f"gate_at_{k}"] = {
            "stops_pinging_turns_below": k,
            "net_change_x": round(change_x, 4),
            "net_change_usd": round(change_usd, 4),
        }
    return out


def build_report(
    mode: str,
    acc: dict,
    prices: w.PriceTable,
    models_seen: Iterable[str],
    pings: int,
    prefix_floor: int,
    idle_sec: float,
    ceiling_usd: float,
    ping_summary: dict | None,
    orphans_summary: dict | None,
    approximate_threads: bool,
    note: str = "",
) -> dict:
    buckets_out = {}
    for _, _, name in BUCKETS:
        b = acc[name]
        buckets_out[name] = {
            **{k: v for k, v in b.items() if k not in ("ping_cost_x", "avoided_x", "ping_cost_usd", "avoided_usd", "real_ping_cost_usd")},
            "ping_cost_x": round(b["ping_cost_x"], 4),
            "avoided_x": round(b["avoided_x"], 4),
            "net_x": round(b["avoided_x"] - b["ping_cost_x"], 4),
            "ping_cost_usd": round(b["ping_cost_usd"], 4),
            "avoided_usd": round(b["avoided_usd"], 4),
            "net_usd": round(b["avoided_usd"] - b["ping_cost_usd"], 4),
            "real_ping_cost_usd": round(b["real_ping_cost_usd"], 4),
        }
    return {
        "mode": mode,
        "approximate_threads": approximate_threads,
        "note": note,
        "settings": {
            "pings": pings,
            "prefix_floor": prefix_floor,
            "idle_sec": idle_sec,
            "ping_ceiling_usd": ceiling_usd,
        },
        "price_sources": prices.sources_used(models_seen),
        "buckets": buckets_out,
        "gate_vs_no_gate": gate_comparison(acc),
        "ping_rows": ping_summary,
        "ping_orphans": orphans_summary,
    }


# ---------------------------------------------------------------------------
# Transcript mode
# ---------------------------------------------------------------------------


def run_transcript_mode(
    dirs: list[str], db_paths: list[str], pings: int, prefix_floor: int, idle_sec: float, ceiling_usd: float
) -> dict:
    now = time.time()
    db_rows: list[w.DBRow] = []
    for p in db_paths:
        db_rows.extend(w.read_db_rows(p))
    prices = w.PriceTable(w.fit_model_prices(db_rows)) if db_rows else w.PriceTable()
    db_index = w.DBIndex(db_rows) if db_rows else None

    acc = {name: new_bucket() for _, _, name in BUCKETS}
    models_seen: set[str] = set()
    threads = w.find_threads(dirs)
    for project, tid, kind, path in threads:
        try:
            calls, gaps, _, _ = w.iter_calls_and_gaps(path, tid, kind, project)
        except Exception:
            continue
        turn_index = {c.message_id: i + 1 for i, c in enumerate(calls)}  # 1-based
        for g in gaps:
            if g.is_cold_start:
                continue  # the keeper never pings before the thread's first call
            turn = turn_index.get(g.start.message_id)
            if turn is None:
                continue
            prefix_tokens = g.start.prompt_tokens_total
            models_seen.add(g.start.model)
            real_pings, real_pings_usd = db_index.pings_for(g) if db_index is not None else (0, 0.0)
            accumulate(
                acc, turn, g.gap_min, prefix_tokens, g.start.model, prices, pings, prefix_floor, idle_sec, ceiling_usd,
                real_pings=real_pings, real_pings_usd=real_pings_usd,
            )
        if calls:
            last = calls[-1]
            idle_min = (now - last.ts) / 60.0
            if (not last.is_error) and idle_min * 60.0 > ABANDONED_SEC:
                models_seen.add(last.model)
                accumulate(
                    acc, len(calls), idle_min, last.prompt_tokens_total, last.model, prices, pings, prefix_floor, idle_sec, ceiling_usd,
                    is_end=True,
                )

    ping_summary = w.ping_rows_summary(db_rows) if db_rows else None
    orphans_summary = w.ping_orphans_summary(db_index.ping_orphans()) if db_index is not None else None
    return build_report(
        "transcript", acc, prices, models_seen, pings, prefix_floor, idle_sec, ceiling_usd,
        ping_summary, orphans_summary, approximate_threads=False,
        note=f"{len(threads)} threads read from {len(dirs)} project dir(s).",
    )


# ---------------------------------------------------------------------------
# DB-only mode
# ---------------------------------------------------------------------------


def run_db_only_mode(db_paths: list[str], pings: int, prefix_floor: int, idle_sec: float, ceiling_usd: float) -> dict:
    now = time.time()
    db_rows: list[w.DBRow] = []
    for p in db_paths:
        db_rows.extend(w.read_db_rows(p))
    prices = w.PriceTable(w.fit_model_prices(db_rows))
    db_index = w.DBIndex(db_rows)

    # Pings are not turns: they never had their own hit/miss, and must not
    # become a turn index or a gap boundary. Only real, successful requests
    # form the turn sequence; pings are attributed to the gap they fall
    # inside separately, via DBIndex (same split wait_causes.py's
    # db_only_report makes, for the same reason).
    real_rows = [r for r in db_rows if r.keepalive == 0 and r.status == 200]
    has_thread_id = any(r.thread_id for r in real_rows)
    groups: dict = defaultdict(list)
    for r in real_rows:
        key = r.thread_id if (has_thread_id and r.thread_id) else (r.session_id, r.model)
        groups[key].append(r)

    acc = {name: new_bucket() for _, _, name in BUCKETS}
    models_seen: set[str] = set()
    for rows in groups.values():
        rows.sort(key=lambda r: r.ts_ms)
        models_seen.add(rows[0].model)
        for i in range(1, len(rows)):
            prev, cur = rows[i - 1], rows[i]
            models_seen.add(cur.model)
            # Gap end = the end of the PREVIOUS request (its own ts +
            # upstream_ms, i.e. approx_response_ts — when its response
            # actually arrived), not the next request's raw ts, which is
            # when it was SENT, a few ms-to-seconds later. Same convention
            # wait_causes.py uses to match a transcript call to its DB row.
            gap_min = (cur.ts_ms / 1000.0 - w.approx_response_ts(prev)) / 60.0
            if gap_min < w.GAP_THRESHOLD_MIN:
                continue
            turn = i  # prev's 1-based index in `rows`
            prefix_tokens = prev.cache_read + prev.cache_write + prev.fresh_input
            real_pings, real_pings_usd = db_index.pings_in_range(
                prev.session_id, prev.model, w.approx_response_ts(prev), cur.ts_ms / 1000.0,
                prev.thread_id if has_thread_id else None,
            )
            accumulate(
                acc, turn, gap_min, prefix_tokens, prev.model, prices, pings, prefix_floor, idle_sec, ceiling_usd,
                real_pings=real_pings, real_pings_usd=real_pings_usd,
            )
        last = rows[-1]
        idle_min = (now - w.approx_response_ts(last)) / 60.0
        if idle_min * 60.0 > ABANDONED_SEC:
            prefix_tokens = last.cache_read + last.cache_write + last.fresh_input
            accumulate(
                acc, len(rows), idle_min, prefix_tokens, last.model, prices, pings, prefix_floor, idle_sec, ceiling_usd,
                is_end=True,
            )

    ping_summary = w.ping_rows_summary(db_rows)
    orphans_summary = w.ping_orphans_summary(db_index.ping_orphans())
    group_kind = "thread_id" if has_thread_id else "(session_id, model)"
    return build_report(
        "db_only_approximate", acc, prices, models_seen, pings, prefix_floor, idle_sec, ceiling_usd,
        ping_summary, orphans_summary, approximate_threads=True,
        note=(
            f"No transcripts: {len(groups)} group(s) by {group_kind} as a best-effort thread proxy. "
            "Turn index is row order within each group, not a real per-thread call count."
        ),
    )


# ---------------------------------------------------------------------------
# Human-readable summary
# ---------------------------------------------------------------------------


def human_summary(report: dict) -> str:
    lines = [f"mode={report['mode']} approximate_threads={report['approximate_threads']}"]
    if report["note"]:
        lines.append(report["note"])
    s = report["settings"]
    lines.append(f"gates: pings={s['pings']} prefix_floor={s['prefix_floor']} idle_sec={s['idle_sec']} ping_ceiling_usd={s['ping_ceiling_usd']}")
    if report["price_sources"]:
        lines.append("price_sources: " + ", ".join(f"{m}={v['price_per_mtok']}/Mtok({v['source']})" for m, v in report["price_sources"].items()))

    lines.append("")
    lines.append("x-unit columns are in Mtok-x (ping_cost_x / 1e6 etc.) -- a dimensionless cost, in millions of base-price-token-equivalents; see wait_causes.py's sim_cost_x docstring.")
    lines.append(f"{'turn':>5} {'pauses':>7} {'elig':>5} {'ends':>5} {'ping_Mx':>8} {'avoid_Mx':>8} {'net_Mx':>8} {'ping_$':>8} {'avoid_$':>8} {'net_$':>8} {'realpings':>9} {'real_$':>8}")
    for _, _, name in BUCKETS:
        v = report["buckets"][name]
        lines.append(
            f"{name:>5} {v['pauses']:>7} {v['eligible_pauses']:>5} {v['ends']:>5} "
            f"{v['ping_cost_x'] / 1e6:>8.3f} {v['avoided_x'] / 1e6:>8.3f} {v['net_x'] / 1e6:>8.3f} "
            f"{v['ping_cost_usd']:>8.4f} {v['avoided_usd']:>8.4f} {v['net_usd']:>8.4f} "
            f"{v['real_pings']:>9} {v['real_ping_cost_usd']:>8.4f}"
        )

    lines.append("")
    for name, g in report["gate_vs_no_gate"].items():
        lines.append(f"{name}: stop pinging turns < {g['stops_pinging_turns_below']} -> net change {g['net_change_x'] / 1e6:+.3f}Mx / ${g['net_change_usd']:+.4f} (positive = gate saves money)")

    if report["ping_rows"] is not None:
        pr = report["ping_rows"]
        lines.append("")
        lines.append(
            f"real ping rows: {pr['ping_rows']} across {pr['ping_sessions']} session(s), ${pr['ping_cost_usd_total']} total "
            f"({pr['substitute_ping_rows']} summary-substitute, ${pr['substitute_ping_cost_usd_total']})"
        )
    if report["ping_orphans"] is not None:
        o = report["ping_orphans"]
        lines.append(f"ping orphans (session never returned): {o['count']} across {o['sessions']} session(s), ${o['cost_usd_total']}")

    return "\n".join(lines)


# ---------------------------------------------------------------------------
# Self-test
# ---------------------------------------------------------------------------


def run_self_test() -> bool:
    import math
    import sqlite3
    import tempfile
    from datetime import datetime, timedelta, timezone

    ok = True

    def check(label: str, got, want, tol: float = 1e-6) -> None:
        nonlocal ok
        same = math.isclose(got, want, abs_tol=tol) if isinstance(want, float) else got == want
        status = "OK" if same else "FAIL"
        if not same:
            ok = False
        print(f"  [{status}] {label}: got {got!r}, want {want!r}")

    # Anchored to "now", not a fixed date: the "ends" check (a thread's last
    # call, abandoned for 20+ min) depends on wall-clock time, so a fixed
    # past date would make every fixture call look abandoned and leak a
    # wasted-ping "end" into whatever bucket the last call's turn falls in.
    t0 = datetime.now(timezone.utc)

    def iso(offset_sec: float) -> str:
        return (t0 + timedelta(seconds=offset_sec)).isoformat().replace("+00:00", "Z")

    price = w.MODEL_BASE_INPUT_PRICE_PER_TOKEN["claude-opus-5-5"]

    with tempfile.TemporaryDirectory() as tmp:
        session_id = "turn-gate-self-test"
        main_path = os.path.join(tmp, f"{session_id}.jsonl")
        with open(main_path, "w") as fh:
            # A gap's prefix is the STARTING call's own tokens (what the
            # keeper would have to re-send on a miss) -- so m1 (not m2)
            # controls the turn-1->2 gap's eligibility, m2 the turn-2->3
            # gap's, and so on.
            #
            # Call 1 (turn 1): cold start, excluded from pauses either way.
            w._write_call_line(fh, iso(0), "m1", "claude-opus-5-5", cache_read=0, cache_creation=100_000)
            # Gap after turn 1: 10 min, m1's prefix 100_010 (>= default floor
            # 80_000, idle 600s >= default 270s) -> this pings.
            w._write_call_line(fh, iso(600), "m2", "claude-opus-5-5", cache_read=0, cache_creation=500)
            # Gap after turn 2: 6 min, m2's prefix only 510 (< default floor)
            # -> recorded as a pause, but the floor blocks the ping.
            w._write_call_line(fh, iso(960), "m3", "claude-opus-5-5", cache_read=0, cache_creation=100_000)
            # Gap after turn 3: 5 min (300s), m3's prefix 100_010 (>= floor),
            # and 300s passes the default idle_sec (270s) -> this pings too.
            # Under a TEST idle_sec of 400s, this one gets blocked instead.
            w._write_call_line(fh, iso(1260), "m4", "claude-opus-5-5", cache_read=0, cache_creation=100_000)

        # DB-only fixture: one group with a thread_id column present, one
        # ping row inside the first gap (real cost), one orphan ping.
        db_path = os.path.join(tmp, "self-test.db")
        con = sqlite3.connect(db_path)
        con.execute(
            """
            create table requests (
                id integer primary key, ts integer, session_id text, model text,
                status integer, cache_read integer, cache_write integer,
                fresh_input integer, output_tokens integer, cost_usd real,
                cache_miss_reason text, upstream_ms real, keepalive integer,
                keepalive_pings integer, saved_unique integer, stop_reason text,
                max_tokens integer, thread_id text
            )
            """
        )

        def iso_to_epoch(ts: str) -> float:
            return w.iso_to_epoch(ts)

        def insert_real(ts_iso, cache_read, cache_write, thread_id):
            con.execute(
                "insert into requests (ts, session_id, model, status, cache_read, cache_write, fresh_input, "
                "output_tokens, cost_usd, cache_miss_reason, upstream_ms, keepalive, keepalive_pings, "
                "saved_unique, stop_reason, max_tokens, thread_id) values (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
                (
                    int(iso_to_epoch(ts_iso) * 1000), "db-session", "claude-opus-5-5", 200, cache_read, cache_write,
                    10, 50, 0.001, "prefix_change", 0.0, 0, 0, 0, "end_turn", 4096, thread_id,
                ),
            )

        def insert_ping(ts_iso, cost_usd, thread_id, max_tok=4096):
            con.execute(
                "insert into requests (ts, session_id, model, status, cache_read, cache_write, fresh_input, "
                "output_tokens, cost_usd, cache_miss_reason, upstream_ms, keepalive, keepalive_pings, "
                "saved_unique, stop_reason, max_tokens, thread_id) values (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
                (
                    int(iso_to_epoch(ts_iso) * 1000), "db-session", "claude-opus-5-5", 200, 0, 0,
                    0, 0, cost_usd, "", 0.0, 1, 0, 0, "end_turn", max_tok, thread_id,
                ),
            )

        # Row 1 (turn 1), row 2 (turn 2): a 10-minute gap, prefix 90_000
        # tokens (all cache_write, so a miss) -> pings and avoids a miss.
        insert_real(iso(0), cache_read=0, cache_write=90_000, thread_id="t1")
        insert_ping(iso(300), cost_usd=0.009, thread_id="t1")  # real ping inside the gap
        insert_real(iso(600), cache_read=0, cache_write=5000, thread_id="t1")
        # An orphan ping, long after, same session+model, no thread_id (tests
        # the (session_id, model) fallback path for ping attribution).
        insert_ping(iso(50_000), cost_usd=0.02, thread_id=None)
        con.commit()
        con.close()

        # --- Transcript mode ---
        report = run_transcript_mode([tmp], [], DEFAULT_PINGS, DEFAULT_PREFIX_FLOOR, DEFAULT_IDLE_SEC, DEFAULT_PING_CEILING_USD)
        b = report["buckets"]
        check("transcript bucket 1: one pause (turn 1->2) and it pings", b["1"]["pauses"], 1)
        check("transcript bucket 1: ping fires (floor+idle+ceiling all pass)", b["1"]["eligible_pauses"], 1)
        check("transcript bucket 2-4: two pauses recorded (turns 2->3, 3->4)", b["2-4"]["pauses"], 2)
        # Turn 2 (m2's small prefix) is floor-blocked; turn 3 (m3's large
        # prefix, 300s idle) passes under the default idle_sec of 270s.
        check("transcript bucket 2-4: turn 2 floor-blocked, turn 3 eligible -> 1 of 2", b["2-4"]["eligible_pauses"], 1)
        # Known value: bucket "1"'s ping cost in x units, pings=2, gap=10min,
        # m1's prefix = 100_000 cache_creation + 10 input_tokens = 100_010.
        # window(2)=4.67*2+5=14.34min >= 10min, so pings_fired = min(2, 10//4.67) = 2.
        # ping_x = 0.1*2*100_010 = 20_002.
        check("transcript bucket 1 ping_cost_x", b["1"]["ping_cost_x"], 0.1 * 2 * 100_010)
        check("transcript bucket 1 ping_cost_usd", b["1"]["ping_cost_usd"], round(0.1 * 2 * 100_010 * price, 4))

        # --- The idle_sec gate, exercised directly: turn 3's gap is 300s,
        # which passes the shipped default (270s) but not a stricter 400s.
        gated = run_transcript_mode([tmp], [], DEFAULT_PINGS, DEFAULT_PREFIX_FLOOR, idle_sec=400.0, ceiling_usd=DEFAULT_PING_CEILING_USD)
        gb = gated["buckets"]
        check("idle_sec=400 blocks turn 3's 300s pause too -> 0 of 2 eligible", gb["2-4"]["eligible_pauses"], 0)
        check("idle_sec=400: no simulated ping cost left in bucket 2-4", gb["2-4"]["ping_cost_x"], 0.0)

        # --- The ceiling gate, exercised directly: a tiny ceiling blocks
        # every ping regardless of prefix/idle.
        capped = run_transcript_mode([tmp], [], DEFAULT_PINGS, DEFAULT_PREFIX_FLOOR, DEFAULT_IDLE_SEC, ceiling_usd=0.0)
        cb = capped["buckets"]
        check("ping_ceiling_usd=0 blocks every ping", sum(v["eligible_pauses"] for v in cb.values()), 0)
        check("ping_ceiling_usd=0: no simulated ping cost anywhere", sum(v["ping_cost_x"] for v in cb.values()), 0.0)

        # --- The prefix floor, exercised directly: a floor of 0 lets the
        # 1000-token pause (turn 3) through too.
        no_floor = run_transcript_mode([tmp], [], DEFAULT_PINGS, prefix_floor=0, idle_sec=DEFAULT_IDLE_SEC, ceiling_usd=DEFAULT_PING_CEILING_USD)
        nf = no_floor["buckets"]
        check("prefix_floor=0: the small-prefix pause (turn 3) now also pings", nf["2-4"]["eligible_pauses"], 2)

        # --- DB-only mode ---
        db_rows = w.read_db_rows(db_path)
        dbr = run_db_only_mode([db_path], DEFAULT_PINGS, DEFAULT_PREFIX_FLOOR, DEFAULT_IDLE_SEC, DEFAULT_PING_CEILING_USD)
        check("db_only is flagged approximate", dbr["approximate_threads"], True)
        db_b = dbr["buckets"]
        check("db_only bucket 1: one pause (row1->row2, by thread_id)", db_b["1"]["pauses"], 1)
        check("db_only ping_rows total: both ping rows counted directly", dbr["ping_rows"]["ping_rows"], 2)
        check("db_only real ping attributed to the row1->row2 gap", db_b["1"]["real_pings"], 1)
        check("db_only real ping cost is that ping's own cost_usd", db_b["1"]["real_ping_cost_usd"], 0.009)
        check("db_only orphan ping (never attributed)", dbr["ping_orphans"]["count"], 1)
        check("db_only orphan ping cost is its own cost_usd", dbr["ping_orphans"]["cost_usd_total"], 0.02)

        # --- Gate comparison, computed directly from the transcript report. ---
        gate5 = report["gate_vs_no_gate"]["gate_at_5"]
        expect_change_x = (b["1"]["ping_cost_x"] - b["1"]["avoided_x"]) + (b["2-4"]["ping_cost_x"] - b["2-4"]["avoided_x"])
        check("gate_at_5 excludes buckets '1' and '2-4'", gate5["net_change_x"], round(expect_change_x, 4))
        gate10 = report["gate_vs_no_gate"]["gate_at_10"]
        check("gate_at_10 excludes '1','2-4','5-9' too (none here, so same as gate_at_5's buckets)", gate10["net_change_x"], round(expect_change_x, 4))

    return ok


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--projects-dir", action="append", default=[], help="A ~/.claude/projects/<project> dir. Repeatable.")
    ap.add_argument("--db", action="append", default=[], help="A dashboard-*.db path. Repeatable.")
    ap.add_argument("--pings", type=int, default=DEFAULT_PINGS, help=f"Keep-alive ping budget to simulate (default {DEFAULT_PINGS}).")
    ap.add_argument("--prefix-floor", type=int, default=DEFAULT_PREFIX_FLOOR, help=f"Minimum prefix tokens to ping (default {DEFAULT_PREFIX_FLOOR}).")
    ap.add_argument("--idle-sec", type=float, default=DEFAULT_IDLE_SEC, help=f"Minimum idle seconds before the first ping (default {DEFAULT_IDLE_SEC}).")
    ap.add_argument("--ping-ceiling-usd", type=float, default=DEFAULT_PING_CEILING_USD, help=f"Max USD for one ping (default {DEFAULT_PING_CEILING_USD}).")
    ap.add_argument("--json", action="store_true", help="Print machine-readable JSON instead of a summary.")
    ap.add_argument("--self-test", action="store_true", help="Run the built-in self-test on synthetic fixtures and exit.")
    args = ap.parse_args()

    if args.self_test:
        ok = run_self_test()
        print("\nself-test " + ("PASSED" if ok else "FAILED"))
        return 0 if ok else 1

    if not args.projects_dir and not args.db:
        ap.error("pass at least one of --projects-dir or --db")

    if args.projects_dir:
        report = run_transcript_mode(args.projects_dir, args.db, args.pings, args.prefix_floor, args.idle_sec, args.ping_ceiling_usd)
    else:
        report = run_db_only_mode(args.db, args.pings, args.prefix_floor, args.idle_sec, args.ping_ceiling_usd)

    if args.json:
        print(json.dumps(report, indent=2, default=str))
    else:
        print(human_summary(report))
    return 0


if __name__ == "__main__":
    sys.exit(main())
