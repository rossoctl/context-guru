#!/usr/bin/env python3
"""The /context-guru:* commands. Each answers ONE question: a bottom line, then a small picture, then (with
`details`) the tables behind it. Dollars first, gateway prices, no raw token counts unless asked.

    home      what is my situation, what should I do next
    savings   how much did Context Guru save me                  (saved and estimated kept apart)
    cache     is my cache about to cost me
    waste     what do I carry and never use
    session   what happened in this session
    doctor    is it set up right
    adapt     which cache / compaction / tools setting would have cost least on my own history

Read-only: GETs against the local proxy, except `adapt <kind> apply`, which the user confirms first. Output is
plain Unicode, no colour codes, so it reads the same on a light and a dark terminal and survives
being echoed by the model. Stdlib only; `run(cmd, api, opts)` takes the fetcher so tests feed it a
captured bundle.
"""

from __future__ import annotations

import argparse
import concurrent.futures
import datetime as dt
import json
import os
import re
import shlex
import socket
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import cgcost as C  # noqa: E402  (usd, spark, bar, table, recommendations, unused_items ...)

DAY_MS = 86_400_000
BAR = 12


def cells(frac: float, n: int = BAR) -> str:
    return C.bar(max(0.0, min(1.0, frac)), n)


def dollars(x: float) -> str:
    """Two decimals so a column lines up; sub-cent amounts say so rather than print $0."""
    return f"-${abs(x):,.2f}" if x <= -0.005 else (f"${x:,.2f}" if x >= 0.005 else "$0")


def share(a: float, b: float) -> str:
    p = 100 * a / b if b > 0 else 0.0
    return f"{p:.0f}%" if p >= 10 else f"{p:.1f}%"


# ---------------------------------------------------------------------------------------------
# the proxy

class Api:
    """GETs the local proxy. `fixture` (a dict path -> body) replaces the network for tests."""

    def __init__(self, base: str, fixture: dict | None = None) -> None:
        self.base, self.fixture = base.rstrip("/"), fixture

    def get(self, path: str, params: dict | None = None, timeout: float = 30) -> dict | None:
        if self.fixture is not None:
            return self.fixture.get(path)
        q = "?" + urllib.parse.urlencode(params) if params else ""
        try:
            with urllib.request.urlopen(self.base + path + q, timeout=timeout) as r:
                return json.loads(r.read())
        except (OSError, ValueError):
            return None

    def send(self, method: str, path: str, params: dict | None = None, body: dict | None = None,
             timeout: float = 30) -> tuple[int, dict | None]:
        """Like get, but keeps the HTTP status so a failure can be told apart: 0 = no answer, -1 = timed out."""
        if self.fixture is not None:
            doc = self.fixture.get(path if method == "GET" else f"{method} {path}")
            return (200, doc) if doc is not None else (404, None)
        q = "?" + urllib.parse.urlencode(params) if params else ""
        data = json.dumps(body).encode() if body is not None else None
        req = urllib.request.Request(self.base + path + q, data=data, method=method,
                                     headers={"Content-Type": "application/json"} if data else {})
        try:
            with urllib.request.urlopen(req, timeout=timeout) as r:
                return r.status, json.loads(r.read() or b"null")
        except urllib.error.HTTPError as e:
            try:
                return e.code, json.loads(e.read())
            except ValueError:
                return e.code, None
        except socket.timeout:
            return -1, None
        except urllib.error.URLError as e:
            return (-1 if isinstance(e.reason, socket.timeout) else 0), None
        except (OSError, ValueError):
            return 0, None


def proxy_base(port: str | None) -> str | None:
    """--port, else $CG_URL, else the loopback host:port of $ANTHROPIC_BASE_URL (what routes this session)."""
    if port:
        return f"http://127.0.0.1:{port}"
    if os.environ.get("CG_URL"):
        return os.environ["CG_URL"]
    for var in ("ANTHROPIC_BASE_URL", "OPENAI_BASE_URL"):  # Claude Code, then Codex
        m = re.match(r"^(https?://(?:127\.0\.0\.1|localhost)(?::\d+)?)", os.environ.get(var, ""))
        if m:
            return m.group(1)
    return None


NO_PROXY = ("No Context Guru proxy is routing this session (no local ANTHROPIC_BASE_URL or OPENAI_BASE_URL).\n"
            "Run /context-guru:install, or point at one with CG_URL=http://127.0.0.1:PORT.")


# ---------------------------------------------------------------------------------------------
# shared reads

def window(o: dict, days: int) -> tuple[int, int]:
    now, off = o["now"], o["tz_min"] * 60_000
    midnight = (now + off) // DAY_MS * DAY_MS - off
    return midnight - (days - 1) * DAY_MS, now


