#!/usr/bin/env python3
"""Per-ping forensics for the idle keep-alive: did OUR ping hit the provider cache, how
long after the previous request did it fire, did the request that followed it hit, and
what did the ping cost.

Two subcommands, and the split is not stylistic: the service database is readable only by the
account the service runs as, and that account has no site-packages. So the half that reads it
uses nothing but the standard library, and the half that wants a DataFrame runs afterwards as
whoever has pandas.

  collect   stdlib only (sqlite3 + csv). Reads the dashboard database READ-ONLY, so it runs as
            the service account. Writes one CSV row per ping. Also accepts a session export
            bundle instead, which needs no privileged access at all.
  stats     pandas. Loads that CSV into a DataFrame and prints the tables.

A ping is identified by requests.keepalive = 1. That is exact in one direction and a lower
bound in the other, and the difference matters when reading a count off this script:

  No false positives. `KeepAlive: true` is set in exactly ONE non-test place in the service,
  proxy/keepalive.go:964 (record1), and nothing ever UPDATEs the column afterwards — the
  strategy backfill writes keepalive_strategy_id only (dash/keepalivestrategybackfill.go:151).
  No benchmark, simulation or campaign path writes a ping row. Rows older than the mechanism
  read 0 by the ALTER TABLE default, and correctly so: the column and the emitter shipped in
  the same commit (50e3966, #86), so there is no window of pings that predates the flag.

  False negatives, all of them OUTSIDE the table. A ping that never got a row: refused by the
  tenant rate limiter, failed in transport, or answered 4xx — that last one returns before
  record1 (proxy/keepalive.go:906) so a REFUSED ping is invisible here. The table therefore
  holds pings that were sent AND answered non-4xx; the process counters in KeepAliveStats
  (/stats) hold what was attempted. Read "pings" below as recorded pings, not as pings sent.
  Pings answered 5xx ARE recorded, with status >= 500 and no usage — the status line in the
  summary is where they show up.

One caveat about the service's OWN attribution columns, which this script carries but does not
compute: keepalive_pings is an in-process counter consumed by keeper.arrive() during the
PREPARATION of the next request on the session (proxy/proxy.go:1109), before that request is
sent upstream and therefore before its status is known. So the first row to arrive takes the
credit whatever becomes of it, and keepalive_saved_usd is only nonzero on a row that read more
than it wrote. In the production export checked here, the row that arrived first was a 400 every
time, so the credit landed on a row that read nothing and $0.00 was attributed — while the real
resumption a second later read the ping's prefix back token for token. Read
next_real_keepalive_saved_usd as the service's own attribution, and next_real_cache_hit together
with next_real_cache_read as what actually happened.

`collect` does not trust the flag alone: it also evaluates an INDEPENDENT structural
fingerprint of the row shape record1 produces, in both directions, so a mislabelled row shows
up as a number rather than as a silently wrong average.

No message content is read — request_content is never touched. Tenant ids are
pseudonymized and session ids hashed unless --raw-ids is passed.
"""
import argparse, csv, glob, hashlib, io, os, sqlite3, sys, tempfile, zipfile

DB = "/var/lib/context-guru/cg.db"

# Columns pulled for every row of a ping-bearing session. Enough to compute the four
# metrics, and enough to audit one suspicious row without going back to the database.
COLS = [
    "id", "ts", "tenant_id", "session_id", "keepalive", "model", "agent", "status",
    "cache_read", "cache_write", "cache_write_1h", "fresh_input", "output_tokens",
    "cost_usd", "upstream_ms", "cache_miss_reason", "keepalive_pings",
    "keepalive_saved_usd", "keepalive_strategy_id",
    # The independent fingerprint. record1 sets max_tokens=1 and leaves every one of the
    # rest at its zero value, because a ping has no agent request behind it to fill them.
    "max_tokens", "stream", "messages", "tokens_before", "mode", "cg_latency_ms", "ttfb_ms",
]

SQL = f"""
WITH ka(tenant_id, session_id) AS (
  SELECT DISTINCT tenant_id, session_id FROM requests
  WHERE keepalive = 1 AND session_id <> '' AND ts >= ?
)
SELECT {', '.join('r.' + c for c in COLS)}
FROM requests r JOIN ka ON r.tenant_id = ka.tenant_id AND r.session_id = ka.session_id
WHERE r.ts >= ?
ORDER BY r.tenant_id, r.session_id, r.ts, r.id
"""

