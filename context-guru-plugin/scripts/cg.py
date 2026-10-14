#!/usr/bin/env python3
"""The /context-guru:* commands. Each answers ONE question: a bottom line, then a small picture, then (with
`details`) the tables behind it. Dollars first, gateway prices, no raw token counts unless asked.

    home      what is my situation, what should I do next
    savings   how much did Context Guru save me                  (saved and estimated kept apart)
    cache     is my cache about to cost me
    waste     what do I carry and never use
    session   what happened in this session
    doctor    is it set up right
    save-more              which config and keep-alive strategy would have cost least on my own history
    remove-mcp-and-skills  which MCP servers and skills I carry and never use, and what removing them saves

Read-only: GETs against the local proxy, except `apply` and `revert`, which the user confirms first. Output is
plain Unicode, no colour codes, so it reads the same on a light and a dark terminal and survives
being echoed by the model. Stdlib only; `run(cmd, api, opts)` takes the fetcher so tests feed it a
captured bundle.
"""

from __future__ import annotations

import argparse
import concurrent.futures
import datetime as dt
import json
import math
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
import tvlib as T  # noqa: E402  (Env: colour, ASCII fallback, width; cols, fit, clip)

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

    def __init__(self, base: str, fixture: dict | None = None, token: str | None = None) -> None:
        self.base, self.fixture, self.token = base.rstrip("/"), fixture, token

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
        headers = {"Content-Type": "application/json"} if data else {}
        if self.token:  # hosted: the account's own proxy token, accepted by /api/adapt/* only
            headers["x-context-guru-token"] = self.token
        req = urllib.request.Request(self.base + path + q, data=data, method=method, headers=headers)
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
# /context-guru:save-more and /context-guru:remove-mcp-and-skills — the proxy replays your own history under
# each setting and returns one report per kind (contract: docs/plugin/save-more.md). This side renders it and,
# after a yes, sends back an option id the report offered. It never composes configuration itself.

KINDS = ("config", "cache", "tools")
ROUTE = {"config": "compaction", "cache": "cache", "tools": "tools"}  # the backend keeps its /api/adapt/* paths
ALIAS = {"compaction": "config", "waste": "tools"}  # older names, still understood
WRITES = ("apply", "revert")  # top-level commands; no read command ever writes, so a skill may pre-approve it
READS = ("save-more", "remove-mcp-and-skills", "adapt")
SLASH = {"config": "/context-guru:save-more", "cache": "/context-guru:save-more", "tools": "/context-guru:remove-mcp-and-skills"}
NOUN = {"config": "config", "cache": "keep-alive strategy", "tools": "MCP servers and skills"}
ITEM_KINDS = {"mcp_server": "MCP server", "mcp_tool": "MCP tool", "skill": "skill"}  # never a built-in tool
ADAPT_DAYS = 30
MARKS = {"best": "★", "unrecommended": "☆", "current": "▸", "default": "◦"}
MARKS_ASCII = {"best": "*", "unrecommended": "o", "current": ">", "default": "d"}
LEGEND = {"best": "recommended", "unrecommended": "highest net, not recommended", "current": "yours now", "default": "the default"}
ADAPT_TIMEOUT = 300
APPLY_WAIT = 110
ADAPT_ROWS = 5  # table rows without `details`: current, default, best, then the next best
EFFECT = {"now": "takes effect now", "restart": "takes effect at the next proxy restart",
          "next_session": "takes effect from the next session"}


def cg_token() -> str | None:
    """$CG_TOKEN, else a context-guru token the agent already sends upstream (a hosted install)."""
    if os.environ.get("CG_TOKEN"):
        return os.environ["CG_TOKEN"]
    for var in ("ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY", "OPENAI_API_KEY"):
        if os.environ.get(var, "").startswith("cg_live_"):
            return os.environ[var]
    return None


def hosted_base() -> str | None:
    """The hosted service this session is routed through, when the agent carries a context-guru token."""
    if not cg_token():
        return None
    for var in ("ANTHROPIC_BASE_URL", "OPENAI_BASE_URL"):
        m = re.match(r"^(https?://[^/]+)", os.environ.get(var, ""))
        if m:
            return m.group(1)
    return None