def stats(api: Api, since: int, until: int | None = None, session: str | None = None) -> dict | None:
    # The FULL endpoint, not lean=1. #502 made lean a bounded path by moving the
    # pre-instrumentation cachesplit credit and the declaration-filter credit out of it for
    # anything wider than a single session — every call this plugin makes is a day/week/month
    # window, never a session, so lean would quietly understate total_saved_usd here. These
    # commands run once per invocation, not on every status-line render, so the full handler's
    # extra cost (bounded now, see #492/#498/#502) is the right trade for an exact number.
    p: dict = {"since": since}
    if until:
        p["until"] = until
    if session:
        p["session"] = session
    return api.get("/api/stats", p)


def levers(st: dict) -> list[tuple[str, float]]:
    """Saved dollars by lever, from the stats waterfall. Own spend is subtracted, not hidden."""
    w = {x["key"]: x["delta_usd"] for x in st.get("waterfall", [])}
    rows = [("Compaction", -w.get("compaction", 0.0)), ("Cache split", w.get("cachesplit_saved", 0.0)),
            ("Keep-alive pings", w.get("keepalive_saved", 0.0) - abs(w.get("keepalive_ping", 0.0))),
            ("Tool filtering", w.get("decl_filter_saved", 0.0)), ("Self-removed content", w.get("self_removed", 0.0)),
            ("Context Guru's own model spend", -abs(w.get("cg_llm", 0.0)))]
    return [(n, v) for n, v in rows if abs(v) >= 0.005]


def daily(api: Api, since: int, until: int, key: str) -> list[float]:
    b = (api.get("/api/series", {"bucket": DAY_MS, "since": since, "until": until}) or {}).get("buckets", [])
    days: dict[int, float] = {}
    for x in b:
        d = x["ts"] // DAY_MS
        days[d] = days.get(d, 0.0) + x.get(key, 0.0)
    first, last = since // DAY_MS, until // DAY_MS
    return [days.get(d, 0.0) for d in range(first, last + 1)]


def spend_doc(api: Api, o: dict, days: int) -> dict | None:
    since, now = window(o, days)
    sp = api.get("/api/spend/explain", {"since": since, "tz": o["tz_min"], "now": now}, 60)
    if sp:
        sp["window"].update(since=since, until=now)
    return sp


def tools_doc(api: Api, o: dict, days: int) -> tuple[dict | None, dict | None]:
    since, now = window(o, days)
    return api.get("/api/tools", {"since": since, "until": now}, 60), api.get("/api/tools/last-used")


def cache_state(api: Api, session: str | None) -> dict | None:
    cs = api.get("/api/cachestate", {"session": session}) if session else None
    return cs if cs and not cs.get("none") and cs.get("last_ts") else None


def remaining(cs: dict) -> float:
    return (cs["last_ts"] + cs["ttl_s"] * 1000 - cs["now_ms"]) / 1000


def session_id(api: Api, a_session: str | None) -> tuple[str | None, str]:
    """(id, how we know). Claude Code exports its session id to the commands it runs."""
    sid = a_session or os.environ.get("CLAUDE_CODE_SESSION_ID")
    if sid:
        return sid, "this session"
    rows = (api.get("/api/sessions", {"limit": 1}) or {}).get("sessions") or []
    return (rows[0]["session_id"], "most recent session") if rows else (None, "")


# ---------------------------------------------------------------------------------------------
# /context-guru:status

def cmd_home(api: Api, o: dict) -> list[str]:
    d = o["days"] or 7
    since, now = window(o, d)
    st = stats(api, since, now)
    if not st:
        return ["No data yet. Run a few prompts through the proxy, then try again."]
    saved, bill = st.get("total_saved_usd", 0.0), st.get("cost_usd", 0.0)
    sp, (tools, last) = spend_doc(api, o, d), tools_doc(api, o, d)
    recs = C.recommendations(sp, tools, d) if sp else []
    L = [f"Saved {dollars(saved)} in {d} days ({share(saved, st.get('baseline_cost_usd') or bill)} of what the same work would have cost)."]
    t0 = window(o, 1)[0]
    today, yest = stats(api, t0, now), stats(api, t0 - DAY_MS, t0)
    spend_d, saved_d = daily(api, since, now, "cost_usd"), daily(api, since, now, "saved_usd")
    L += ["", "            saved     spent"]
    if today:
        L.append(f"Today     {dollars(today['total_saved_usd']):>8}  {dollars(today['cost_usd']):>8}")
    if yest:
        L.append(f"Yesterday {dollars(yest['total_saved_usd']):>8}  {dollars(yest['cost_usd']):>8}")
    L.append(f"{d} days   {dollars(saved):>8}  {dollars(bill):>8}   saved {C.spark(saved_d)}  spent {C.spark(spend_d)}")
    L += ["", "≈ Estimated   (spend you could still avoid; rows overlap, do not add)"]
    for i, r in enumerate(recs[:3], 1):
        L.append(f"  {i}. {dollars(r['usd']):>8}  {r['short']}: {r['do']}")
    if not recs:
        L.append("  Nothing worth acting on right now.")
    L += ["", "/context-guru:savings  /context-guru:cache  /context-guru:waste  /context-guru:session  /context-guru:doctor",
          "/context-guru:watch  /context-guru:timeline  /context-guru:chart  /context-guru:heatmap  /context-guru:fill"]
    return L


