"""Golden tests for tv.py. The proxy is a function over a seeded synthetic history and a fixed clock, so every
screen is byte-for-byte reproducible at three widths, in colour (dark and light), without colour, and ASCII-only.
UPDATE_GOLDEN=1 rewrites the goldens after an intended change; read the diff.
Run: python3 scripts/test_tv.py"""

import json
import os
import random
import re
import sys
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import tv  # noqa: E402
from tvlib import Env  # noqa: E402

TD = os.path.join(HERE, "..", "testdata", "tv")
NOW = 1791473400000  # 2026-10-08T15:30:00Z
MIN = 60_000
RATE = dict(fresh=3.8e-6, read=3.8e-7, write=4.75e-6, out=1.9e-5)


def req(i, ts, sid, model, tools, sysb, msgs, c, miss, ping=False):
    """One request row from a context size and whether the cache was lost."""
    write = c if miss else int(c * 0.01) + 600
    read = 0 if miss else c - write - 2
    cost = 2 * RATE["fresh"] + read * RATE["read"] + write * RATE["write"] + 600 * RATE["out"]
    return {"id": i, "ts": ts, "session_id": sid, "model": model, "messages": msgs, "tools": tools, "system_blocks": sysb,
            "fresh_input": 2, "cache_read": read, "cache_write": write, "cost_usd": round(cost, 4), "keepalive": ping}


