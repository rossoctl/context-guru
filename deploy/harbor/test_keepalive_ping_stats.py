#!/usr/bin/env python3
"""Exercises the collector against a synthetic requests table. No production access.

Run it directly -- `python3 deploy/harbor/test_keepalive_ping_stats.py` -- or under pytest.
Only the summarize tests need pandas; the collector tests are stdlib.

The fixture is built from the column subset keepalive_ping_stats.SQL actually names, which
doubles as the contract: if the real table loses one of these, this fails to build.
"""
import os, sqlite3, sys, tempfile, unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import keepalive_ping_stats as ps

REAL = dict(keepalive=0, max_tokens=32000, stream=1, messages=12, tokens_before=48000,
            mode="active", cache_miss_reason="hit", output_tokens=400, cg_latency_ms=3.0,
            ttfb_ms=900.0, cache_read=48000, cache_write=0, cache_write_1h=0, fresh_input=20,
            cost_usd=0.12, upstream_ms=4000.0, status=200, model="aws/claude-sonnet-5",
            agent="claude-code", keepalive_pings=0, keepalive_saved_usd=0.0,
            keepalive_strategy_id=None)
PING = dict(keepalive=1, max_tokens=1, stream=0, messages=0, tokens_before=0, mode="",
            cache_miss_reason="", output_tokens=1, cg_latency_ms=0.0, ttfb_ms=0.0,
            cache_read=48000, cache_write=0, cache_write_1h=0, fresh_input=4,
            cost_usd=0.0073, upstream_ms=1500.0, status=200, model="aws/claude-sonnet-5",
            agent="claude-code", keepalive_pings=0, keepalive_saved_usd=0.0,
            keepalive_strategy_id=None)


def db_with(rows):
    fd, path = tempfile.mkstemp(suffix=".db")
    os.close(fd)
    con = sqlite3.connect(path)
    con.execute("CREATE TABLE requests (id INTEGER PRIMARY KEY, ts INTEGER, "
                "tenant_id TEXT, session_id TEXT, " +
                ", ".join(f"{c} {'TEXT' if c in ('model','agent','mode','cache_miss_reason','keepalive_strategy_id') else 'REAL' if c in ('cost_usd','upstream_ms','cg_latency_ms','ttfb_ms','keepalive_saved_usd') else 'INTEGER'}"
                          for c in ps.COLS if c not in ("id", "ts", "tenant_id", "session_id")) + ")")
    for i, r in enumerate(rows, 1):
        r = dict(r, id=r.get("id", i))
        con.execute(f"INSERT INTO requests ({','.join(ps.COLS)}) VALUES ({','.join('?' * len(ps.COLS))})",
                    [r[c] for c in ps.COLS])
    con.commit()
    con.close()
    return path


def row(kind, ts, tenant="acme", session="s1", **kw):
    return dict(dict(REAL if kind == "real" else PING), ts=ts, tenant_id=tenant,
                session_id=session, **kw)


