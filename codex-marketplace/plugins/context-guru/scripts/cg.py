#!/usr/bin/env python3
"""The /context-guru:* commands. Each answers ONE question: a bottom line, then a small picture, then (with
`details`) the tables behind it. Dollars first, gateway prices, no raw token counts unless asked.

    home      what is my situation, what should I do next
    savings   how much did Context Guru save me                  (saved and estimated kept apart)
    cache     is my cache about to cost me
    waste     what do I carry and never use
    session   what happened in this session
    doctor    is it set up right

Read-only: GETs against the local proxy. Output is
plain Unicode, no colour codes, so it reads the same on a light and a dark terminal and survives
being echoed by the model. Stdlib only; `run(cmd, api, opts)` takes the fetcher so tests feed it a
captured bundle.
"""

from __future__ import annotations

import argparse
import datetime as dt
import json
import os
import re
import sys
import time
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
    p: dict = {"lean": 1, "since": since}
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
        L += ["", f"Cache misses in {d} days: {sum(v['n'] for v in rw.values())} costing {dollars(tot)} extra   (observed)"]
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
    L += ["", f"Observed carry; the monthly figure is an estimate that assumes your usage stays the same."]
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


CMDS = {"home": cmd_home, "savings": cmd_savings, "cache": cmd_cache, "waste": cmd_waste, "session": cmd_session, "doctor": cmd_doctor}


def run(cmd: str, api: Api, opts: dict) -> str:
    o = {"now": int(time.time() * 1000), "tz_min": 0, "days": 0, "details": False, "session": None, "args": [], **opts}
    L = CMDS[cmd](api, o)
    if not o["details"] and cmd not in ("doctor", "home"):
        L += ["", f"More: /context-guru:{cmd} details"]
    return "\n".join(L)


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("cmd", choices=sorted(CMDS))
    ap.add_argument("args", nargs="*", help="`details` for the tables behind a screen")
    ap.add_argument("--days", type=int, default=0)
    ap.add_argument("--port")
    ap.add_argument("--session")
    ap.add_argument("--json", action="store_true", help="print the raw documents of one command")
    ap.add_argument("--fixture", help="read a captured bundle of API responses from this file (tests, demos)")
    ap.add_argument("--now", help="pretend it is this ISO time")
    ap.add_argument("--tz-offset-min", type=int)
    a = ap.parse_args(argv)
    opts: dict = {"days": a.days, "session": a.session,
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
    print(run(a.cmd, api, opts))
    return 0


if __name__ == "__main__":
    sys.exit(main())
