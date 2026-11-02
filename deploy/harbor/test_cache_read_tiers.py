#!/usr/bin/env python3
"""Pins the Python port of modelinfo.CacheReadFracFor and the price-file reader's read fill.

Run: `python3 deploy/harbor/test_cache_read_tiers.py` (needs PyYAML for the reader cases).
The id table is the same as TestCacheReadFracFor in internal/modelinfo/cachefrac_test.go.
Source of the tiers: https://platform.claude.com/docs/en/about-claude/pricing (read 2026-11-02).
"""
import os, sys, tempfile, unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import kv_ttl_cost_model as m

CASES = [
    ("claude-fable-5-1", 0.025), ("claude-fable-5.1", 0.025), ("claude-mythos-5-1", 0.025),
    ("claude-mythos-5.1", 0.025), ("claude-fable-5-1[1m]", 0.025), ("aws/claude-fable-5-1", 0.025),
    ("us.anthropic.claude-mythos-5-1-v1:0", 0.025),
    ("claude-opus-5-5", 0.05), ("claude-sonnet-5-5", 0.05), ("claude-opus-5.5", 0.05),
    ("Claude-Sonnet-5-5", 0.05), ("claude-opus-5-5[1m]", 0.05), ("aws/claude-opus-5-5", 0.05),
    ("anthropic.claude-sonnet-5-5-v1:0", 0.05), ("us.anthropic.claude-opus-5-5-v1:0", 0.05),
    ("vertex_ai/claude-sonnet-5-5@20261101", 0.05),
    ("claude-opus-5", 0.1), ("claude-sonnet-5", 0.1), ("claude-fable-5", 0.1),
    ("claude-mythos-5", 0.1), ("aws/claude-opus-5", 0.1), ("claude-opus-5[1m]", 0.1),
    ("claude-opus-5-20261101", 0.1), ("claude-haiku-5-5", 0.1), ("claude-haiku-4-5", 0.1),
    ("claude-opus-4-8", 0.1), ("claude-sonnet-4-5", 0.1), ("claude-opus-5-1", 0.1),
    ("claude-fable-5-5", 0.1), ("claude-opus-5-50", 0.1),
    ("azure/gpt-5.5", 0.1), ("gemini-3-flash-preview", 0.1), ("premium", 0.1), ("", 0.1),
]

DOC = """
models:
  - {match: "claude-opus*", in: 5.0, out: 25.0}
  - {match: "aws/claude-sonnet-5", in: 3.0, out: 15.0}
  - {match: "aws/claude-fable-5-1*", in: 10.0, out: 50.0, cache_read: 0.7}
"""


def book(doc):
    with tempfile.TemporaryDirectory() as d:
        p = os.path.join(d, "prices.yaml")
        with open(p, "w") as f:
            f.write(doc)
        return m.PriceBook.from_operator_file(p)


def read(b, model):
    return b.for_model(model).cache_read * m.PER_MTOK


class Tiers(unittest.TestCase):
    def test_table(self):
        for model, want in CASES:
            self.assertEqual(m.cache_read_frac_for(model), want, model)


class Reader(unittest.TestCase):
    def test_unset_follows_the_queried_model(self):
        b = book(DOC)
        for model, want in [("aws/claude-opus-5-5", 0.25), ("claude-opus-5", 0.5),
                            ("aws/claude-sonnet-5-5", 0.15), ("aws/claude-sonnet-5", 0.3)]:
            self.assertAlmostEqual(read(b, model), want, msg=model)

    def test_entry_cache_read_wins(self):
        self.assertAlmostEqual(read(book(DOC), "aws/claude-fable-5-1"), 0.7)
        self.assertAlmostEqual(read(book("cache_read_frac: 0.1\n" + DOC), "aws/claude-fable-5-1"), 0.7)

    def test_negative_write_frac_is_rejected(self):
        with self.assertRaises(ValueError):
            book("cache_write_frac: -1\n" + DOC)

    def test_file_level_value_wins(self):
        b = book("cache_read_frac: 0.1\n" + DOC)
        self.assertAlmostEqual(read(b, "claude-opus-5-5"), 0.5)
        self.assertAlmostEqual(read(b, "aws/claude-sonnet-5-5"), 0.3)

    def test_zero_or_negative_is_an_error(self):
        for v in ("0", "-0.5"):
            with self.assertRaises(ValueError):
                book(f"cache_read_frac: {v}\n" + DOC)


if __name__ == "__main__":
    unittest.main()