class TestCollect(unittest.TestCase):
    def collect(self, rows, raw=True):
        path = db_with(rows)
        try:
            out, audit = ps.collect(path, 0, raw)
        finally:
            os.unlink(path)
        return {p["ping_id"]: p for p in out}, audit

    def test_one_ping_between_two_real_requests(self):
        # real @ t=0, ping starts at t=280s and takes 1.5s, real again 20 min later.
        pings, audit = self.collect([
            row("real", 1_000_000, id=1),
            row("ping", 1_000_000 + 281_500, id=2),
            row("real", 1_000_000 + 1_200_000, id=3, cache_read=48000, cache_miss_reason="hit",
                keepalive_pings=1, keepalive_saved_usd=0.66),
        ])
        self.assertEqual(audit["pings"], 1)
        p = pings[2]
        # gap_before is measured from the ping's START (ts - upstream_ms), which is what the
        # keeper's Idle and the provider's lifetime are both measured from: 281.5 - 1.5 = 280.
        self.assertAlmostEqual(p["gap_before_s"], 280.0, places=3)
        self.assertAlmostEqual(p["gap_before_raw_s"], 281.5, places=3)
        self.assertAlmostEqual(p["gap_before_prev_real_s"], 280.0, places=3)
        self.assertEqual(p["prev_id"], 1)
        self.assertEqual(p["prev_is_ping"], 0)
        self.assertAlmostEqual(p["gap_after_s"], 1_200_000 / 1000.0 - 280.0, places=3)
        self.assertEqual(p["next_id"], 3)
        self.assertEqual(p["ping_cache_hit"], 1)
        self.assertEqual(p["ping_wrote_more_than_read"], 0)
        self.assertEqual(p["next_real_cache_hit"], 1)
        self.assertEqual(p["next_real_credited"], 1)
        self.assertEqual(p["ping_seq"], 1)
        self.assertEqual(p["pings_in_span"], 1)
        self.assertEqual(p["fingerprint_ok"], 1)
        self.assertEqual((p["no_prev_row"], p["no_next_row"]), (0, 0))

    def test_two_pings_in_one_span_chain_and_share_the_span(self):
        pings, _ = self.collect([
            row("real", 0, id=1),
            row("ping", 281_000, id=2),
            row("ping", 561_000, id=3),
            row("real", 900_000, id=4, cache_read=0, cache_write=48000, cache_miss_reason="ttl_expiry"),
        ])
        a, b = pings[2], pings[3]
        self.assertEqual((a["ping_seq"], a["pings_in_span"]), (1, 2))
        self.assertEqual((b["ping_seq"], b["pings_in_span"]), (2, 2))
        # The second ping's immediate predecessor is the FIRST PING, 280s of idle earlier
        # — start to start, so the first ping's own 1.5s round trip does not eat into it;
        # its distance to the last real request is the whole span.
        self.assertEqual(b["prev_id"], 2)
        self.assertEqual(b["prev_is_ping"], 1)
        self.assertAlmostEqual(b["gap_before_s"], 280.0, places=3)
        self.assertAlmostEqual(b["gap_before_prev_real_s"], 559.5, places=3)
        self.assertEqual(b["prev_real_id"], 1)
        # Both pings point at the same following real request, which missed anyway.
        self.assertEqual(a["next_real_id"], 4)
        self.assertEqual(b["next_real_id"], 4)
        self.assertEqual(b["next_real_cache_hit"], 0)
        self.assertEqual(b["next_real_cache_miss_reason"], "ttl_expiry")
        self.assertEqual(a["next_id"], 3)  # the immediate next row is the other ping

    def test_non_2xx_rows_are_stepped_over_not_taken_as_the_request_after(self):
        """The shape every ping in the production export is followed by: a 400 with a single
        message and no tokens, then the actual resumption a second later. Taking the 400 would
        report a miss where the prefix was in fact read back token for token."""
        pings, _ = self.collect([
            row("real", 0, id=1),
            row("ping", 281_000, id=2, cache_read=103254),
            row("real", 346_000, id=3, status=400, messages=1, tokens_before=76, cache_read=0,
                cache_write=0, cache_miss_reason="ttl_expiry", output_tokens=0, cost_usd=0.0),
            row("real", 347_000, id=4, cache_read=103254, cache_write=52, cache_miss_reason="hit"),
        ])
        p = pings[2]
        self.assertEqual(p["next_id"], 3)             # the immediate next row is still reported
        self.assertEqual(p["next_real_id"], 4)        # ...but the request AFTER is the 2xx one
        self.assertEqual(p["next_real_cache_hit"], 1)
        self.assertEqual(p["next_real_cache_miss_reason"], "hit")
        self.assertEqual(p["nonok_rows_after"], 1)
        self.assertAlmostEqual(p["gap_after_next_real_s"], 347.0 - 279.5, places=3)

    def test_non_2xx_before_a_ping_is_not_the_message_before(self):
        pings, _ = self.collect([
            row("real", 0, id=1),
            row("real", 200_000, id=2, status=400, messages=1, tokens_before=76, cache_read=0),
            row("ping", 281_000, id=3),
        ])
        p = pings[3]
        self.assertEqual(p["prev_id"], 2)             # immediate predecessor, whatever it was
        self.assertEqual(p["prev_real_id"], 1)        # the last request that reached the provider
        self.assertAlmostEqual(p["gap_before_prev_real_s"], 279.5, places=3)
        self.assertEqual(p["nonok_rows_before"], 1)

    def test_late_ping_that_wrote_instead_of_reading(self):
        pings, _ = self.collect([
            row("real", 0, id=1),
            row("ping", 281_000, id=2, cache_read=0, cache_write=48000, cost_usd=0.09),
        ])
        p = pings[2]
        self.assertEqual(p["ping_cache_hit"], 0)
        self.assertEqual(p["ping_wrote_more_than_read"], 1)
        self.assertEqual(p["no_next_row"], 1)
        self.assertIsNone(p["gap_after_s"])
        self.assertIsNone(p["next_real_cache_hit"])

    def test_ping_with_no_preceding_row_reports_blank_not_zero(self):
        pings, _ = self.collect([row("ping", 500_000, id=1), row("real", 900_000, id=2)])
        p = pings[1]
        self.assertEqual(p["no_prev_row"], 1)
        self.assertIsNone(p["gap_before_s"])
        self.assertIsNone(p["gap_before_prev_real_s"])
        self.assertIsNone(p["prev_real_id"])

    def test_unpriced_ping_is_flagged_not_averaged_as_free(self):
        pings, _ = self.collect([row("real", 0, id=1),
                                 row("ping", 281_000, id=2, cost_usd=0.0)])
        self.assertEqual(pings[2]["ping_cost_unpriced"], 1)

    def test_identification_audit_counts_both_directions(self):
        # id=3 is flagged but shaped like agent traffic; id=4 is shaped like a ping but not
        # flagged. Both are counted, neither is silently folded into the metrics.
        _, audit = self.collect([
            row("real", 0, id=1),
            row("ping", 281_000, id=2),
            row("real", 400_000, id=3, keepalive=1),
            row("ping", 500_000, id=4, keepalive=0),
            row("real", 900_000, id=5),
        ])
        self.assertEqual(audit["flagged_not_fingerprinted"], 1)
        self.assertEqual(audit["fingerprinted_not_flagged"], 1)

    def test_same_session_id_under_two_tenants_does_not_bleed(self):
        pings, _ = self.collect([
            row("real", 0, tenant="acme", id=1),
            row("ping", 281_000, tenant="acme", id=2),
            row("real", 100_000, tenant="globex", id=3),
            row("ping", 381_000, tenant="globex", id=4),
        ])
        self.assertEqual(pings[2]["prev_id"], 1)
        self.assertEqual(pings[4]["prev_id"], 3)
        self.assertAlmostEqual(pings[4]["gap_before_s"], 279.5, places=3)

    def test_blank_session_ids_are_excluded(self):
        _, audit = self.collect([row("real", 0, session="", id=1),
                                 row("ping", 281_000, session="", id=2)])
        self.assertEqual(audit["pings"], 0)

    def test_pseudonymized_by_default(self):
        pings, _ = self.collect([row("real", 0, id=1), row("ping", 281_000, id=2)], raw=False)
        p = pings[2]
        self.assertEqual(p["tenant"], "t01")
        self.assertNotEqual(p["session"], "s1")
        self.assertEqual(len(p["session"]), 12)