def adapt_words(cmd: str, args: list[str], o: dict) -> tuple[str, list[str], str | None, str | None]:
    """(verb, kinds, token, error) from the words in any order: `apply cache X`, `apply X cache`, `revert config`,
    `config revert` ... A change typed under a read command is refused with the exact command to run, because a
    skill pre-approves the read commands and a change must always reach the harness's own permission prompt."""
    script = sys.argv[0] if sys.argv[0].endswith("cg.py") else os.path.abspath(__file__)  # as invoked: copy-paste exact
    words = [ALIAS.get(w, w) for w in [cmd] + args]
    verbs = [w for w in words if w in WRITES]
    kinds = [w for w in words if w in KINDS]
    rest = [w for w in words if w not in WRITES and w not in KINDS and w not in READS]
    verb = verbs[0] if verbs else "read"
    if cmd == "remove-mcp-and-skills" and not kinds:
        kinds = ["tools"]
    token = rest[0] if rest else None
    flags = (f" --days {o['days']}" if o["days"] else "") + (" --force" if o.get("force") else "")
    usage = (f'Usage: python3 "{script}" save-more [config|cache] [details] [--custom] [--days N]\n'
             f'       python3 "{script}" remove-mcp-and-skills [details] [--days N]\n'
             f'       python3 "{script}" apply <{"|".join(KINDS)}> <option>@<basis>\n'
             f'       python3 "{script}" revert <{"|".join(KINDS)}>')
    if len(verbs) > 1 or len(kinds) > 1 or (verb == "apply" and (not kinds or len(rest) != 1)) \
            or (verb == "revert" and (not kinds or rest)) or (verb == "read" and rest) \
            or (cmd == "remove-mcp-and-skills" and kinds != ["tools"]):
        return verb, kinds, token, usage
    if verb != "read" and cmd != verb:
        exact = f'python3 "{script}" {verb} {kinds[0]}' + (f" {shlex.quote(token)}" if token else "") + flags
        return verb, kinds, token, f"Run this instead (a change is its own command): {exact}"
    return verb, kinds or ["config", "cache"], token, None


def money(x: float | None) -> str:
    """dollars, or n/a for a figure the report does not have: missing is not zero."""
    return "n/a" if x is None else dollars(x)


def signed(x: float | None) -> str:
    return "n/a" if x is None else ("+" if x >= 0.005 else "") + dollars(x)


def ci(r) -> str:
    """A 95% confidence interval, named as one, or nothing when the report has none."""
    return f"(95% CI {money(r[0])} to {money(r[1])})" if isinstance(r, (list, tuple)) and len(r) == 2 else ""


def adapt_error(api: Api, kind: str, status: int, doc: dict | None, applying: bool = False) -> str:
    err = (doc or {}).get("error") if isinstance(doc, dict) else None
    if status == 0:
        return f"The proxy at {api.base} is not answering. Open a new session (it starts the proxy) or run /context-guru:update."
    if status == -1:
        return f"The analysis took longer than {ADAPT_TIMEOUT // 60} minutes. Try a shorter window: --days 7"
    if status == 404 and not err:
        return ("This proxy cannot make this change from the terminal. Update it (/context-guru:update), or change it in the dashboard."
                if applying else "This proxy is older than this command. Update it: /context-guru:update")
    if status in (401, 403):
        return ("This proxy refused your credentials for this. Set CG_TOKEN to your context-guru token, or change it in the dashboard."
                if api.token else "This proxy needs a sign-in for this. Set CG_TOKEN to your context-guru token, or change it in the dashboard.")
    return err or f"The proxy answered HTTP {status} with no explanation."


def with_ticker(fn, label: str):
    """Run fn; on a terminal, count seconds on stderr meanwhile (the replay can take minutes)."""
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
    q = {"since": since, "until": now, **({"custom": 1} if o.get("custom") and kind == "config" else {})}
    get = lambda: api.send("GET", f"/api/adapt/{ROUTE[kind]}", q, timeout=ADAPT_TIMEOUT)  # noqa: E731
    return with_ticker(get, f"Replaying {o['days'] or ADAPT_DAYS} days of history...") if tick else get()


