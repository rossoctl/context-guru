#!/usr/bin/env python3
"""Terminal views for Context Guru. Each answers one question, bottom line first, then a picture.

    watch     live full-screen: every cache's countdown, saved today, the last requests    (q quits)
    timeline  one session as a strip: turns, pauses, cache misses with their dollar cost, pings
    chart     saved per day, by lever; what is still avoidable is drawn apart, hatched
    heatmap   weekday x hour: when your pauses cost you
    fill      how full the context window is, what fills it, and when it will be full

Reads the same local proxy as cg.py (GETs only). Colour only on a terminal and never with NO_COLOR;
every view reads the same without it. --ascii for terminals without Unicode.
"""

from __future__ import annotations

import argparse
import datetime as dt
import json
import os
import re
import select
import shutil
import sys
import threading
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import cg  # noqa: E402  (Api, window, levers, daily, session_rows, is_rewrite, cache_state ...)
import tvlib as T  # noqa: E402
from tvlib import Env, dur, span, usd  # noqa: E402

DAY = cg.DAY_MS
WEEKDAYS = ["Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"]


class NoData(Exception):
    """The proxy did not answer the one document a view cannot draw without."""


class Fast(cg.Api):
    """cg.Api with a short timeout: a view must never hang on a slow proxy."""

    def get(self, path, params=None, timeout=4):
        return super().get(path, params, timeout)


def ctx(r: dict) -> int:
    return r["fresh_input"] + r["cache_read"] + r["cache_write"]


def kind(r: dict) -> str:
    """hit / part / miss from the token columns (cache_miss_reason is not trusted): how much of the
    prompt was re-written rather than read."""
    c = ctx(r)
    w = r["cache_write"] / c if c else 0
    return "miss" if w >= 0.8 else "part" if w >= 0.1 else "hit"


def thread_key(r: dict) -> tuple:
    return (r["model"], r["tools"], r["system_blocks"])


def short_model(m: str) -> str:
    return re.sub(r"-\d{8}$", "", m.split("/")[-1].replace("claude-", ""))