# ---------------------------------------------------------------------------------------------
# /context-guru:savings

def cmd_savings(api: Api, o: dict) -> list[str]:
    d = o["days"] or 7
    since, now = window(o, d)
    st = stats(api, since, now)
    if not st:
        return ["No data yet. Run a few prompts through the proxy, then try again."]
    saved, bill = st["total_saved_usd"], st["cost_usd"]
    L = [f"Saved {dollars(saved)} in {d} days, net of Context Guru's own spend "
         f"({share(saved, st.get('baseline_cost_usd') or bill)} of the {dollars(st.get('baseline_cost_usd') or bill)} "
         f"the same work would have cost)."]
    lv = levers(st)
    top = max([abs(v) for _, v in lv] + [0.01])
    L += ["", f"✓ Saved   (vs the same requests without Context Guru, at gateway prices)  {C.spark(daily(api, since, now, 'saved_usd'))}"]
    L += C.table([[n, dollars(v), cells(max(v, 0) / top)] for n, v in lv] or [["Nothing saved yet", "", ""]], "lrl")
    est = []
    sp = spend_doc(api, o, d)
    if sp:
        for r in C.recommendations(sp, None, d)[:2]:
            est.append([r["short"], dollars(r["usd"]), r["cls"].split(";")[0]])
    if est:
        L += ["", "≈ Estimated   (spend you could still avoid; rows overlap, do not add them)"] + C.table(est, "lrl")
    if o["details"]:
        L += ["", f"Billed {dollars(bill)}; prompt cache alone saved a further {dollars(st.get('cache_saved_usd', 0))} "
                  "versus no caching at all (a provider feature, not counted above)."]
    return L


# ---------------------------------------------------------------------------------------------
# /context-guru:cache

CAUSES = {"idle_expiry": "You paused longer than the cache lasts", "prefix_change": "Settings or tools changed mid-session",
          "cold_start": "A new session started", "new_thread": "A helper agent started",
          "history_rewrite": "The conversation was rewound or compacted"}


def cmd_cache(api: Api, o: dict) -> list[str]:
    sid, _ = session_id(api, o["session"])
    cs = cache_state(api, sid)
    d = o["days"] or 7
    sp = spend_doc(api, o, d)
    if cs is None:
        L = ["No live cache for this session yet. The first message writes it."]
    else:
        rem, ttl = remaining(cs), cs["ttl_s"]
        cost = f"{dollars(cs['rewrite_usd'])}" if cs.get("priced") else "an unpriced amount"
        L = [f"Cache is warm for {C.fmt_dur(rem)} more. If it lapses, the next message costs {cost} extra."
             if rem > 0 else f"Cache is COLD. The next message re-sends the whole conversation: {cost} extra."]
        ping = "ping planned ↻" if cs.get("keepalive") else "no ping planned"
        L += ["", f"{'●' if rem > 60 else '◐' if rem > 0 else '○'} {cells(rem / ttl, 20)} {C.fmt_dur(max(rem, 0))} of {C.fmt_dur(ttl)}   {ping}"]
    rw = (sp or {}).get("rewrites", {})
    if rw:
        tot = sum(v["usd"] for v in rw.values())
        L += ["", f"Cache misses in {d} days: {sum(v['n'] for v in rw.values())} costing {dollars(tot)} extra   (measured)"]
        L += C.table([[CAUSES.get(k, k.replace("_", " ")), str(v["n"]) + "x", dollars(v["usd"]), cells(v["usd"] / tot, 10)]
                      for k, v in sorted(rw.items(), key=lambda kv: -kv[1]["usd"])], "lrrl")
    L += ["", "The cache keeps your conversation cheap to reuse for a few minutes after each message."]
    if o["details"] and sp:
        L += [""] + C.cmd_cold(sp, {**o, "label": f"in the last {d} days"}, C.Style(False), C.Projects())
    return L