def day(ms: int, o: dict) -> str:
    return dt.datetime.fromtimestamp((ms + o["tz_min"] * 60_000) / 1000, dt.timezone.utc).strftime("%Y-%m-%d")


def days_of(w: dict) -> int:
    return max(1, round(((w.get("until") or 0) - (w.get("since") or 0)) / DAY_MS)) if w.get("since") else ADAPT_DAYS


def apply_token(rep: dict) -> str:
    return f"{rep['recommendation']['option']}@{rep.get('basis_id', '')}".rstrip("@")


def apply_line(kind: str, token: str, o: dict, force: bool = False) -> str:
    """The exact command for a change, quoted when an id carries shell syntax: the line is meant to be run as it is."""
    days = f" --days {o['days']}" if o["days"] else ""
    return f"{SLASH[kind]} apply {kind} {shlex.quote(token)}{days}" + (" --force" if force else "")


def week_range(rec: dict, w: dict) -> str:
    """The weekly figure's range, named for what it is: a 95% CI is a "95% CI", a bound is a "range". Never one
    for the other. A window figure is scaled by the same factor that turned the change into its weekly figure."""
    pw = per_week(rec, w)
    for key, name, total in (("ci95", "95% CI", rec.get("net_delta_usd")), ("range_per_week_usd", "range", None),
                             ("gain_range_usd", "range", rec.get("gain_usd", rec.get("net_delta_usd")))):
        r = rec.get(key)
        if not (isinstance(r, (list, tuple)) and len(r) == 2 and any(r)):
            continue
        k = 1.0 if total is None else (pw / total if pw is not None and total else None)
        if k is None:
            continue
        return f" ({name} {money(r[0] * k)} to {money(r[1] * k)})"
    return ""


def per_week(rec: dict, w: dict) -> float | None:
    if rec.get("per_week_usd") is not None:
        return rec["per_week_usd"]
    return None if rec.get("net_delta_usd") is None else rec["net_delta_usd"] / days_of(w) * 7


# --- drawing ----------------------------------------------------------------------------------

def card(title: str, lines: list[str], e) -> list[str]:
    """A boxed summary, as wide as the terminal allows; ASCII corners without Unicode."""
    w = min(e.width - 1, max([T.vlen(x) for x in lines] + [len(title)]) + 4)
    h, v, tl, tr, bl, br = ("-", "|", "+", "+", "+", "+") if e.ascii else ("─", "│", "╭", "╮", "╰", "╯")
    out = [f"{tl}{h} {e.paint('bold', title)} " + h * max(0, w - len(title) - 5) + tr]
    for x in lines:
        for y in T.fit([x], T.Env(color=False, ascii_only=e.ascii, width=w - 3)):
            pad = " " * max(0, w - 4 - T.vlen(y))
            out.append(f"{v} {y}{pad} {v}")
    return out + [bl + h * (w - 2) + br]


def dot(e) -> str:
    return "|" if e.ascii else "·"


def approx(e) -> str:
    return "~" if e.ascii else "≈"


def bar(frac: float, n: int, e) -> str:
    k = max(0, min(n, round(frac * n)))
    return e.g["full"] * k + e.g["empty"] * (n - k)


def heatmap(hm: dict, e) -> list[str]:
    """Day-of-week x hour: darker is more saved. The density glyph carries the level, colour only repeats it."""
    vals = hm.get("values") or []
    if len(vals) != 7 or any(len(r) != 24 for r in vals):
        return []
    top = max([v for r in vals for v in r if v is not None] + [0.0])
    ramp = e.g["ramp"]
    names = ["Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"]
    label, tz = hm.get("label") or "Saved by hour", hm.get("tz") or "UTC"
    out = [f"  {label}" + ("" if tz in label else f" ({tz})"),
           "        0     6     12    18   23"]
    for name, r in zip(names, vals):
        cells_ = ""
        for v in r:
            lv = 0 if not v or v <= 0 or top <= 0 else min(4, 1 + int(v / top * 3.999))
            cells_ += e.heat(lv, ramp[lv]) if e.color else ramp[lv]
        out.append(f"  {name}   {cells_}")
    out.append(f"        {ramp[1]} little   {ramp[4]} most (up to {dollars(top)} in one hour slot)")
    return out