def extra_usd(rows: list[dict]) -> dict[int, float]:
    """Estimated extra dollars of each cache miss: its cost above a normal (non-miss) turn of the same
    model. The first request of a thread always writes, so it is not a miss anyone could have avoided."""
    ok: dict[str, list[float]] = {}
    for r in rows:
        if kind(r) != "miss":
            ok.setdefault(r["model"], []).append(r["cost_usd"])
    med = {m: sorted(v)[len(v) // 2] for m, v in ok.items()}
    seen, out = set(), {}
    for r in sorted(rows, key=lambda r: r["ts"]):
        k = thread_key(r)
        if kind(r) == "miss" and k in seen:
            out[r["id"]] = max(r["cost_usd"] - med.get(r["model"], 0.0), 0.0)
        seen.add(k)
    return out


def clock(ms: int, o: dict, fmt="%H:%M") -> str:
    return time.strftime(fmt, time.gmtime((ms + o["tz_min"] * 60_000) / 1000))


# ---------------------------------------------------------------------------------------------
# timeline

def view_timeline(api, o, e: Env) -> list[str]:
    sid, how = cg.session_id(api, o["session"])
    rows = cg.session_rows(api, sid) if sid else []
    if not rows:
        raise NoData
    allrows = rows
    t1 = rows[-1]["ts"]
    lo = max(rows[0]["ts"], t1 - 4 * 3600_000)
    rows = [r for r in rows if r["ts"] >= lo]
    ex = extra_usd(allrows)
    miss = [r for r in rows if r["id"] in ex]
    billed = sum(r["cost_usd"] for r in rows)
    lost = sum(ex[r["id"]] for r in miss)
    n = min(e.width - 2, 96)
    win = max(t1 - lo, 1)
    cell = lambda ts: min(n - 1, int((ts - lo) / win * n))  # noqa: E731
    strip = [e.g["idle"]] * n
    prev = None
    for r in rows:  # a pause longer than the cache lives is drawn as a line, shorter idle as blank
        if prev is not None and r["ts"] - prev["ts"] > 300_000:
            for c in range(cell(prev["ts"]) + 1, cell(r["ts"])):
                strip[c] = e.g["pause"]
        prev = r
    for r in rows:
        c = cell(r["ts"])
        strip[c] = e.g["msg"] if strip[c] in (e.g["idle"], e.g["pause"]) else e.g["busy"]
    comp, last = [], {}
    for r in allrows:  # a drop in message count on the same thread is a compaction or rewind
        k = thread_key(r)
        if k in last and last[k] > 20 and r["messages"] < 0.5 * last[k] and r["ts"] >= lo:
            comp.append(r)
        last[k] = r["messages"]
    for mark, grp in (("compact", comp), ("ping", [r for r in rows if r.get("keepalive")]), ("miss", miss)):
        for r in grp:
            strip[cell(r["ts"])] = e.g[mark]
    labels = [" "] * n  # dollar labels above the misses, skipped where they would overlap
    for r in sorted(miss, key=lambda r: -ex[r["id"]]):
        s, c = usd(ex[r["id"]]), cell(r["ts"])
        if c + len(s) <= n and all(x == " " for x in labels[max(0, c - 1):c + len(s) + 1]):
            labels[c:c + len(s)] = list(s)
    axis = [" "] * n
    for i in range(0, 5):
        t, c = clock(lo + win * i // 4, o), (n - 5) * i // 4
        axis[c:c + 5] = list(t)
    scope = "this session" if lo == allrows[0]["ts"] else "the last 4 hours"
    L = [(f"Cache misses cost about {usd(lost)} of {usd(billed)} billed in {scope} ({len(miss)} of {len(rows)} messages)."
          if miss else f"No cache misses in {scope}: {usd(billed)} billed. Nothing to fix."), ""]
    tone = {e.g["miss"]: "bad", e.g["compact"]: "warn", e.g["ping"]: "info"}
    paint = lambda s: "".join(e.paint(tone[c], c) if c in tone else c for c in s)  # noqa: E731
    L += ["".join(labels).rstrip(), paint(strip), e.paint("dim", "".join(axis).rstrip()), ""]
    L += [f"{e.g['msg']} message  {e.g['busy']} busy  {e.g['miss']} cache miss (extra cost)  {e.g['compact']} compacted  "
          f"{e.g['ping']} keep-alive ping  {e.g['pause']} pause over 5 min"]
    if miss:
        L += ["", e.paint("bold", "Expensive moments")]
        by_id = {r["id"]: r for r in allrows}
        gaps, prev_ts = {}, {}
        for r in allrows:  # the pause before a request is measured on its own thread (subagents interleave)
            gaps[r["id"]] = r["ts"] - prev_ts.get(thread_key(r), r["ts"])
            prev_ts[thread_key(r)] = r["ts"]
        L += T.cols([[clock(r["ts"], o), usd(ex[r["id"]]), f"after a {span(gaps.get(r['id'], 0) / 1000)} pause" if gaps.get(r["id"], 0) > 300_000
                      else "rewritten mid-conversation"] for r in sorted(miss, key=lambda r: -ex[r["id"]])[:3] if r["id"] in by_id], "lrl")
    L += ["", e.paint("dim", "Extra = cost above a normal turn of the same model (estimate).")]
    return L


# ---------------------------------------------------------------------------------------------
# chart

def vbars(vals: list[float], e: Env, height: int, w: int) -> list[str]:
    top = max(vals + [0.0]) or 1.0
    eighths = [round(v / top * height * 8) for v in vals]
    out = []
    for r in range(height):  # top row first
        row = ""
        for q in eighths:
            f = max(0, min(8, q - (height - 1 - r) * 8))
            ch = " ▁▂▃▄▅▆▇█"[f] if not e.ascii else ("#" if f >= 4 or (f > 0 and r == height - 1) else " ")
            row += ch * (w - 1 if w > 1 else 1) + (" " if w > 1 else "")
        out.append(row.rstrip())
    return out


def view_chart(api, o, e: Env) -> list[str]:
    d = o["days"] or 14
    since, now = cg.window(o, d)
    st = cg.stats(api, since, now)
    if not st:
        raise NoData
    vals = cg.daily(api, since, now, "saved_usd")[-d:]
    saved, bill = st["total_saved_usd"], st["cost_usd"]
    best = max(range(len(vals)), key=vals.__getitem__) if vals else 0
    day0 = since // DAY
    wk = lambda i: WEEKDAYS[(day0 + i + 3) % 7]  # noqa: E731  (1970-01-01 was a Thursday)
    L = [f"Saved {usd(saved)} in {d} days, {cg.share(saved, st.get('baseline_cost_usd') or bill)} of what the same work would have cost. "
         f"Best day {wk(best)} {usd(vals[best])}.", ""]
    w = 3 if d * 3 + 8 <= e.width else 2 if d * 2 + 8 <= e.width else 1
    top = max(vals + [0.0])
    H = 6
    L += [e.paint("ok", e.paint("bold", f"Saved per day")) + e.paint("dim", "  observed, net of Context Guru's own spend")]
    for i, line in enumerate(vbars(vals, e, H, w)):
        axis = usd(top) if i == 0 else usd(top / 2) if i == H // 2 else ""
        L.append(f"{axis:>7} " + e.paint("ok", line))
    ticks = [" "] * (d * w)
    for i in range(0, d, 7 if w == 1 else 1):
        ticks[i * w] = wk(i)[0]
    L.append(" " * 8 + e.paint("dim", "".join(ticks).rstrip()))
    L.append(" " * 8 + e.paint("dim", clock(since, o, "%b %d") + " " * max(1, d * w - 14) + clock(now, o, "%b %d")))
    lv = cg.levers(st)
    big = max([abs(v) for _, v in lv] + [0.01])
    L += ["", e.paint("bold", "By lever") + e.paint("dim", "  observed, same window")]
    L += T.cols([[n, usd(v), (e.bar(v / big, 14, "ok") if v > 0 else "")] for n, v in lv] or [["Nothing saved yet", "", ""]], "lrl")
    sp = cg.spend_doc(api, o, d)
    recs = cg.C.recommendations(sp, None, d)[:3] if sp else []
    est = [[r["short"], usd(r["usd"]), e.bar(r["usd"] / recs[0]["usd"], 14, "warn", hatch=True)] for r in recs]
    if est:
        L += ["", e.paint("bold", f"{'~' if e.ascii else '≈'} Still avoidable") + e.paint("dim", "  estimated, not saved; rows overlap, do not add")]
        L += T.cols(est, "lrl", maxw=e.width - 1)
    return L


# ---------------------------------------------------------------------------------------------
# heatmap

def view_heatmap(api, o, e: Env) -> list[str]:
    d = o["days"] or 28
    since, now = cg.window(o, d)
    rows, before = [], 0
    for _ in range(12):  # newest first, 500 a page: at most 6,000 requests, and we say so
        p = {"since": since, "limit": 500, **({"before": before} if before else {})}
        page = api.get("/api/requests", p) or {}
        rows += page.get("requests") or []
        before = page.get("next_cursor") or 0
        if not before:
            break
    if not rows:
        raise NoData
    ex = extra_usd(rows)
    grid = [[0.0] * 24 for _ in range(7)]
    cnt = 0
    for r in rows:
        if r["id"] in ex:
            t = time.gmtime((r["ts"] + o["tz_min"] * 60_000) / 1000)
            grid[t.tm_wday][t.tm_hour] += ex[r["id"]]
            cnt += 1
    tot = sum(map(sum, grid))
    top = max(max(g) for g in grid)
    covered_from = min(r["ts"] for r in rows)
    if tot <= 0:
        return [f"No cache misses from pauses in {d} days. Nothing to fix.", ""]
    wd, hr = max(((a, b) for a in range(7) for b in range(24)), key=lambda x: grid[x[0]][x[1]])
    L = [f"Pauses and rewrites cost you about {usd(tot)} in {d} days. Costliest hour: {WEEKDAYS[wd]} {hr:02d}:00 ({usd(grid[wd][hr])}).", ""]
    cw = 2 if e.width >= 24 * 2 + 16 else 1
    head = [" "] * (24 * cw)
    for h in range(0, 24, 3 if cw == 2 else 6):
        head[h * cw:h * cw + len(str(h))] = str(h)
    L.append("    " + e.paint("dim", "".join(head).rstrip()))
    for i in range(7):
        L.append(f"{WEEKDAYS[i]} " + "".join(e.heat_cell(v, top, cw) for v in grid[i]) + e.paint("dim", f"  {usd(sum(grid[i])):>7}"))
    L += ["", "Scale  " + "".join(e.heat_cell(top * f, top, 2) for f in (0, .25, .5, .75, 1.0)) + f"  {usd(0)} to {usd(top)} per hour slot"]
    L += [e.paint("dim", f"Estimated extra per miss; {cnt} misses in the latest {len(rows):,} requests since {clock(covered_from, o, '%b %d')}.")]
    return L


# ---------------------------------------------------------------------------------------------
# fill

def growth(rows: list[dict]) -> float:
    """Context tokens per second over the latest stretch of one thread since its last compaction."""
    pts = []
    for r in rows:
        if pts and r["messages"] < pts[-1][2] * 0.5:
            pts = []
        pts.append((r["ts"], ctx(r), r["messages"]))
    pts = pts[-12:]
    if len(pts) < 3 or pts[-1][0] <= pts[0][0]:
        return 0.0
    return max(pts[-1][1] - pts[0][1], 0) / ((pts[-1][0] - pts[0][0]) / 1000)


def view_fill(api, o, e: Env) -> list[str]:
    sid, _ = cg.session_id(api, o["session"])
    rows = cg.session_rows(api, sid) if sid else []
    if not rows:
        raise NoData
    cs = api.get("/api/cachestate", {"session": sid}) or {}
    win = cs.get("window") or 0
    main = max({thread_key(r) for r in rows}, key=lambda k: sum(1 for r in rows if thread_key(r) == k))
    th = [r for r in rows if thread_key(r) == main]
    cur, base = ctx(th[-1]), ctx(th[0])
    if not win:
        return ["Context window size is not known for this model, so there is no gauge."]
    frac = cur / win
    g = growth(th)
    eta = (win - cur) / g if g > 0 else None
    cold = cs.get("rewrite_usd") if cs.get("priced") else None
    pct = f"{frac * 100:.0f}%"
    bl = f"Context is {pct} full" + (f"; at this pace it is full in {span(eta)}" if eta and eta < 6 * 3600 else "; not filling fast") + \
         (f". A cache miss now costs {usd(cold)}." if cold else ".")
    kindc = "bad" if frac >= 0.85 else "warn" if frac >= 0.65 else "ok"
    n = min(e.width - 8, 60)
    base_f = min(base / win, frac)
    fx, cv = ("=", "#") if e.ascii else ("▒", "█")
    L = [bl, "", e.paint(kindc, e.stack([(base_f, fx), (frac - base_f, cv)], n)) + f" {pct}",
         f"{fx} fixed start (instructions and tools)   {cv} conversation so far   {e.g['empty']} free"]
    L += [""] + T.cols([["Fixed start", f"{base / win * 100:.0f}%"], ["Conversation", f"{(cur - base) / win * 100:.0f}%"],
                        ["Free", f"{(1 - frac) * 100:.0f}%"]], "lr")
    if frac >= 0.8:
        L += ["", f"{e.paint('warn', e.g['up'])} Nearly full: compact before a long pause, so the next turn does not rewrite everything."]
    return L


# ---------------------------------------------------------------------------------------------
# watch

class Snap:
    """What watch last learned. The fetch thread replaces fields; the draw loop only reads, so a slow
    proxy slows the data, never the screen."""

    def __init__(self) -> None:
        self.at = 0
        self.err = False
        self.saved_today = 0.0
        self.hourly: list[float] = []
        self.recent: list[dict] = []
        self.sessions: list[dict] = []  # {sid, model, ttl, window, rewrite, priced, threads: [{label, model, last, ctx, ping}]}
        self.sess_at = 0.0
        self.slow_at = 0.0
        self.proj = None


def fetch(api, o, s: Snap, now: int | None = None) -> None:
    now = now or int(time.time() * 1000)
    since = cg.window({**o, "now": now}, 1)[0]
    if time.time() - s.slow_at > 30:
        st = cg.stats(api, since, now)
        sr = api.get("/api/series", {"bucket": 3600_000, "since": since, "until": now})
        if st:
            s.saved_today = st["total_saved_usd"]
        if sr:
            s.hourly = [b.get("saved_usd", 0.0) for b in sr.get("buckets", [])]
        s.slow_at = time.time()
    rec = api.get("/api/requests", {"limit": 20})
    if time.time() - s.sess_at > 20 or not s.sessions:
        ls = (api.get("/api/sessions", {"limit": 8}) or {}).get("sessions") or []
        s.sessions = [{"sid": x["session_id"]} for x in ls if now - x["end"] < 3600_000 * 2]
        s.sess_at = time.time()
    if s.proj is None:
        s.proj = cg.C.Projects()
    out = []
    for x in s.sessions:
        name = s.proj.name(x["sid"])
        cs = api.get("/api/cachestate", {"session": x["sid"]})
        rr = (api.get("/api/requests", {"session": x["sid"], "limit": 60}) or {}).get("requests") or []
        if not cs or cs.get("none") or not rr:
            continue
        ths: dict = {}
        for r in sorted(rr, key=lambda r: r["ts"]):
            ths[thread_key(r)] = r
        ordered = sorted(ths.values(), key=lambda r: -r["messages"])
        out.append({"sid": x["sid"], "name": name if name[0] != "(" else x["sid"][-8:], "ttl": cs["ttl_s"], "window": cs.get("window") or 0, "rewrite": cs.get("rewrite_usd"),
                    "priced": cs.get("priced"), "prefix": cs.get("prefix_tokens") or 1, "skew": now - cs["now_ms"],
                    "threads": [{"label": "main" if i == 0 else f"sub-{i}", "model": r["model"], "last": r["ts"], "ctx": ctx(r),
                                 "ping": bool(r.get("keepalive"))} for i, r in enumerate(ordered[:3])]})
    s.sessions = out or s.sessions
    if rec:
        s.recent = rec.get("requests") or []
    s.err = rec is None
    s.at = now


def frame(s: Snap, o: dict, e: Env, now_ms: int) -> list[str]:
    return T.fit(_frame(s, o, e, now_ms), e)


def _frame(s: Snap, o: dict, e: Env, now_ms: int) -> list[str]:
    """One screen from a Snap. Pure: `now_ms` is passed in so tests freeze the clock."""
    live = []
    for x in s.sessions:
        for t in x["threads"]:
            live.append((x, t, (t["last"] + x["ttl"] * 1000 - now_ms) / 1000))
    cold = [(x, t) for x, t, rem in live if rem <= 0 and t["label"] == "main"]
    soon = [1 for x, t, rem in live if 0 < rem <= 60]
    at_risk = sum((x["rewrite"] or 0) for x, t in cold)
    age = int((now_ms - s.at) / 1000) if s.at else 0
    bl = f"{usd(s.saved_today)} saved today. " + (f"{len(cold)} cache{'s' if len(cold) != 1 else ''} cold: next message costs {usd(at_risk)} extra." if cold
                                                  else f"{len(soon)} expiring within a minute." if soon else "All caches warm.")
    L = [e.paint("bold", bl), ""]
    L.append(e.paint("bold", "Caches") + e.paint("dim", "  time until a full rewrite"))
    rows = []
    for x, t, rem in live:
        kc = "bad" if rem <= 0 else "warn" if rem <= 60 else "ok"
        icon = e.g["off"] if rem <= 0 else e.g["dim"] if rem <= 60 else e.g["dot"]
        fillp = f"{t['ctx'] / x['window'] * 100:.0f}%" if x["window"] else ""
        risk = usd((x["rewrite"] or 0) * t["ctx"] / x["prefix"]) if x["priced"] and rem <= 120 else ""
        cells = [e.paint(kc, icon), f"{x['name']}/{t['label']}", short_model(t["model"])[:12],
                 e.paint(kc, e.bar(max(rem, 0) / x["ttl"], 12)), e.paint(kc, "COLD" if rem <= 0 else dur(rem)),
                 e.g["ping"] if t["ping"] else " ", f"ctx {fillp}" if fillp else "", risk and f"{risk} if cold"]
        rows.append(cells)
    drop = [i for i, floor in ((7, 84), (6, 72), (2, 64)) if e.width < floor]  # narrow terminal: lose risk, then ctx, then model
    rows = [[c for i, c in enumerate(r) if i not in drop] for r in rows]
    L += T.cols(rows, "llllrl" + "l" * (len(rows[0]) - 6 if rows else 0)) or [e.paint("dim", "  No live session in the last 2 hours.")]
    L += ["", e.paint("bold", "Saved today ") + f"{usd(s.saved_today)}  " + e.paint("ok", e.spark(s.hourly))]
    L += ["", e.paint("bold", "Last 20 requests") + e.paint("dim", "  oldest to newest")]
    strip = ""
    for r in sorted(s.recent, key=lambda r: r["ts"]):
        k = kind(r)
        strip += e.paint({"hit": "ok", "part": "warn", "miss": "bad"}[k], e.g[k if k != "part" else "part"])
        strip += e.g["ping"] if r.get("keepalive") else " "
    L += [strip.rstrip(), f"{e.paint('ok', e.g['hit'])} hit  {e.paint('warn', e.g['part'])} partial  {e.paint('bad', e.g['miss'])} miss  {e.g['ping']} ping"]
    L += ["", e.paint("dim", f"updated {age}s ago" + ("  proxy not answering, showing the last data" if s.err else "") + "   q quit  r refresh")]
    return L


def watch(api, o, e: Env, once: bool, hint: bool = False) -> None:
    s = Snap()
    fetch(api, o, s)
    if once or not sys.stdin.isatty() or not sys.stdout.isatty():
        print("\n".join(frame(s, o, e, int(time.time() * 1000))))
        if hint:
            print(f"\nLive: python3 {os.path.abspath(__file__)} watch   (q quits)")
        return
    import termios
    import tty
    stop = threading.Event()

    def loop():
        while not stop.is_set():
            fetch(api, o, s)
            stop.wait(5)
    threading.Thread(target=loop, daemon=True).start()
    fd = sys.stdin.fileno()
    old = termios.tcgetattr(fd)
    out = sys.stdout
    out.write("\x1b[?1049h\x1b[?25l")
    try:
        tty.setcbreak(fd)
        while True:
            e.width = max(40, min(shutil.get_terminal_size((80, 24)).columns, 140))  # follows a resize
            lines = frame(s, o, e, int(time.time() * 1000))[:shutil.get_terminal_size((80, 24)).lines - 1]
            out.write("\x1b[H" + "\x1b[K\n".join(lines) + "\x1b[K\x1b[J")
            out.flush()
            if select.select([sys.stdin], [], [], 1.0)[0]:
                k = sys.stdin.read(1)
                if k in ("q", "Q", "\x03"):
                    return
                if k == "r":
                    s.slow_at = s.sess_at = 0
    finally:
        stop.set()
        termios.tcsetattr(fd, termios.TCSADRAIN, old)
        out.write("\x1b[?25h\x1b[?1049l")
        out.flush()


# ---------------------------------------------------------------------------------------------

VIEWS = {k: (lambda f: lambda api, o, e: T.fit(f(api, o, e), e))(f)
         for k, f in {"timeline": view_timeline, "chart": view_chart, "heatmap": view_heatmap, "fill": view_fill}.items()}


def cache_path(view: str, key: str) -> str:
    base = os.environ.get("XDG_CACHE_HOME") or os.path.expanduser("~/.cache")
    return os.path.join(base, "context-guru", f"tv-{view}-{key or 'all'}.txt")


def render(view: str, api, o: dict, e: Env) -> str:
    """Draw a view; remember it. If the proxy does not answer, show the last good drawing and say how old it is."""
    p = cache_path(view, o["session"] or "")
    try:
        out = "\n".join(VIEWS[view](api, o, e))
    except NoData:
        try:
            with open(p, encoding="utf-8") as f:
                ts, body = f.read().split("\n", 1)
            return body + "\n\n" + e.paint("warn", f"Proxy not answering. Showing the picture from {dur(time.time() - float(ts))} ago.")
        except (OSError, ValueError):
            return "No data yet. Run a few prompts through the proxy, then try again."
    try:
        os.makedirs(os.path.dirname(p), exist_ok=True)
        with open(p, "w", encoding="utf-8") as f:
            f.write(f"{time.time()}\n{out}")
    except OSError:
        pass
    return out


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("view", choices=["watch", *sorted(VIEWS)])
    ap.add_argument("days_arg", nargs="?", type=int, default=0, metavar="DAYS", help="chart and heatmap: days to cover")
    ap.add_argument("--days", type=int, default=0)
    ap.add_argument("--hint", action="store_true", help="watch --once: add the command that runs the live view")
    ap.add_argument("--port")
    ap.add_argument("--session")
    ap.add_argument("--once", action="store_true", help="watch: draw one frame and exit")
    ap.add_argument("--ascii", action="store_true", default=None)
    ap.add_argument("--no-color", action="store_true")
    ap.add_argument("--theme", choices=["dark", "light"])
    ap.add_argument("--width", type=int)
    ap.add_argument("--fixture")
    ap.add_argument("--now")
    ap.add_argument("--tz-offset-min", type=int)
    a = ap.parse_args(argv)
    o: dict = {"now": int(time.time() * 1000), "days": a.days or a.days_arg, "session": a.session, "details": False, "args": [],
               "tz_min": a.tz_offset_min if a.tz_offset_min is not None else time.localtime().tm_gmtoff // 60}
    if a.now:
        o["now"] = int(dt.datetime.fromisoformat(a.now.replace("Z", "+00:00")).timestamp() * 1000)
    e = Env(color=False if a.no_color else None, ascii_only=a.ascii, width=a.width, theme=a.theme)
    if a.fixture:
        with open(a.fixture, encoding="utf-8") as f:
            api = Fast("", json.load(f))
    else:
        base = cg.proxy_base(a.port)
        if not base:
            print(cg.NO_PROXY)
            return 0
        api = Fast(base)
    if a.view == "watch":
        watch(api, o, e, a.once, a.hint)
    else:
        print(render(a.view, api, o, e))
    return 0


if __name__ == "__main__":
    sys.exit(main())