OUT_COLS = [
    # identity / provenance
    "ping_id", "tenant", "session", "ping_ts_ms", "ping_seq", "pings_in_span",
    "model", "agent", "status", "strategy_id",
    # 1. did the ping hit, and 4. what did it cost
    "ping_cache_hit", "ping_cache_read", "ping_cache_write", "ping_cache_write_1h",
    "ping_fresh_input", "ping_output_tokens", "ping_wrote_more_than_read",
    "ping_cost_usd", "ping_cost_unpriced", "ping_upstream_ms",
    # 2. how long after the message before it
    "gap_before_s", "gap_before_raw_s", "prev_id", "prev_is_ping", "prev_ts_ms",
    "gap_before_prev_real_s", "prev_real_id", "prev_real_cache_read", "prev_real_cache_write",
    # 3. the message after: did it hit, how long after
    "gap_after_s", "next_id", "next_is_ping", "next_ts_ms",
    "gap_after_next_real_s", "next_real_id", "next_real_cache_hit",
    "next_real_cache_read", "next_real_cache_write", "next_real_cache_miss_reason",
    "next_real_cost_usd", "next_real_keepalive_pings", "next_real_keepalive_saved_usd",
    "next_real_credited", "nonok_rows_before", "nonok_rows_after",
    # data quality
    "fingerprint_ok", "no_prev_row", "no_next_row",
]


def fingerprint(r):
    """Is this row shaped like something record1 wrote, judged without the keepalive flag?

    record1 fills the ping's own tokens, cost, status and max_tokens=1 and leaves
    everything an agent request would carry at zero: no messages, no pre-compaction token
    count, no operating mode, no cache-miss attribution (a ping is deliberately excluded
    from it), no in-process latency. A real /v1/messages call cannot have zero messages.
    """
    return (r["max_tokens"] == 1 and r["stream"] == 0 and r["messages"] == 0
            and r["tokens_before"] == 0 and r["mode"] == ""
            and r["cache_miss_reason"] == "" and r["output_tokens"] <= 1
            and r["cg_latency_ms"] == 0 and r["ttfb_ms"] == 0)


def start_ms(r):
    """When this request went on the wire, for a row of either kind.

    The two writers stamp ts differently: a real request carries its START
    (proxy/dashcapture.go, TS: c.start.UnixMilli()) while a ping carries the moment
    record1 booked it, which is AFTER the round trip. Every gap here is start-to-start,
    because that is the footing the provider's cache lifetime and the keeper's own Idle
    are both measured on — "how long a session must be idle before the first ping,
    measured from the previous request's START" (CachePolicy.Idle). gap_before_raw_s keeps
    the uncorrected ts-to-ts figure for anyone reproducing this from the raw columns.
    """
    return r["ts"] - r["upstream_ms"] if r["keepalive"] else r["ts"]


def sessions(rows):
    """Group the ordered rows into sessions, yielding one list per (tenant, session)."""
    cur, key = [], None
    for r in rows:
        k = (r["tenant_id"], r["session_id"])
        if k != key and cur:
            yield cur
            cur = []
        key = k
        cur.append(r)
    if cur:
        yield cur


def spans(rows):
    """Positions of each maximal run of consecutive ping rows, as (start, length).

    A run between two real requests IS one idle span: the keeper resets its per-span
    counter when a real request re-records the session, so a run's length is K-in-practice
    and a ping's position in the run is its ping number.
    """
    out, i = [], 0
    while i < len(rows):
        if rows[i]["keepalive"]:
            j = i
            while j < len(rows) and rows[j]["keepalive"]:
                j += 1
            out.append((i, j - i))
            i = j
        else:
            i += 1
    return out


INT = {"id", "ts", "keepalive", "status", "cache_read", "cache_write", "cache_write_1h",
       "fresh_input", "output_tokens", "max_tokens", "stream", "messages", "tokens_before",
       "keepalive_pings"}
FLT = {"cost_usd", "upstream_ms", "cg_latency_ms", "ttfb_ms", "keepalive_saved_usd"}