def table(rep: dict, o: dict, e) -> list[str]:
    opts = [x for x in rep.get("options") or []]
    rec = rep.get("recommendation")
    if not opts:
        return []
    top = max([abs(x.get("net_usd") or 0) for x in opts] + [0.01])
    tagged = [rep.get(t) for t in ("current", "default", "best")]
    shown = [x for x in opts if x.get("net_usd") is not None]
    if not o["details"]:  # current, default, best, then the next best by net
        rest = sorted((x for x in shown if x["id"] not in tagged), key=lambda x: -x["net_usd"])
        keep = {x["id"] for x in rest[:max(0, ADAPT_ROWS - len(set(tagged) - {None}))]} | set(tagged)
        shown = [x for x in shown if x["id"] in keep]
    shown.sort(key=lambda x: -x["net_usd"])  # ranked
    M = MARKS_ASCII if e.ascii else MARKS
    rows = [["", "option", "", "saved", "spent", "net", ""]]
    used = set()
    for x in shown:
        tags = {t for t in ("current", "default", "best") if rep.get(t) == x["id"]}
        if "best" in tags and not rec and not {"current", "default"} & tags:
            tags = tags - {"best"} | {"unrecommended"}  # the top net the proxy does not recommend: never a plain "best"
        elif "best" in tags and not rec:
            tags -= {"best"}
        used |= tags
        mark = "".join(M[t] for t in ("best", "unrecommended", "current", "default") if t in tags)
        est = approx(e) if x.get("class") == "estimate" else ""  # a figure partly estimated rather than replayed
        b = bar(max(x["net_usd"], 0) / top, 12, e)
        b = e.paint("ok", b) if "best" in tags else e.paint("info", b) if "current" in tags else b
        rows.append([mark, x["id"], x.get("label") if x.get("label") != x["id"] else "",
                     money(x.get("saved_usd")), money(x.get("spend_usd")), est + dollars(x["net_usd"]), b])
    # Width-aware: everything but the label keeps its width; the label gives way, down to 10 columns.
    other = sum(max(T.vlen(r[i]) for r in rows) for i in (0, 1, 3, 4, 5, 6)) + 2 * 6 + 2
    labelw = max(10, e.width - 1 - other)
    rows = [r[:2] + [T.clip(r[2], labelw, e)] + r[3:] for r in rows]
    out = [""] + ["  " + r for r in T.cols(rows, "lllrrrl")]
    out.append("  " + "   ".join(f"{M[t]} {LEGEND[t]}" for t in ("best", "unrecommended", "current", "default") if t in used)
               + (f"   {approx(e)} partly estimated" if any(x.get("class") == "estimate" for x in shown) else ""))
    hidden = [x for x in opts if x.get("net_usd") is not None and x not in shown]
    skipped = [x for x in opts if x.get("net_usd") is None]
    if hidden:
        out.append(f"  {len(hidden)} more option{'s' * (len(hidden) > 1)}, net {dollars(min(x['net_usd'] for x in hidden))} to "
                   f"{dollars(max(x['net_usd'] for x in hidden))} (details)")
    if skipped:
        if o["details"]:
            out += [f"  {x['id']}: not evaluated: {x.get('unavailable') or 'not enough data'}" for x in skipped]
        else:
            out.append(f"  {len(skipped)} more not evaluated: {skipped[0].get('unavailable') or 'not enough data'}"
                       + (" (details)" if len(skipped) > 1 else ""))
    out.append(f"  saved: what it avoided paying {dot(e)} spent: what it paid itself {dot(e)} net = saved - spent, on your past sessions")
    return out