# ---------------------------------------------------------------------------------------------
# /context-guru:waste

def cmd_waste(api: Api, o: dict) -> list[str]:
    d = o["days"] or 7
    tools, last = tools_doc(api, o, d)
    if not tools or not tools.get("totals"):
        return ["No tool inventory yet. The proxy records it per session; try again after a few sessions."]
    items = C.unused_items(tools, last)
    if not items:
        return ["Nothing wasted: every tool, MCP server and skill you carry was used in this window."]
    carried = sum(i["usd"] for i in items)
    per_month = carried / d * 30
    L = [f"{len(items)} things you carry were never used: {dollars(carried)} in {d} days, "
         f"about {dollars(per_month)} a month.", ""]
    top = items[:5]
    kind = lambda k: "built-in tool" if k == "client tool" else k  # noqa: E731
    L += [f"Top {len(top)} of {len(items)}:"] + C.table([[kind(i["kind"]), i["name"], dollars(i["usd"]), cells(i["usd"] / items[0]["usd"], 10)] for i in top], "llrl")
    L += ["", "Remove the first:", f"  {top[0]['cmd']}"]
    if o["details"]:
        L += ["", "All of them, with the command that removes each:"]
        L += C.table([[kind(i["kind"]), i["name"], dollars(i["usd"]), i["cmd"]] for i in items], "llrl")
    L += ["", f"Measured carry; the monthly figure is an estimate that assumes your usage stays the same."]
    return L


# ---------------------------------------------------------------------------------------------
# /context-guru:session

STRIP = 48
STRIP_MS = 4 * 3600 * 1000  # the strip shows at most the last four hours; longer sessions say so


def session_rows(api: Api, sid: str) -> list[dict]:
    """Newest 2000 turns of a session, oldest first (the API pages newest-first, 500 at a time)."""
    rows: list[dict] = []
    before = 0
    for _ in range(4):
        p = {"session": sid, "limit": 500, **({"before": before} if before else {})}
        page = api.get("/api/requests", p) or {}
        rows += page.get("requests") or []
        before = page.get("next_cursor") or 0
        if not before:
            break
    return sorted(rows, key=lambda r: r["ts"])


def is_rewrite(r: dict) -> bool:
    """A full-prefix rewrite, read from the token columns (cache_miss_reason is not trusted)."""
    ctx = r["fresh_input"] + r["cache_read"] + r["cache_write"]
    return ctx > 0 and r["cache_write"] >= 0.8 * ctx