def collect(db, since_ms, raw_ids):
    """Rows straight from the service database. Read-only, and must run as `cg`."""
    con = sqlite3.connect(f"file:{db}?mode=ro", uri=True)
    con.row_factory = sqlite3.Row
    rows = [dict(r) for r in con.execute(SQL, (since_ms, since_ms))]
    # How far back the table itself goes, so a ping count is read against the window that
    # could have contained pings rather than against "all history".
    first_ts = con.execute("SELECT MIN(ts) FROM requests").fetchone()[0]
    con.close()
    return build(rows, first_ts, raw_ids)


def collect_export(path, raw_ids):
    """Rows from a session export bundle — a zip, or an already-extracted directory.

    The shape expected is one directory per session, each holding a turns.csv that is
    `SELECT * FROM requests` for that session, so every column this needs is present including
    keepalive. It is the only source available without the service account's read on the live
    database, and the numbers come out identical to reading the database directly.
    """
    tmp = None
    if zipfile.is_zipfile(path):
        tmp = tempfile.mkdtemp(prefix="ping_stats.")
        with zipfile.ZipFile(path) as z:
            z.extractall(tmp)
        path = tmp
    files = sorted(glob.glob(os.path.join(path, "*", "turns.csv")))
    if not files:
        sys.exit(f"no */turns.csv under {path}: not a session export bundle")
    rows = []
    for f in files:
        for r in csv.DictReader(open(f)):
            if "keepalive" not in r:
                sys.exit(f"{f} predates the keepalive column; it cannot contain pings")
            rows.append({c: int(float(r[c] or 0)) if c in INT else
                            float(r[c] or 0) if c in FLT else (r.get(c) or "")
                         for c in COLS})
    rows = [r for r in rows if r["session_id"]]
    keyed = {(r["tenant_id"], r["session_id"]) for r in rows if r["keepalive"]}
    first_ts = min((r["ts"] for r in rows), default=None)
    rows = sorted((r for r in rows if (r["tenant_id"], r["session_id"]) in keyed),
                  key=lambda r: (r["tenant_id"], r["session_id"], r["ts"], r["id"]))
    return build(rows, first_ts, raw_ids)