def plain_words(kind: str, rep: dict, o: dict) -> list[str]:
    """(a) what the current setting saved so far, (b) what the recommended change would add."""
    w = rep.get("window") or {}
    td = rep.get("to_date") or {}
    if kind == "tools":
        n_, saved = td.get("items") or 0, td.get("saved_usd")
        since = f", since {day(td['since'], o)}" if td.get("since") else ""
        a = (f"So far: the {n_} item{'s' * (n_ != 1)} you already removed saved {dollars(saved)} ({td.get('class', 'measured')}{since})." if saved is not None and n_
             else f"So far: earlier removals saved {dollars(saved)} ({td.get('class', 'measured')}{since})." if saved
             else "So far: nothing removed yet, so nothing saved by removing yet.")
    elif td.get("saved_usd") is not None:
        a = (f"So far: your current {NOUN[kind]} saved {dollars(td['saved_usd'])} in {days_of({'since': td.get('since'), 'until': td.get('until')})} days"
             f" ({td.get('class', 'measured')}).")
    else:
        a = f"So far: what your current {NOUN[kind]} saved is not measured on this proxy yet."
    rec = rep.get("recommendation")
    if rec:
        pw = per_week(rec, w)
        b = f"Change: {rec['change']}." + (f" Over the last {days_of(w)} days it would have saved about {dollars(pw)} a week"
                                           f"{week_range(rec, w)}." if pw is not None else "")
    else:
        b = "Change: none recommended (the reason is just below)."
    by = {x["id"]: x for x in rep.get("options") or []}
    now_, then = by.get(rep.get("current"), {}).get("billed_usd"), by.get((rec or {}).get("option"), {}).get("billed_usd")
    if now_ is not None and then is not None:  # before -> after, on the same past sessions
        return [a, b, f"Before -> after: {dollars(now_)} billed on these sessions, {dollars(then)} with the change."]
    return [a, b]


def render_kind(kind: str, rep: dict, o: dict, e) -> list[str]:
    title = {"config": "Save more: config", "cache": "Save more: cache", "tools": "Remove MCP servers and skills"}[kind]
    L = card(title, plain_words(kind, rep, o), e)
    L.append(rep.get("summary", ""))
    rel = rep.get("relative") or {}
    pairs = [(k, rel[k]) for k in ("best_vs_default", "current_vs_default", "best_vs_current")
             if isinstance(rel.get(k), dict) and (rel[k].get("usd") or any(rel[k].get("ci95") or []))]
    if pairs:  # the paired differences: what the decision rests on
        L.append("  " + f"  {dot(e)}  ".join(f"{k.replace('_vs_', ' vs ')} {signed(v.get('usd'))} {ci(v.get('ci95'))}".rstrip() for k, v in pairs))
    if rep.get("disagreement"):
        L.append(f"  ! {rep['disagreement']}")
    L += table(rep, o, e)
    if kind == "cache" and isinstance(rep.get("heatmap"), dict):
        L += [""] + heatmap(rep["heatmap"], e)
    items = [i for i in rep.get("items") or [] if i.get("kind") in ITEM_KINDS]  # a built-in tool is never offered
    if items:
        shown = items if o["details"] else items[:8]
        top = max([i.get("usd") or 0 for i in shown] + [0.01])
        L += ["", f"Never used in this window ({len(shown)} of {len(items)}):" if len(shown) < len(items) else "Never used in this window:"]
        L += ["  " + r for r in T.cols([[ITEM_KINDS[i["kind"]], T.clip(i["name"], 40, e),
                                         money(i.get("usd") if i.get("priced", True) else None),
                                         bar((i.get("usd") or 0) / top, 10, e)] for i in shown], "llrl")]
    removed = rep.get("removed") or []
    if removed and o["details"]:
        # Measured (the proxy's filter) and estimated (removed in the client) stay apart and are never added
        # together. The "unattributed" row is the measured gap not matched to one item: kept, so the rows add up.
        row = lambda i: [ITEM_KINDS.get(i.get("kind"), "not matched" if i.get("kind") == "unattributed" else i.get("kind", "")),  # noqa: E731
                         i["name"], day(i["since_ms"], o) if i.get("since_ms") else "", money(i.get("saved_usd"))]
        meas = [i for i in removed if i.get("class", "observed") != "estimated"]
        est = [i for i in removed if i.get("class") == "estimated"]
        if meas:
            L += ["", "Removed earlier, measured since:"] + ["  " + r for r in T.cols([row(i) for i in meas], "lllr")]
        if est:
            L += ["", "Estimated (removed in the client, not measured by the proxy):"] + ["  " + r for r in T.cols([row(i) for i in est], "lllr")]
    if o["details"]:
        held = [x for x in rep.get("options") or [] if isinstance(x.get("held_out"), dict) and x["held_out"].get("vs_default_usd") is not None]
        if held:
            L += ["", "Held out (your most recent decisions, plans tuned only on what came before), vs the default:"]
            L += ["  " + r for r in T.cols([[x["id"], signed(x["held_out"]["vs_default_usd"]), ci(x["held_out"].get("vs_default_ci95")),
                                             {True: "passes the go-live check", False: "fails the go-live check"}.get(
                                                 (x["held_out"].get("builder_gate") or {}).get("pass"), "")] for x in held], "lrll")]
        notes = []
        for x in rep.get("options") or []:
            notes += ([f"  {x['id']}: {x['detail']}"] if x.get("detail") else []) + ([f"  {x['id']} plan: {x['plan']}"] if x.get("plan") else [])
        L += [""] + notes if notes else []
    w = rep.get("window") or {}
    if w.get("since"):
        L += ["", f"  {rep.get('class', '')} {dot(e)} {rep.get('method', '')} {dot(e)} {day(w['since'], o)}..{day(w['until'], o)}"
                  f" {dot(e)} {w.get('sessions', 0):,} sessions, {w.get('requests', 0):,} requests"]
    L += [f"  ! {x}" for x in rep.get("warnings") or [] if x != rep.get("disagreement")]
    rec = rep.get("recommendation")
    act = []  # never wrapped: these lines are run exactly as printed
    if rec:
        eff = EFFECT.get(rec.get("effective"))
        L += ["", f"Recommended: {rec['change']}" + (f" ({eff})." if eff else ".")]
        if rec.get("reason"):
            L.append(f"Why: {rec['reason']}")
        act.append(f"Apply it? Say yes, or run: {apply_line(kind, apply_token(rep), o)}")
    elif rep.get("options") and rep.get("basis_id"):  # nothing recommended, but a row may still be chosen by name
        act.append(f"Any row, after a yes: {apply_line(kind, '<option>@' + rep['basis_id'], o)}")
    return T.fit(L, e) + act


