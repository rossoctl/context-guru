"""Tests for the learned 5-hour weights the band reads through `cg.py cachestate` (#1095): share_weights (the gate the
band applies) and fivehour_weights (the on-disk cache, its TTL and its fail-open rule). Every test uses a temp
XDG_CACHE_HOME: no real cache file is read or written. Run: python3 -I scripts/test_cg_weights.py"""

import os
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import cg  # noqa: E402
import cgcache  # noqa: E402

BASE = "http://127.0.0.1:8787"
FIT = {
    "ok": True,
    "weights": {"input": 2e-7, "cache_read": 1e-8, "cache_write_5m": 1.6e-7, "cache_write_1h": 2.5e-7, "output": 9e-7},
    "learned": {"input": False, "cache_read": True, "cache_write_5m": True, "cache_write_1h": False, "output": True},
    "rel_se": {"cache_read": 0.1},
    "requests": 1234,
    "windows": 5,
    "spans": 60,
    "fit_error": 0.12,
}


def with_(**kw):
    return {**FIT, **kw}


class ShareWeights(unittest.TestCase):
    def test_a_fit_that_passed_keeps_numbers_and_flags_only(self):
        got = cg.share_weights(FIT)
        self.assertEqual(got["weights"]["cache_read"], 1e-8)
        self.assertEqual(got["weights"]["cache_write_1h"], 2.5e-7)  # not learned, but the fit's scaled price: kept
        self.assertEqual(got["learned"], FIT["learned"])
        self.assertEqual(got["requests"], 1234)
        self.assertNotIn("rel_se", got)
        self.assertNotIn("fit_error", got)

    def test_below_the_gate_gives_none(self):
        for name, fit in [
            ("not ok", with_(ok=False, reason="too few requests")),
            ("an API key ({})", {}),
            ("no answer", None),
            ("no weights", with_(weights=None)),
            ("read not learned", with_(learned={**FIT["learned"], "cache_read": False})),
            ("read weight 0", with_(weights={**FIT["weights"], "cache_read": 0})),
            ("read weight not a number", with_(weights={**FIT["weights"], "cache_read": "1e-8"})),
            ("read weight a bool", with_(weights={**FIT["weights"], "cache_read": True})),
            ("no write learned", with_(learned={**FIT["learned"], "cache_write_5m": False})),
            ("the learned write is 0", with_(weights={**FIT["weights"], "cache_write_5m": 0})),
        ]:
            self.assertIsNone(cg.share_weights(fit), name)

    def test_a_learned_1h_write_alone_passes(self):
        got = cg.share_weights(with_(learned={**FIT["learned"], "cache_write_5m": False, "cache_write_1h": True}))
        self.assertIsNotNone(got)


class FiveHourWeights(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.old = os.environ.get("XDG_CACHE_HOME")
        os.environ["XDG_CACHE_HOME"] = self.tmp.name
        self.addCleanup(self.restore)
        self.calls = 0

    def restore(self):
        os.environ.pop("XDG_CACHE_HOME", None)
        if self.old is not None:
            os.environ["XDG_CACHE_HOME"] = self.old

    def fetch(self, doc):
        def f():
            self.calls += 1
            return doc
        return f

    def path(self):
        return os.path.join(cgcache.cache_dir(), "context-guru-band-8787-fivehour-weights.json")

    def age(self, seconds):
        """Make the cache file `seconds` old, and clear the back-off and the single-flight locks."""
        t = os.stat(self.path()).st_mtime - seconds
        os.utime(self.path(), (t, t))
        cgcache.clear_failure(self.path())
        for f in os.listdir(cgcache.cache_dir()):
            if ".lock." in f:
                os.unlink(os.path.join(cgcache.cache_dir(), f))

    def test_one_read_a_minute(self):
        self.assertEqual(cg.fivehour_weights(BASE, self.fetch(FIT))["requests"], 1234)
        self.assertEqual(cg.fivehour_weights(BASE, self.fetch(FIT))["requests"], 1234)
        self.assertEqual(self.calls, 1)  # the second read is the cached copy
        self.age(61)
        cg.fivehour_weights(BASE, self.fetch(with_(requests=2000)))
        self.assertEqual(self.calls, 2)

    def test_a_fit_below_the_gate_is_cached_but_gives_none(self):
        self.assertIsNone(cg.fivehour_weights(BASE, self.fetch(with_(ok=False))))
        self.assertIsNone(cg.fivehour_weights(BASE, self.fetch(FIT)))
        self.assertEqual(self.calls, 1)

    def test_a_failed_read_serves_the_last_copy_for_15_minutes_then_none(self):
        cg.fivehour_weights(BASE, self.fetch(FIT))
        self.age(120)
        self.assertEqual(cg.fivehour_weights(BASE, self.fetch(None))["requests"], 1234)  # fail open to the last copy
        self.age(15 * 60 + 1)
        self.assertIsNone(cg.fivehour_weights(BASE, self.fetch(None)))  # older than the endpoint's own cache

    def test_no_answer_and_nothing_cached_gives_none(self):
        self.assertIsNone(cg.fivehour_weights(BASE, self.fetch(None)))
        self.assertIsNone(cg.fivehour_weights(BASE, self.fetch(FIT)))  # backed off: no second read at once
        self.assertEqual(self.calls, 1)

    def test_add_weights_adds_the_weights_only_on_a_subscription(self):
        orig = cg.fivehour_weights
        cg.fivehour_weights = lambda base, fetch=None: cg.share_weights(FIT)
        self.addCleanup(lambda: setattr(cg, "fivehour_weights", orig))
        doc = {}
        cg.add_weights(BASE, doc, None)  # an API key: the mod sends no window start
        self.assertNotIn("fivehour_weights", doc)
        cg.add_weights(BASE, doc, 1_800_000_000_000)
        self.assertEqual(doc["fivehour_weights"]["requests"], 1234)

    def test_add_weights_fails_open(self):
        orig = cg.fivehour_weights

        def boom(base, fetch=None):
            raise RuntimeError("no")
        cg.fivehour_weights = boom
        self.addCleanup(lambda: setattr(cg, "fivehour_weights", orig))
        doc = {}
        cg.add_weights(BASE, doc, 1_800_000_000_000)
        self.assertNotIn("fivehour_weights", doc)


if __name__ == "__main__":
    unittest.main()