def build(rows, table_first_ts, raw_ids):
    """One output row per ping, from rows of ping-bearing sessions in (session, ts, id) order."""
    tenants, out = {}, []
    for sess in sessions(rows):
        # Pseudonyms, assigned in first-seen order so a rerun over the same window is stable.
        t = sess[0]["tenant_id"]
        if t not in tenants:
            tenants[t] = f"t{len(tenants) + 1:02d}"
        for start, n in spans(sess):
            # The last agent request before this span and the first one after it. Either can be
            # absent: the span may sit against the retention edge of the table.
            #
            # "Agent request" means keepalive = 0 AND a 2xx from upstream, and the second half is
            # not fussiness. In the one production export available to check this against, EVERY
            # ping was followed within ~1 s by a status-400 row carrying a single message and no
            # tokens, and the actual resumption came the second after that. Those rows still get
            # a cache_miss_reason from AttributeCache (four of them read 'ttl_expiry'), so taking
            # the immediate next row inverts the answer to "did the request after the ping hit":
            # it reported 0 of 4 hitting where 2 of 4 in fact read back the ping's own prefix,
            # token for token. A row that never reached the provider has no cache result to read.
            ok = lambda r: not r["keepalive"] and 200 <= r["status"] < 300
            prev_real = next((sess[i] for i in range(start - 1, -1, -1) if ok(sess[i])), None)
            next_real = next((sess[i] for i in range(start + n, len(sess)) if ok(sess[i])), None)
            # How many non-2xx agent rows were stepped over to find it, so the skip is a number
            # on the row rather than a silent choice.
            skipped_before = sum(1 for i in range(start - 1, -1, -1)
                                 if not sess[i]["keepalive"] and not ok(sess[i])
                                 and (prev_real is None or sess[i]["ts"] > prev_real["ts"]))
            skipped_after = sum(1 for i in range(start + n, len(sess))
                                if not sess[i]["keepalive"] and not ok(sess[i])
                                and (next_real is None or sess[i]["ts"] < next_real["ts"]))
            for seq in range(1, n + 1):
                p = sess[start + seq - 1]
                prev = sess[start + seq - 2] if start + seq - 2 >= 0 else None
                nxt = sess[start + seq] if start + seq < len(sess) else None
                p_start = start_ms(p)
                gap = lambda a, b: None if a is None or b is None else round((a - b) / 1000.0, 3)
                out.append({
                    "ping_id": p["id"],
                    "tenant": t if raw_ids else tenants[t],
                    "session": p["session_id"] if raw_ids
                               else hashlib.sha256(p["session_id"].encode()).hexdigest()[:12],
                    "ping_ts_ms": p["ts"], "ping_seq": seq, "pings_in_span": n,
                    "model": p["model"], "agent": p["agent"], "status": p["status"],
                    "strategy_id": p["keepalive_strategy_id"] or "",

                    "ping_cache_hit": int(p["cache_read"] > 0),
                    "ping_cache_read": p["cache_read"], "ping_cache_write": p["cache_write"],
                    "ping_cache_write_1h": p["cache_write_1h"],
                    "ping_fresh_input": p["fresh_input"],
                    "ping_output_tokens": p["output_tokens"],
                    "ping_wrote_more_than_read": int(p["cache_write"] > p["cache_read"]),
                    "ping_cost_usd": p["cost_usd"],
                    # Event.Price only prices a row when the model is in the price table AND
                    # some token counter is nonzero; an unpriced ping reads as $0.00 and would
                    # drag the mean cost down without saying so.
                    "ping_cost_unpriced": int(p["cost_usd"] == 0 and bool(
                        p["cache_read"] or p["cache_write"] or p["output_tokens"])),
                    "ping_upstream_ms": p["upstream_ms"],

                    "gap_before_s": gap(p_start, start_ms(prev)) if prev else None,
                    "gap_before_raw_s": gap(p["ts"], prev["ts"]) if prev else None,
                    "prev_id": prev["id"] if prev else None,
                    "prev_is_ping": prev["keepalive"] if prev else None,
                    "prev_ts_ms": prev["ts"] if prev else None,
                    "gap_before_prev_real_s": gap(p_start, prev_real["ts"]) if prev_real else None,
                    "prev_real_id": prev_real["id"] if prev_real else None,
                    "prev_real_cache_read": prev_real["cache_read"] if prev_real else None,
                    "prev_real_cache_write": prev_real["cache_write"] if prev_real else None,
                    "nonok_rows_before": skipped_before,

                    "gap_after_s": gap(start_ms(nxt), p_start) if nxt else None,
                    "next_id": nxt["id"] if nxt else None,
                    "next_is_ping": nxt["keepalive"] if nxt else None,
                    "next_ts_ms": nxt["ts"] if nxt else None,
                    "gap_after_next_real_s": gap(next_real["ts"], p_start) if next_real else None,
                    "next_real_id": next_real["id"] if next_real else None,
                    "next_real_cache_hit": int(next_real["cache_read"] > 0) if next_real else None,
                    "next_real_cache_read": next_real["cache_read"] if next_real else None,
                    "next_real_cache_write": next_real["cache_write"] if next_real else None,
                    "next_real_cache_miss_reason": next_real["cache_miss_reason"] if next_real else None,
                    "next_real_cost_usd": next_real["cost_usd"] if next_real else None,
                    "next_real_keepalive_pings": next_real["keepalive_pings"] if next_real else None,
                    "next_real_keepalive_saved_usd": next_real["keepalive_saved_usd"] if next_real else None,
                    "next_real_credited": int(next_real["keepalive_saved_usd"] > 0) if next_real else None,
                    "nonok_rows_after": skipped_after,

                    "fingerprint_ok": int(fingerprint(p)),
                    "no_prev_row": int(prev is None),
                    "no_next_row": int(nxt is None),
                })

    # The other half of the identification question: rows that LOOK like a ping but are not
    # flagged as one. Counted over the same window, and reported rather than folded in.
    unflagged = sum(1 for r in rows if not r["keepalive"] and fingerprint(r))
    return out, {"rows_scanned": len(rows), "pings": len(out),
                 "flagged_not_fingerprinted": sum(1 for r in out if not r["fingerprint_ok"]),
                 "fingerprinted_not_flagged": unflagged,
                 "table_first_ts": table_first_ts, "first_ping_ts": min((r["ping_ts_ms"] for r in out), default=None)}