def weeks(n: float) -> str:
    """A payback, rounded up: a change that takes 1.2 weeks to pay back has not paid back in 1."""
    k = max(1, math.ceil(n - 1e-9))
    return f"{k} week" + ("s" if k > 1 else "")


def tools_question(rep: dict, e) -> str | None:
    """The one question remove-mcp-and-skills asks, led by the recommendation, in hindsight: what the items
    would have saved, not a promise of what removing them will."""
    rec = rep.get("recommendation")
    if not rec:
        return None
    w = rep.get("window") or {}
    n = rec.get("items_n") or len([i for i in rep.get("items") or [] if i.get("kind") in ITEM_KINDS])
    q = f"Question: Remove these {n} items?"
    pw = per_week(rec, w)
    if pw is not None:
        q += f" Over the last {days_of(w)} days they would have saved about {dollars(pw)} a week"
        pb = rec.get("payback_weeks", rep.get("payback_weeks"))
        q += (f" (pays back in about {weeks(pb)})." if isinstance(pb, (int, float)) and pb > 1 else ".")
    return q


# --- changes ----------------------------------------------------------------------------------

def adapt_apply(api: Api, o: dict, kind: str, token: str) -> tuple[list[str], int]:
    opt, _, basis = token.partition("@")
    if not basis and opt != "revert":  # an option named by hand (another row of the table): act on the current report
        st, rep = adapt_report(api, o, kind)
        if st != 200 or not isinstance(rep, dict):
            return ["Not applied: " + adapt_error(api, kind, st, rep)], 1
        basis = rep.get("basis_id", "")
    since, now = window(o, o["days"] or ADAPT_DAYS)  # the report's window, so the proxy can check the basis on it
    body = {"option": opt, **({"basis": basis} if basis else {}), **({"force": True} if o.get("force") else {})}
    st, doc = with_ticker(lambda: api.send("POST", f"/api/adapt/{ROUTE[kind]}/apply", {"since": since, "until": now},
                                           body, timeout=APPLY_WAIT + 40),
                          "Applying...")
    if st == 202 and isinstance(doc, dict) and doc.get("poll"):  # a long apply (a campaign build): wait for it
        poll, deadline = doc["poll"], time.time() + APPLY_WAIT
        while st in (200, 202) and isinstance(doc, dict) and doc.get("state") not in ("done", "failed"):
            if time.time() > deadline:
                return [f"Still applying after {APPLY_WAIT} s; the proxy carries on. Check again: {SLASH[kind]}"], 0
            time.sleep(2)
            st, doc = api.send("GET", poll, timeout=30)
        if st == 200 and isinstance(doc, dict) and doc.get("state") == "failed":
            return ["Not applied: " + (doc.get("message") or "the proxy reported a failure.")], 1
    if st == 202 and isinstance(doc, dict):  # still running in the proxy, nothing to poll
        return [doc.get("note") or "Still applying; the proxy carries on."], 0
    if st == 200 and isinstance(doc, dict) and doc.get("ok") is False:  # the proxy decided not to (its own gate)
        L = ["Not applied: " + (doc.get("note") or "the proxy declined the change.")]
        if doc.get("force_available") and not o.get("force"):  # a second, explicit decision; never sent implicitly
            L.append(f"Apply anyway, although that check failed: {apply_line(kind, token, o, force=True)}")
        return L, 1
    if st == 200 and isinstance(doc, dict) and doc.get("ok"):
        eff = EFFECT.get(doc.get("effective"))
        L = [f"Applied: {(doc.get('note') or doc.get('applied', opt)).rstrip('.')}" + (f" ({eff})." if eff else ".")]
        undo = doc.get("undo") or {}
        if undo.get("option") == "revert":
            L.append(f"Undo: {SLASH[kind]} revert {kind}")
        elif undo.get("option"):
            L.append(f"Undo: {apply_line(kind, undo['option'] + (('@' + undo['basis']) if undo.get('basis') else ''), o)}")
        return L, 0
    if st == 409 and opt != "revert":  # a revert has no report to go stale: its 409 is the proxy's own sentence
        L = ["Not applied: the numbers changed since you saw them. Here they are again; nothing was written.", ""]
        st2, rep = adapt_report(api, o, kind)
        return L + (render_kind(kind, rep, o, T.Env()) if st2 == 200 and rep else [adapt_error(api, kind, st2, rep)]), 1
    return ["Not applied: " + adapt_error(api, kind, st, doc, applying=True)], 1