def cmd_session(api: Api, o: dict) -> list[str]:
    sid, how = session_id(api, o["session"])
    if not sid:
        return ["No session found on this proxy yet."]
    rows = session_rows(api, sid)
    if not rows:
        return [f"No requests recorded yet for {how}."]
    t1 = rows[-1]["ts"]
    lo = max(rows[0]["ts"], t1 - STRIP_MS)
    whole = lo == rows[0]["ts"]
    allrows, rows = rows, [r for r in rows if r["ts"] >= lo]  # one window for the verdict and the strip
    span = max(t1 - lo, 1)
    misses = [r for r in rows[1:] if is_rewrite(r)]  # the first request of a session always writes
    gaps = [(b["ts"] - a["ts"], a, b) for a, b in zip(rows, rows[1:])]
    compactions, prev = [], {}  # per thread (a session interleaves subagents; their counts are not comparable)
    for r in allrows:
        k = (r["tools"], r["system_blocks"], r["model"])
        if k in prev and prev[k] > 20 and r["messages"] < 0.5 * prev[k] and r["ts"] >= lo:
            compactions.append(r)
        prev[k] = r["messages"]
    ok = [r["cost_usd"] for r in rows if not is_rewrite(r)]
    typical = sorted(ok)[len(ok) // 2] if ok else 0.0
    extra = sum(max(r["cost_usd"] - typical, 0.0) for r in misses)  # estimate: a miss vs a typical hit
    billed = sum(r["cost_usd"] for r in rows)
    scope = "this session" if whole else f"the last 4 hours of this session ({C.fmt_dur((t1 - allrows[0]['ts']) / 1000)} long)"
    L = [f"In {scope}, cache misses cost you about {dollars(extra)} of the {dollars(billed)} billed ({len(misses)} misses in {len(rows)} messages)."
         if misses else f"In {scope}: no cache misses, {dollars(billed)} billed. Nothing to fix."]
    cell = lambda ts: min(STRIP - 1, int((ts - lo) / span * STRIP))  # noqa: E731
    strip = ["·"] * STRIP
    for r in rows:
        strip[cell(r["ts"])] = "▪" if strip[cell(r["ts"])] == "·" else "▮"
    for mark, group in (("✖", misses), ("▼", compactions), ("↻", [r for r in rows if r.get("keepalive")])):
        for r in group:
            strip[cell(r["ts"])] = mark
    stamp = lambda ms: time.strftime("%H:%M", time.localtime(ms / 1000))  # noqa: E731
    L += ["", "".join(strip), f"{stamp(lo)}{' ' * (STRIP - 10)}{stamp(t1)}",
          "▪ message  ▮ busy  ✖ cache miss (cost extra)  ▼ conversation compacted  ↻ keep-alive ping  · idle"]
    longest = max([g[0] for g in gaps] + [0])
    L += ["", f"Longest pause: {C.fmt_dur(longest / 1000)}." + (" Next time: run /compact before a break longer than 5 minutes." if misses and longest > 300_000 else "")]
    if o["details"]:
        L += ["", "Longest pauses"]
        L += C.table([[stamp(b["ts"]), C.fmt_dur(g / 1000), "cache expired" if is_rewrite(b) else "cache survived"]
                      for g, a, b in sorted(gaps, key=lambda x: -x[0])[:6] if g > 300_000] or [["none over 5 minutes", "", ""]], "llr")
    return L


# ---------------------------------------------------------------------------------------------
# /context-guru:doctor

def cmd_doctor(api: Api, o: dict) -> list[str]:
    checks: list[tuple[str, str, str]] = []  # (ok|warn|bad, what, fix)

    def add(ok: str, what: str, fix: str = "") -> None:
        checks.append((ok, what, fix))

    base = os.environ.get("ANTHROPIC_BASE_URL", "")
    add("ok" if proxy_base(None) else "bad", "This session is routed through a local proxy",
        "Run /context-guru:install")
    up = api.get("/healthz") is not None or api.get("/api/stats", {"lean": 1, "since": int(time.time() * 1000) - 60000}) is not None
    add("ok" if up else "bad", "The proxy answers", "Start it again: /context-guru:update")
    cfg = (api.get("/api/config") or {}).get("config", {})
    add("ok" if cfg else "warn", "Dashboard API is on (these commands need it)", "Start the proxy with --dashboard")
    sp = api.get("/api/spend/explain", {"since": int(time.time() * 1000) - DAY_MS, "now": int(time.time() * 1000)}, 60) or {}
    priced = sp.get("priced_requests", 0) > 0 or sp.get("requests", 0) == 0
    add("ok" if priced else "warn", "Prices are loaded (dollars are real, not zero)", "Set MODEL_PRICES to the gateway price file")
    sl = {}
    try:
        with open(os.path.join(os.environ.get("CLAUDE_CONFIG_DIR", os.path.expanduser("~/.claude")), "settings.json"), encoding="utf-8") as f:
            sl = json.load(f).get("statusLine") or {}
    except (OSError, ValueError):
        pass
    add("ok" if "statusline" in str(sl.get("command", "")) else "warn", "Context Guru status line is installed", "Run /context-guru:statusline")
    add("ok" if sl.get("refreshInterval") else "warn", "Status line refreshes on a timer (live cache countdown)",
        'Add "refreshInterval": 5 to statusLine in settings.json')
    cache_mode = cfg.get("cache_mode")
    add("ok" if cache_mode else "warn", "Cache mode is set", "See /context-guru:cache-strategy-picker")
    bad = [c for c in checks if c[0] == "bad"]
    warn = [c for c in checks if c[0] == "warn"]
    first = (bad or warn or [None])[0]
    L = [f"All {len(checks)} checks pass." if not first else
         f"{len(checks) - len(bad) - len(warn)} of {len(checks)} checks pass. Fix first: {first[2]}"]
    L += [""] + [f"  {({'ok': '✓', 'warn': '!', 'bad': '✗'})[ok]} {what}" + (f"\n      → {fix}" if ok != "ok" else "") for ok, what, fix in checks]
    return L


# ---------------------------------------------------------------------------------------------
# /context-guru:adapt — the proxy replays your own history under each setting and returns one report
# per kind (contract: docs/plugin/adapt.md). This side only renders it and, after a yes, sends back
# an option id the report offered. It never composes configuration itself.

KINDS = ("cache", "compaction", "tools")
ADAPT_DAYS = 30
ADAPT_TIMEOUT = 300
APPLY_WAIT = 110
ADAPT_ROWS = 4  # table rows without `details`: current, default, best, then the next best
EFFECT = {"now": "takes effect now", "restart": "takes effect at the next proxy restart",
          "next_session": "takes effect from the next session"}


def adapt_error(api: Api, kind: str, status: int, doc: dict | None, applying: bool = False) -> str:
    err = (doc or {}).get("error") if isinstance(doc, dict) else None
    if status == 0:
        return f"The proxy at {api.base} is not answering. Open a new session (it starts the proxy) or run /context-guru:update."
    if status == -1:
        return f"The analysis took longer than {ADAPT_TIMEOUT // 60} minutes. Try a shorter window: /context-guru:adapt {kind} --days 7"
    if status == 404 and not err:
        return ("This proxy cannot make this change from the terminal. Update it (/context-guru:update), or change it in the dashboard."
                if applying else "This proxy is older than /context-guru:adapt. Update it: /context-guru:update")
    if status in (401, 403):
        return "This proxy needs a dashboard sign-in for this. Open the dashboard, sign in, and change the setting there."
    return err or f"The proxy answered HTTP {status} with no explanation."


def with_ticker(fn, label: str):
    """Run fn; on a terminal, count seconds on stderr meanwhile (the read can take a minute)."""
    if not sys.stderr.isatty():
        return fn()
    done = threading.Event()

    def tick() -> None:
        t0 = time.time()
        while not done.wait(1):
            sys.stderr.write(f"\r{label} {int(time.time() - t0)}s")
            sys.stderr.flush()
    threading.Thread(target=tick, daemon=True).start()
    try:
        return fn()
    finally:
        done.set()
        sys.stderr.write("\r\x1b[K")


def adapt_report(api: Api, o: dict, kind: str, tick: bool = True) -> tuple[int, dict | None]:
    since, now = window(o, o["days"] or ADAPT_DAYS)
    get = lambda: api.send("GET", f"/api/adapt/{kind}", {"since": since, "until": now}, timeout=ADAPT_TIMEOUT)  # noqa: E731
    return with_ticker(get, f"Replaying {o['days'] or ADAPT_DAYS} days of {kind} history...") if tick else get()


def day(ms: int, o: dict) -> str:
    return dt.datetime.fromtimestamp((ms + o["tz_min"] * 60_000) / 1000, dt.timezone.utc).strftime("%Y-%m-%d")


def apply_token(rep: dict) -> str:
    return f"{rep['recommendation']['option']}@{rep.get('basis_id', '')}".rstrip("@")


def render_adapt(rep: dict, o: dict) -> list[str]:
    kind, opts, rec = rep["kind"], rep.get("options") or [], rep.get("recommendation")
    L = [f"{kind.capitalize()}: {rep.get('summary', '')}"]
    if opts:
        top = max([x.get("net_usd") or 0 for x in opts] + [0.01])
        rows = [["", "", "saved", "spent", "net", "", ""]]
        # Not evaluated (too little history, say): missing is not zero, so never a $0 row. Listed by name
        # under details; otherwise counted in one line.
        skipped = [x for x in opts if x.get("net_usd") is None]
        tagged = {rep.get(t) for t in ("current", "default", "best")}
        rest = sorted((x for x in opts if x["id"] not in tagged and x.get("net_usd") is not None), key=lambda x: -x["net_usd"])
        hidden = [] if o["details"] else rest[max(0, ADAPT_ROWS - len(tagged)):]
        for x in opts:
            if x in hidden:
                continue
            tags = [t for t in ("current", "default", "best") if rep.get(t) == x["id"]]
            mark = "★" if "best" in tags else "▸" if "current" in tags else " "
            if x.get("net_usd") is None:
                if o["details"] or tags:
                    rows.append([mark, x.get("label") or x["id"], "", "", "n/a", "", ", ".join(tags)])
                continue
            rows.append([mark, x.get("label") or x["id"], dollars(x.get("saved_usd", 0)), dollars(x.get("spend_usd", 0)),
                         dollars(x["net_usd"]), cells(max(x["net_usd"], 0) / top), ", ".join(tags)])
        L += [""] + C.table(rows, "llrrrll")
        if hidden:
            L.append(f"  {len(hidden)} more option{'s' * (len(hidden) > 1)}, net {dollars(min(x['net_usd'] for x in hidden))} to "
                     f"{dollars(max(x['net_usd'] for x in hidden))} (details)")
        if skipped and not o["details"]:
            L.append(f"  {len(skipped)} more not evaluated: {skipped[0].get('unavailable') or 'not enough data'}" + (" (details)" if len(skipped) > 1 else ""))
        L += ["", "  saved = what the option avoided paying; spent = what it paid for itself; net = saved - spent."]
    w = rep.get("window") or {}
    if w.get("since"):
        L.append(f"  {rep.get('class', '')} · {rep.get('method', '')} · {day(w['since'], o)}..{day(w['until'], o)}"
                 f" · {w.get('sessions', 0):,} sessions, {w.get('requests', 0):,} requests")
    L += [f"  ! {x}" for x in rep.get("warnings") or []]
    items = rep.get("items") or []
    if items:
        shown = items if o["details"] else items[:5]
        L += ["", f"Unused in this window ({len(shown)} of {len(items)}):" if len(shown) < len(items) else "Unused in this window:"]
        L += C.table([[i.get("kind", "").replace("_", " "), i["name"], dollars(i.get("usd", 0)),
                       "never used" if not i.get("last_used_ms") else "last used " + day(i["last_used_ms"], o)] for i in shown], "llrl")
    if o["details"]:
        L += [""] + [f"  {x.get('label') or x['id']}: {x.get('unavailable') or x['detail']}" + (f" (helped {x['sessions_helped']}, hurt {x['sessions_hurt']} sessions)"
                     if "sessions_helped" in x else "") for x in opts if x.get("detail") or x.get("unavailable")]
    if rec:
        eff = EFFECT.get(rec.get("effective"))
        L += ["", f"Change: {rec['change']}" + (f" ({eff})." if eff else "")]
        if rec.get("reason"):
            L.append(f"Why: {rec['reason']}")
        days = f" --days {o['days']}" if o["days"] else ""
        # Quoted when an id carries shell syntax: the line is meant to be run as it is.
        L.append(f"Apply it? Say yes, or run: /context-guru:adapt {kind} apply {shlex.quote(apply_token(rep))}{days}")
    return L


def adapt_apply(api: Api, o: dict, kind: str, token: str) -> tuple[list[str], int]:
    opt, _, basis = token.partition("@")
    if not basis:  # an option named by hand (an undo, another row of the table): act on the current report
        st, rep = adapt_report(api, o, kind)
        if st != 200 or not isinstance(rep, dict):
            return ["Not applied: " + adapt_error(api, kind, st, rep)], 1
        basis = rep.get("basis_id", "")
    since, now = window(o, o["days"] or ADAPT_DAYS)  # the report's window, so the proxy can check the basis on it
    st, doc = with_ticker(lambda: api.send("POST", f"/api/adapt/{kind}/apply", {"since": since, "until": now},
                                           {"option": opt, "basis": basis}, timeout=APPLY_WAIT + 40),
                          "Applying...")
    if st == 202 and isinstance(doc, dict) and doc.get("poll"):  # a long apply (a campaign build): wait for it
        poll, deadline = doc["poll"], time.time() + APPLY_WAIT
        while st in (200, 202) and isinstance(doc, dict) and doc.get("state") not in ("done", "failed"):
            if time.time() > deadline:
                return [f"Still applying after {APPLY_WAIT} s; the proxy carries on. Check again: /context-guru:adapt {kind}"], 0
            time.sleep(2)
            st, doc = api.send("GET", poll, timeout=30)
        if st == 200 and isinstance(doc, dict) and doc.get("state") == "failed":
            return ["Not applied: " + (doc.get("message") or "the proxy reported a failure.")], 1
    if st == 202 and isinstance(doc, dict):  # still running in the proxy, nothing to poll
        return [doc.get("note") or "Still applying; the proxy carries on."], 0
    if st == 200 and isinstance(doc, dict) and doc.get("ok") is False:  # the proxy decided not to (its own gate)
        return ["Not applied: " + (doc.get("note") or "the proxy declined the change.")], 1
    if st == 200 and isinstance(doc, dict) and doc.get("ok"):
        eff = EFFECT.get(doc.get("effective"))
        L = [f"Applied: {(doc.get('note') or doc.get('applied', opt)).rstrip('.')}" + (f" ({eff})." if eff else ".")]
        undo = (doc.get("undo") or {}).get("option")
        if undo:
            L.append(f"Undo: /context-guru:adapt {kind} apply {undo}")
        return L, 0
    if st == 409:
        L = ["Not applied: the numbers changed since you saw them. Here they are again; nothing was written.", ""]
        st2, rep = adapt_report(api, o, kind)
        return L + (render_adapt(rep, o) if st2 == 200 and rep else [adapt_error(api, kind, st2, rep)]), 1
    return ["Not applied: " + adapt_error(api, kind, st, doc, applying=True)], 1


def adapt_overview(api: Api, o: dict) -> list[str]:
    rows, best, read = [], 0.0, 0
    # In parallel, so the overview is bounded by the slowest report rather than the sum of three.
    with concurrent.futures.ThreadPoolExecutor(len(KINDS)) as ex:
        got = with_ticker(lambda: list(ex.map(lambda k: adapt_report(api, o, k, tick=False), KINDS)),
                          f"Replaying {o['days'] or ADAPT_DAYS} days of history...")
    for kind, (st, rep) in zip(KINDS, got):
        if st != 200 or not rep:
            rows.append([kind, "", adapt_error(api, kind, st, rep)])
            continue
        read += 1
        rec = rep.get("recommendation")
        if rec:
            best = max(best, rec.get("net_delta_usd", 0.0))
        rows.append([kind, dollars(rec.get("net_delta_usd", 0.0)) if rec else "", rec["change"] if rec else "already on the best option"])
    if read == 0 and len({r[2] for r in rows}) == 1:
        return [rows[0][2]]
    d = o["days"] or ADAPT_DAYS
    L = [f"The best single change would have saved {dollars(best)} over the last {d} days." if best > 0
         else f"Nothing to change: your settings were already the cheapest on the last {d} days." if read == len(KINDS)
         else "Could not read every report; see each row."]
    L += ["", "  (each row is a separate change measured on its own; do not add them)"] + C.table(rows, "lrl")
    L += ["", "See one, and apply it: /context-guru:adapt cache | compaction | tools"]
    return L


def cmd_adapt(api: Api, o: dict) -> tuple[list[str], int, dict | None]:
    """(lines, exit code, the report when one was rendered and has a recommendation)."""
    args = o["args"]
    if not args:
        return adapt_overview(api, o), 0, None
    kind = args[0]
    if kind not in KINDS:
        return [f"Unknown subcommand {kind!r}. Use one of: {', '.join(KINDS)}."], 1, None
    if args[1:2] == ["apply"]:
        if len(args) < 3:
            return [f"Say which option to apply: /context-guru:adapt {kind} apply <option>"], 1, None
        L, rc = adapt_apply(api, o, kind, args[2])
        return L, rc, None
    st, rep = adapt_report(api, o, kind)
    if st != 200 or not isinstance(rep, dict):
        return [adapt_error(api, kind, st, rep)], 1, None
    if o.get("json"):
        return [json.dumps(rep, indent=2)], 0, None
    return render_adapt(rep, o), 0, rep if rep.get("recommendation") else None


def adapt_main(api: Api, opts: dict) -> int:
    o = {"now": int(time.time() * 1000), "tz_min": 0, "days": 0, "details": False, "session": None, "args": [], **opts}
    L, rc, rep = cmd_adapt(api, o)
    print("\n".join(L))
    if rep and sys.stdin.isatty() and sys.stdout.isatty():
        if input("\nApply this change? [y/N] ").strip().lower() in ("y", "yes"):
            L, rc = adapt_apply(api, o, rep["kind"], apply_token(rep))
            print("\n".join(L))
        else:
            print("Nothing changed.")
    return rc


CMDS = {"home": cmd_home, "savings": cmd_savings, "cache": cmd_cache, "waste": cmd_waste, "session": cmd_session, "doctor": cmd_doctor}


def run(cmd: str, api: Api, opts: dict) -> str:
    o = {"now": int(time.time() * 1000), "tz_min": 0, "days": 0, "details": False, "session": None, "args": [], **opts}
    L = CMDS[cmd](api, o)
    if not o["details"] and cmd not in ("doctor", "home"):
        L += ["", f"More: /context-guru:{cmd} details"]
    return "\n".join(L)


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("cmd", choices=sorted([*CMDS, "adapt"]))
    ap.add_argument("args", nargs="*", help="`details` for the tables behind a screen; for adapt: <kind> [apply <option>]")
    ap.add_argument("--days", type=int, default=0)
    ap.add_argument("--port")
    ap.add_argument("--session")
    ap.add_argument("--json", action="store_true", help="print the raw documents of one command")
    ap.add_argument("--fixture", help="read a captured bundle of API responses from this file (tests, demos)")
    ap.add_argument("--now", help="pretend it is this ISO time")
    ap.add_argument("--tz-offset-min", type=int)
    a = ap.parse_args(argv)
    opts: dict = {"days": a.days, "session": a.session, "json": a.json,
                  "details": "details" in a.args, "args": [x for x in a.args if x != "details"],
                  "tz_min": a.tz_offset_min if a.tz_offset_min is not None else time.localtime().tm_gmtoff // 60}
    if a.now:
        opts["now"] = int(dt.datetime.fromisoformat(a.now.replace("Z", "+00:00")).timestamp() * 1000)
    if a.fixture:
        with open(a.fixture, encoding="utf-8") as f:
            api = Api("", json.load(f))
    else:
        base = proxy_base(a.port)
        if not base:
            print(NO_PROXY)
            return 0
        api = Api(base)
    if a.cmd == "adapt":
        return adapt_main(api, opts)
    print(run(a.cmd, api, opts))
    return 0


if __name__ == "__main__":
    sys.exit(main())