class TestSummarize(unittest.TestCase):
    def test_reports_miss_count_and_percentage(self):
        import pandas as pd
        rows = [dict.fromkeys(ps.OUT_COLS, 0) for _ in range(4)]
        for i, r in enumerate(rows):
            r.update(ping_id=i, tenant="t01", session="abc", ping_ts_ms=1_700_000_000_000,
                     ping_seq=1, pings_in_span=1, status=200, fingerprint_ok=1,
                     ping_cost_usd=0.01, gap_before_s=280.0, gap_after_s=600.0,
                     next_real_cache_hit=1, next_real_credited=1,
                     next_real_cache_miss_reason="hit")
        rows[0]["ping_cache_hit"] = 0                       # a miss
        rows[0]["ping_wrote_more_than_read"] = 1            # ...that wrote
        for r in rows[1:]:
            r["ping_cache_hit"] = 1
        out = ps.summarize(pd.DataFrame(rows))
        self.assertIn("4 pings", out)
        self.assertIn("25.00%", out)                        # 1 of 4 missed
        self.assertIn("cache MISS (cache_read = 0) in total", out)
        self.assertIn("total spent on pings", out)

    def test_late_pings_are_not_printed_as_a_subset_of_misses(self):
        """A ping can read something and still write more, so "wrote instead of read" is not a
        subset of "missed". The production window had 21 late against 20 misses, which the old
        indented shape rendered as a contradiction."""
        import pandas as pd
        rows = [dict.fromkeys(ps.OUT_COLS, 0) for _ in range(3)]
        for i, r in enumerate(rows):
            r.update(ping_id=i, tenant="t01", session="abc", ping_ts_ms=1_700_000_000_000,
                     ping_seq=1, status=200, fingerprint_ok=1, gap_before_s=280.0)
        rows[0].update(ping_cache_hit=0, ping_wrote_more_than_read=1)   # missed and wrote
        rows[1].update(ping_cache_hit=1, ping_wrote_more_than_read=1)   # hit but wrote MORE
        rows[2].update(ping_cache_hit=1, ping_wrote_more_than_read=0)   # clean
        out = ps.summarize(pd.DataFrame(rows))
        self.assertIn("partial: read, but wrote MORE than it read", out)
        self.assertIn("wrote a new entry, read nothing", out)
        # one miss of three, while TWO of three wrote more than they read: the miss total is
        # printed as its own line rather than as a parent the late count hangs under.
        miss = [l for l in out.splitlines() if "cache MISS (cache_read = 0) in total" in l][0]
        self.assertRegex(miss, r"\s1\s+33\.33%")
        self.assertNotIn("of which wrote", out)

    def test_flags_fingerprint_disagreement_loudly(self):
        import pandas as pd
        r = dict.fromkeys(ps.OUT_COLS, 0)
        r.update(ping_id=1, tenant="t01", session="abc", ping_ts_ms=1_700_000_000_000,
                 ping_seq=1, status=200, fingerprint_ok=0)
        self.assertIn("WARNING", ps.summarize(pd.DataFrame([r])))


if __name__ == "__main__":
    unittest.main(verbosity=2)