# --- the read commands ------------------------------------------------------------------------

def read_kinds(api: Api, o: dict, kinds: list[str]) -> list[tuple[str, int, dict | None]]:
    """In parallel, so `save-more` is bounded by the slower report rather than the sum."""
    with concurrent.futures.ThreadPoolExecutor(len(kinds)) as ex:
        got = with_ticker(lambda: list(ex.map(lambda k: adapt_report(api, o, k, tick=False), kinds)),
                          f"Replaying {o['days'] or ADAPT_DAYS} days of history...")
    return [(k, st, rep) for k, (st, rep) in zip(kinds, got)]


def cmd_save(api: Api, o: dict, kinds: list[str], e) -> tuple[list[str], int, list[tuple[str, dict]]]:
    """(lines, exit code, (kind, report) for each report that carries a recommendation)."""
    got = read_kinds(api, o, kinds)
    if o.get("json"):
        return [json.dumps({k: rep for k, _, rep in got}, indent=2)], 0, []
    L, recs, rc = [], [], 0
    for k, st, rep in got:
        if st != 200 or not isinstance(rep, dict):
            L += [f"{k}: {adapt_error(api, k, st, rep)}", ""]
            rc = 1
            continue
        L += render_kind(k, rep, o, e) + [""]
        if rep.get("recommendation"):
            recs.append((k, rep))
    if len(kinds) > 1:  # the combined summary, then one question
        ok = [(k, rep) for k, st, rep in got if st == 200 and isinstance(rep, dict)]
        if ok:
            lines = []
            for k, rep in ok:
                rec, td = rep.get("recommendation"), rep.get("to_date") or {}
                lines.append(f"{k:<7} saved so far {money(td.get('saved_usd')):>8}   "
                             + ((f"the recommended change would have saved about {dollars(pw)} a week" if (pw := per_week(rec, rep.get("window") or {})) is not None
                                 else "a change is recommended") if rec else "no change recommended"))
            L += card("Save more: both", lines, e)
        if len(recs) > 1:
            L.append("Question: apply both, only one of them, or neither?")
        rc = 0 if ok else 1
    elif kinds == ["tools"] and recs:
        L.append(tools_question(recs[0][1], e))
    return L, rc, recs