def history():
    rnd = random.Random(7)
    rows, i = [], 1
    # one live session: a main thread and a subagent, three pauses, one compaction, a ping
    t, c, m = NOW - 180 * MIN, 60_000, 4
    pauses = {40: 9, 75: 14, 110: 22}
    for k in range(150):
        miss = k == 0 or k in pauses
        if k in pauses:
            t += pauses[k] * MIN
            if pauses[k] > 12:
                rows.append(req(i, t - 8 * MIN, "session-A", "claude-opus-5", 28, 3, m, c, False, ping=True)); i += 1
        else:
            t += int(rnd.uniform(40, 110) * 1000)
        if k == 95:
            c, m = 22_000, 8
        rows.append(req(i, t, "session-A", "claude-opus-5", 28, 3, m, c, miss)); i += 1
        if k % 9 == 0:
            rows.append(req(i, t + 5000, "session-A", "claude-sonnet-5", 10, 2, 3 + k // 9, 14_000 + 400 * (k // 9), k == 0)); i += 1
        c, m = c + int(rnd.uniform(1500, 4500)), m + 2
    last = rows[-1]["ts"]
    rows[:] = [r for r in rows if r["ts"] <= NOW - 90_000]
    # a second session gone cold
    for k in range(10):
        rows.append(req(i, NOW - 50 * MIN + k * 40_000, "session-B", "claude-opus-5", 28, 3, 6 + 2 * k, 90_000 + 3000 * k, k == 0)); i += 1
    # fourteen days of other sessions, with pauses at habitual hours
    for day in range(1, 15):
        for hour, n in ((9, 6), (11, 6), (13, 8), (16, 5)):
            sid, t0, c = f"session-d{day}h{hour}", NOW - day * 86_400_000 + (hour - 15) * 3_600_000 - 30 * MIN, 50_000
            for k in range(n):
                rows.append(req(i, t0 + k * (rnd.choice([1, 1, 1, 12]) * MIN), sid, "claude-opus-5", 28, 3, 2 * k + 2, c,
                                k == 0 or (hour == 13 and k % 3 == 0))); i += 1
                c += 2500
    rows.sort(key=lambda r: r["ts"])
    for n, r in enumerate(rows, 1):
        r["id"] = n
    return rows


ROWS = history()
SPEND = json.load(open(os.path.join(TD, "spend.json")))


def stats():
    wf = [("baseline", 898.05, True), ("compaction", -33.9, False), ("cachesplit_saved", 0, False), ("keepalive_saved", 9.4, False),
          ("keepalive_ping", 3.1, False), ("decl_filter_saved", 7.85, False), ("self_removed", 0, False), ("cg_llm", 4.1, False)]
    return {"total_saved_usd": 41.2, "cost_usd": 856.85, "baseline_cost_usd": 898.05, "waterfall": [{"key": k, "delta_usd": v} for k, v, _ in wf]}


class Fx(tv.Fast):
    """The proxy as a function of (path, params)."""

    def get(self, path, params=None, timeout=4):
        p = params or {}
        if path == "/api/requests":
            rs = [r for r in ROWS if (not p.get("session") or r["session_id"] == p["session"]) and r["ts"] >= p.get("since", 0)]
            rs = sorted(rs, key=lambda r: -r["id"])
            if p.get("before"):
                rs = [r for r in rs if r["id"] < p["before"]]
            lim = p.get("limit", 50)
            return {"requests": rs[:lim], "next_cursor": rs[lim - 1]["id"] if len(rs) > lim else 0}
        if path == "/api/sessions":
            return {"sessions": [{"session_id": "session-A", "end": NOW - 100_000}, {"session_id": "session-B", "end": NOW - 50 * MIN}]}
        if path == "/api/cachestate":
            sid = p["session"]
            last = max(r["ts"] for r in ROWS if r["session_id"] == sid)
            return {"session": sid, "model": "claude-opus-5", "last_ts": last, "now_ms": NOW, "ttl_s": 300, "prefix_tokens": 120_000,
                    "keepalive": False, "rewrite_usd": 0.57, "priced": True, "window": 200_000, "window_exact": True}
        if path == "/api/stats":
            return stats()
        if path == "/api/series":
            n = (NOW - p["since"]) // p["bucket"] + 1
            rnd = random.Random(p["bucket"])
            return {"buckets": [{"ts": p["since"] // p["bucket"] * p["bucket"] + k * p["bucket"], "saved_usd": round(rnd.uniform(0, 5) * (k % 5 != 3), 2)}
                                for k in range(n)]}
        if path == "/api/spend/explain":
            return SPEND
        return None


def draw(view, e, session="session-A", days=0):
    o = {"now": NOW, "tz_min": 0, "days": days, "session": session, "details": False, "args": []}
    api = Fx("")
    if view == "watch":
        s = tv.Snap()
        s.proj = type("P", (), {"name": staticmethod(lambda sid: "(unknown project)")})()
        tv.fetch(api, o, s, NOW - 2000)
        return "\n".join(tv.frame(s, o, e, NOW + 20_000)) + "\n"
    return "\n".join(tv.VIEWS[view](api, o, e)) + "\n"


MODES = [("w60", dict(width=60, color=False, ascii_only=False)), ("w80", dict(width=80, color=False, ascii_only=False)),
         ("w120", dict(width=120, color=False, ascii_only=False)),
         ("ascii", dict(width=80, color=False, ascii_only=True)),
         ("dark", dict(width=80, color=True, ascii_only=False, theme="dark")), ("light", dict(width=80, color=True, ascii_only=False, theme="light"))]
VIEWS = ["watch", "timeline", "chart", "heatmap", "fill"]
ANSI = re.compile(r"\x1b\[[0-9;]*m")


class Goldens(unittest.TestCase):
    def check(self, view, mode, kw):
        out = draw(view, Env(**kw))
        path = os.path.join(TD, f"{view}-{mode}.golden")
        if os.environ.get("UPDATE_GOLDEN"):
            open(path, "w", encoding="utf-8").write(out)
        self.assertEqual(out, open(path, encoding="utf-8").read(), path)
        plain = ANSI.sub("", out)
        self.assertTrue(all(len(l) <= kw["width"] for l in plain.split("\n")), f"{view}/{mode}: a line is wider than {kw['width']}")
        self.assertEqual("\x1b" in out, kw["color"], f"{view}/{mode}: colour codes do not match the colour switch")
        if kw["ascii_only"]:
            out.encode("ascii")  # raises on any non-ASCII glyph
        self.assertNotRegex(plain.split("\n")[0], r"\b\d{4,}\b.*tokens", "raw token counts on the bottom line")


for _v in VIEWS:
    for _m, _kw in MODES:
        setattr(Goldens, f"test_{_v}_{_m}", lambda self, v=_v, m=_m, kw=_kw: self.check(v, m, kw))


class Behaviour(unittest.TestCase):
    def test_no_color_env_wins(self):
        os.environ["NO_COLOR"] = "1"
        try:
            self.assertFalse(Env(stream=type("S", (), {"isatty": lambda s: True, "encoding": "utf-8"})()).color)
        finally:
            del os.environ["NO_COLOR"]

    def test_unreachable_proxy_shows_the_last_picture(self):
        os.environ["XDG_CACHE_HOME"] = os.path.join(os.environ.get("TMPDIR", "/tmp"), "tvcache")
        o = {"now": NOW, "tz_min": 0, "days": 0, "session": "session-A", "details": False, "args": []}
        e = Env(width=80, color=False, ascii_only=False)
        first = tv.render("fill", Fx(""), o, e)
        down = tv.render("fill", type("D", (tv.Fast,), {"get": lambda s, *a, **k: None})(""), o, e)
        self.assertTrue(down.startswith(first.split("\n")[0]))
        self.assertIn("Proxy not answering", down)


if __name__ == "__main__":
    unittest.main()