def cmd_collect(args):
    out, audit = (collect_export(args.export, args.raw_ids) if args.export
                  else collect(args.db, args.since_ms, args.raw_ids))
    with open(args.out, "w", newline="") as f:
        w = csv.DictWriter(f, fieldnames=OUT_COLS)
        w.writeheader()
        w.writerows(out)
    os.chmod(args.out, 0o644)
    print(f"wrote {args.out}: {audit['pings']} pings from {audit['rows_scanned']} rows "
          f"of ping-bearing sessions", file=sys.stderr)
    print(f"identification audit: {audit['flagged_not_fingerprinted']} flagged pings whose "
          f"row shape disagrees, {audit['fingerprinted_not_flagged']} unflagged rows shaped "
          f"like a ping", file=sys.stderr)
    d = lambda ms: "-" if ms is None else __import__("datetime").datetime.utcfromtimestamp(
        ms / 1000).strftime("%Y-%m-%d %H:%M")
    print(f"window: table starts {d(audit['table_first_ts'])}Z, first recorded ping "
          f"{d(audit['first_ping_ts'])}Z (UTC)", file=sys.stderr)


def cmd_stats(args):
    import pandas as pd
    df = pd.read_csv(args.csv)
    print(summarize(df))


def summarize(df):
    """The two tables asked for: averages, and the cache-miss counts with percentages."""
    import pandas as pd
    n = len(df)
    if not n:
        return "no pings in this window"
    L = []
    span = (pd.to_datetime(df.ping_ts_ms.min(), unit="ms").strftime("%Y-%m-%d"),
            pd.to_datetime(df.ping_ts_ms.max(), unit="ms").strftime("%Y-%m-%d"))
    L.append(f"{n} pings | {df.session.nunique()} sessions | {df.tenant.nunique()} tenants "
             f"| {span[0]} .. {span[1]}")
    if df.fingerprint_ok.eq(0).any():
        L.append(f"WARNING: {int(df.fingerprint_ok.eq(0).sum())} pings whose row shape does "
                 f"not match the keep-alive writer — inspect before trusting these numbers")

    avg = pd.DataFrame({
        "mean": df[MEASURES].mean(), "median": df[MEASURES].median(),
        "p95": df[MEASURES].quantile(0.95), "n": df[MEASURES].count(),
    })
    L += ["", "AVERAGES  (gaps are start-to-start seconds; *_prev_real / *_next_real skip",
          "           over intervening pings to the nearest real agent request)",
          avg.to_string(float_format=lambda v: f"{v:,.4f}")]

    def pct(mask, of=n):
        return f"{int(mask.sum()):>7d}  {100.0 * mask.sum() / of:>6.2f}%" if of else "      0       -"

    hit, late = df.ping_cache_hit.eq(1), df.ping_wrote_more_than_read.eq(1)
    L += ["", "WHAT THE PING DID  (four disjoint outcomes)       n     pct       spend"]
    for lab, m in [("clean read: hit, wrote no more", hit & ~late),
                   ("partial: read, but wrote MORE than it read", hit & late),
                   ("wrote a new entry, read nothing", ~hit & late),
                   ("no usage at all (see status)", ~hit & ~late)]:
        L.append(f"  {lab:41s} {pct(m)}  ${df.loc[m, 'ping_cost_usd'].sum():>9,.2f}")
    L += [f"  {'-> cache MISS (cache_read = 0) in total':41s} {pct(~hit)}",
          f"  {'-> priced at $0 (model not in price table)':41s} {pct(df.ping_cost_unpriced.eq(1))}",
          f"  {'-> upstream status != 200':41s} {pct(df.status.ne(200))}"]

    have = df.next_real_cache_hit.notna()
    m = int(have.sum())
    L += ["", f"THE REQUEST AFTER THE PING ({m} of {n} pings have one in the table)"]
    if m:
        L += [f"next real request HIT                          {pct(df.next_real_cache_hit.eq(1), m)}",
              f"next real request MISS                         {pct(df.next_real_cache_hit.eq(0), m)}",
              f"credited to the ping (keepalive_saved_usd > 0) {pct(df.next_real_credited.eq(1), m)}",
              "  (that last line is the SERVICE's attribution, which a non-2xx row arriving "
              "first can consume", "   before the real resumption — see the module docstring; "
              "the HIT line above is what happened)"]
        reasons = df.loc[have, "next_real_cache_miss_reason"].fillna("").value_counts()
        L.append("  by cache_miss_reason: " +
                 ", ".join(f"{k or '(blank)'}={v}" for k, v in reasons.items()))
        if df.nonok_rows_after.gt(0).any():
            L.append(f"  {int(df.nonok_rows_after.gt(0).sum())} pings had a non-2xx row between "
                     f"them and that request, stepped over ({int(df.nonok_rows_after.sum())} rows "
                     f"in total) — counting one as the request after inverts this table")

    by = df.groupby("ping_seq").agg(
        pings=("ping_id", "size"), hit_pct=("ping_cache_hit", lambda s: 100 * s.mean()),
        mean_gap_before_s=("gap_before_s", "mean"), mean_cost_usd=("ping_cost_usd", "mean"),
        total_cost_usd=("ping_cost_usd", "sum"))
    L += ["", "BY PING POSITION IN THE IDLE SPAN",
          by.to_string(float_format=lambda v: f"{v:,.4f}")]
    g = df.gap_before_s.dropna()
    if len(g) > 1:
        med = g.median()
        band = g.between(med - 12, med + 12)
        neg, dbl = g.lt(0), g.gt(1.75 * med)
        L += ["", f"timing self-check: {100 * band.mean():.1f}% of gap_before_s sits within "
                  f"+/-12s of the median {med:,.1f}s — the band a 2s sweep firing one tick after "
                  f"Idle produces, across however many Idle settings are configured.",
              f"  {int(neg.sum())} negative ({100 * neg.mean():.2f}%): the ping was still in "
              f"flight when a request arrived, so ts ordering puts that request first. Money "
              f"spent on an idle span that had already ended; arrive() can cancel a pending "
              f"ping, not one on the wire.",
              f"  {int(dbl.sum())} near 2x the median ({100 * dbl.mean():.2f}%): sweep stamps "
              f"startedAt and increments the counter BEFORE dispatch, so an attempt that bailed "
              f"at the rate limiter, in transport, or on a 4xx advanced the timer and wrote no "
              f"row. These are the visible shadow of pings missing from the table."]
    L += ["", f"total spent on pings in this window: ${df.ping_cost_usd.sum():,.4f}"]
    if df.no_prev_row.any() or df.no_next_row.any():
        L.append(f"note: {int(df.no_prev_row.sum())} pings have no preceding row and "
                 f"{int(df.no_next_row.sum())} no following row in the table (retention "
                 f"edge or evicted neighbour) — their gaps are blank, not zero")
    return "\n".join(L)


MEASURES = ["gap_before_s", "gap_before_prev_real_s", "gap_after_s",
            "gap_after_next_real_s", "ping_cost_usd", "ping_cache_read",
            "ping_upstream_ms", "next_real_cost_usd", "next_real_keepalive_saved_usd"]


def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest="cmd", required=True)

    c = sub.add_parser("collect", help="read the DB (as cg) or an export bundle into a CSV")
    c.add_argument("--db", default=DB)
    c.add_argument("--export", help="a session export zip or extracted directory to read "
                                   "instead of the live database (no privileged access needed)")
    c.add_argument("--out", required=True)
    c.add_argument("--since-ms", type=int, default=0,
                   help="epoch ms lower bound; 0 scans the whole table (a full scan of a "
                        "2 GB live database — bound it when you can)")
    c.add_argument("--raw-ids", action="store_true",
                   help="keep real tenant and session ids instead of pseudonyms")
    c.set_defaults(fn=cmd_collect)

    s = sub.add_parser("stats", help="load the CSV into a DataFrame and print the tables")
    s.add_argument("csv")
    s.set_defaults(fn=cmd_stats)

    args = ap.parse_args()
    args.fn(args)


if __name__ == "__main__":
    main()