def adapt_main(api: Api, cmd: str, opts: dict) -> int:
    o = {"now": int(time.time() * 1000), "tz_min": 0, "days": 0, "details": False, "session": None, "args": [], **opts}
    verb, kinds, token, err = adapt_words(cmd, o["args"], o)
    if err:
        print(err)
        return 1
    if verb != "read":
        L, rc = adapt_apply(api, o, kinds[0], token if verb == "apply" else "revert")
        print("\n".join(L))
        return rc
    e = T.Env()
    pre = []
    if cmd == "adapt":  # the 0.6 name, for one release
        pre = ["/context-guru:adapt is now /context-guru:save-more (config, cache) and /context-guru:remove-mcp-and-skills.", ""]
    L, rc, recs = cmd_save(api, o, kinds, e)
    print("\n".join(pre + L))
    if recs and sys.stdin.isatty() and sys.stdout.isatty():
        for k, rep in recs:
            if input(f"\nApply {rep['recommendation']['change']}? [y/N] ").strip().lower() in ("y", "yes"):
                L, rc = adapt_apply(api, o, k, apply_token(rep))
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
    ap.add_argument("cmd", choices=sorted([*CMDS, *READS, *WRITES]))
    ap.add_argument("args", nargs="*", help="`details` for the tables behind a screen; save-more [config|cache]; apply <kind> <option>@<basis>; revert <kind>")
    ap.add_argument("--custom", action="store_true", help="save-more config: also search custom combinations, not only the shipped presets")
    ap.add_argument("--force", action="store_true", help="apply: go ahead although the proxy's own check failed (asked separately)")
    ap.add_argument("--days", type=int, default=0)
    ap.add_argument("--port")
    ap.add_argument("--session")
    ap.add_argument("--json", action="store_true", help="print the raw documents of one command")
    ap.add_argument("--fixture", help="read a captured bundle of API responses from this file (tests, demos)")
    ap.add_argument("--now", help="pretend it is this ISO time")
    ap.add_argument("--tz-offset-min", type=int)
    argv = sys.argv[1:] if argv is None else argv
    if argv and argv[0] in ("config", "compaction", "tools"):  # `cg.py config revert`: the read side, which names the change
        argv = ["save-more", *argv]
    a = ap.parse_args(argv)
    if a.cmd not in (*READS, *WRITES) and set(a.args) & set(WRITES):  # `cg.py cache revert`: say the one command that does it
        a.args, a.cmd = [a.cmd, *a.args], "save-more"
    opts: dict = {"days": a.days, "session": a.session, "json": a.json, "force": a.force, "custom": a.custom,
                  "details": "details" in a.args, "args": [x for x in a.args if x != "details"],
                  "tz_min": a.tz_offset_min if a.tz_offset_min is not None else time.localtime().tm_gmtoff // 60}
    if a.now:
        opts["now"] = int(dt.datetime.fromisoformat(a.now.replace("Z", "+00:00")).timestamp() * 1000)
    if a.fixture:
        with open(a.fixture, encoding="utf-8") as f:
            api = Api("", json.load(f))
    else:
        adapt = a.cmd in (*READS, *WRITES)
        base = proxy_base(a.port) or (hosted_base() if adapt else None)
        if not base:
            print(NO_PROXY)
            return 1 if adapt else 0  # a read that showed no report must not count as run (the skills chain on it)
        api = Api(base, token=cg_token() if adapt else None)
    if a.cmd in (*READS, *WRITES):
        return adapt_main(api, a.cmd, opts)
    print(run(a.cmd, api, opts))
    return 0


if __name__ == "__main__":
    sys.exit(main())
