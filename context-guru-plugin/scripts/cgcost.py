#!/usr/bin/env python3
"""Five cost questions, answered from the proxy's own billed traffic.

    today    what cost me most, and why                       (/api/spend/explain)
    unused   tools / MCP servers / skills I carry, never use  (/api/tools, /api/tools/last-used)
    cold     where my cache went cold, and what it cost       (/api/spend/explain)
    compact  am I about to pay a rewrite - compact now?       (/api/spend/explain, live sessions)
    trends   per-project / per-model trends, ranked actions   (/api/spend/explain, /api/tools)

Every figure is priced at the GATEWAY rates the proxy was configured with (not list prices) and
carries a class: `observed` (tokens the provider billed), `modelled` (a counterfactual the proxy
computed) or `estimate` (arithmetic done here, assumptions printed). Context Guru's own books are
shown as honest net: gross saving MINUS what Context Guru itself spent (summariser calls and
keep-alive pings).

This module is stdlib-only and takes the fetcher as a parameter, so the Codex plugin can vendor it
unchanged: `run(cmd, fetch, opts)` returns the text, `fetch(path, params, heavy)` returns the parsed
JSON or None. Read-only: GETs against the local proxy, nothing is written or applied.
"""

from __future__ import annotations

import argparse
import datetime as dt
import glob
import json
import os
import re
import sys
import time

DAY_MS = 86_400_000
SPARK = "▁▂▃▄▅▆▇█"
# Components of Context Guru itself. Never offered for removal, whatever the numbers say.
OWN = re.compile(r"context[-_]?guru", re.I)
# What a /compact is assumed to cost and leave behind: the summary the model writes, and the
# context that survives (system prompt, tools, summary). Printed with the verdict.
SUMMARY_OUT_TOKENS = 4_000
SURVIVING_TOKENS = 15_000
RECENT_USE_DAYS = 30
CAUSE_WORDS = {
    "cold_start": "Starting sessions (first message writes the system prompt and tool list)",
    "idle_expiry": "Pauses longer than the cache lives: the whole conversation was re-sent and re-cached",
    "new_thread": "Subagents or a changed tool set starting a fresh cache entry",
    "history_rewrite": "The conversation was rewound or compacted, so the cache could not be reused",
    "prefix_change": "The start of the prompt changed mid-session (tools, MCP, CLAUDE.md, model settings)",
}
AVOIDABLE = ("idle_expiry", "prefix_change", "history_rewrite", "new_thread")


# ---------------------------------------------------------------------------------------------
# formatting

def usd(x: float) -> str:
    x = float(x or 0)
    sign, x = ("-" if x < 0 else ""), abs(x)
    return sign + (f"${x:,.2f}" if x >= 1 else (f"${x:.3f}" if x >= 0.001 else "$0"))


def ktok(n: float) -> str:
    n = float(n or 0)
    return f"{n/1e6:.1f}M" if n >= 1e6 else (f"{n/1e3:.0f}k" if n >= 1e3 else f"{n:.0f}")


def spark(vals: list[float]) -> str:
    top = max(vals) if vals else 0
    if top <= 0:
        return SPARK[0] * len(vals)
    return "".join(SPARK[min(7, int(v / top * 7.999))] if v > 0 else SPARK[0] for v in vals)


def bar(frac: float, width: int = 10) -> str:
    n = max(0, min(width, round(frac * width)))
    return "█" * n + "░" * (width - n)


def pct(a: float, b: float) -> str:
    return f"{100*a/b:.0f}%" if b > 0 else "-"


class Style:
    """ANSI only on a terminal and never when NO_COLOR is set. Skills run without a tty, so their
    output is plain."""

    def __init__(self, color: bool) -> None:
        self.on = color

    def _w(self, code: str, s: str) -> str:
        return f"\x1b[{code}m{s}\x1b[0m" if self.on else s

    def b(self, s: str) -> str:
        return self._w("1", s)

    def dim(self, s: str) -> str:
        return self._w("2", s)

    def warn(self, s: str) -> str:
        return self._w("33", s)


def table(rows: list[list[str]], aligns: str) -> list[str]:
    """Left/right aligned columns; `aligns` is one of 'l' or 'r' per column."""
    if not rows:
        return []
    widths = [max(len(r[i]) for r in rows) for i in range(len(rows[0]))]
    out = []
    for r in rows:
        cells = [c.ljust(w) if a == "l" else c.rjust(w) for c, w, a in zip(r, widths, aligns)]
        out.append("  " + "  ".join(cells).rstrip())
    return out


def fmt_dur(sec: float) -> str:
    sec = int(sec)
    if sec < 0:
        return "0:00"
    if sec >= 3600:
        return f"{sec//3600}h{(sec%3600)//60:02d}m"
    return f"{sec//60}:{sec%60:02d}"


# ---------------------------------------------------------------------------------------------
# local project names (Claude Code keeps transcripts at ~/.claude/projects/<dir>/<uuid>.jsonl)

class Projects:
    def __init__(self, root: str | None = None) -> None:
        base = root or os.path.join(os.environ.get("CLAUDE_CONFIG_DIR") or os.path.expanduser("~/.claude"),
                                    "projects")
        self.by_uuid: dict[str, str] = {}
        for p in glob.glob(os.path.join(base, "*", "*.jsonl")):
            self.by_uuid[os.path.basename(p)[:-6]] = p
        self.cache: dict[str, str] = {}

    def name(self, session: str) -> str:
        uuid = session.split(":")[-1]
        if uuid in self.cache:
            return self.cache[uuid]
        path, name = self.by_uuid.get(uuid), "(unknown project)"
        if path:
            name = os.path.basename(os.path.dirname(path)).strip("-").split("-")[-1] or name
            try:
                with open(path, encoding="utf-8") as f:
                    for _, line in zip(range(40), f):
                        m = re.search(r'"cwd":"((?:[^"\\]|\\.)*)"', line)
                        if m:
                            name = os.path.basename(json.loads('"%s"' % m.group(1)).rstrip("/")) or name
                            break
            except (OSError, ValueError):
                pass
        self.cache[uuid] = name
        return name


# ---------------------------------------------------------------------------------------------
# shared computations

def window_days(spend: dict) -> float:
    w = spend.get("window", {})
    since, until = w.get("since", 0), w.get("until") or w.get("now", 0)
    return max(0.0, (until - since) / DAY_MS) if since and until else 0.0


def short_model(m: str) -> str:
    return m.split("/")[-1]


def merged_models(spend: dict) -> list[dict]:
    """Per-model rows with the gateway prefix dropped, so aws/claude-x and claude-x are one row."""
    out: dict[str, dict] = {}
    for m in spend["models"]:
        k = short_model(m["model"])
        r = out.setdefault(k, {"model": k, "ids": [], "usd": {"total": 0.0}})
        r["usd"]["total"] += m["usd"]["total"]
        r["ids"].append(m["model"])
    return sorted(out.values(), key=lambda r: -r["usd"]["total"])


def partition(spend: dict) -> list[dict]:
    """Disjoint pieces of the bill that sum to usd.total. Rewrite pieces carry the extra paid over
    a cache hit, which is the part that was not the normal cost of the same tokens."""
    u, rw = spend["usd"], spend.get("rewrites", {})
    rows, rewrite_writes = [], 0.0
    for cause, c in rw.items():
        rewrite_writes += c["write_usd"]
        rows.append({"key": cause, "usd": c["write_usd"], "extra": c["usd"], "n": c["n"],
                     "what": CAUSE_WORDS.get(cause, cause)})
    rows += [
        {"key": "cache_read", "usd": u["cache_read"], "what": "Re-reading the conversation each turn (cache reads, normal)"},
        {"key": "new_content", "usd": max(0.0, u["cache_write"] - rewrite_writes),
         "what": "New content entering the conversation (tool output, your messages)"},
        {"key": "output", "usd": u["output"], "what": "Model output"},
        {"key": "fresh", "usd": u["fresh"], "what": "Uncached input"},
    ]
    return sorted((r for r in rows if r["usd"] > 0), key=lambda r: -r["usd"])


def ledger_lines(spend: dict, S: Style) -> list[str]:
    L = spend.get("ledger", {})
    gross = L.get("gross_saved_usd", 0.0)
    over = L.get("cg_llm_usd", 0.0) + L.get("ping_usd", 0.0)
    return [S.dim(f"Context Guru books: gross saved {usd(gross)} (modelled) - own overhead {usd(over)} "
                  f"(summariser {usd(L.get('cg_llm_usd', 0))}, keep-alive pings {usd(L.get('ping_usd', 0))}, observed) "
                  f"= net {usd(gross - over)}")]


def coverage_line(spend: dict, S: Style) -> list[str]:
    un = spend.get("unpriced_requests", 0)
    return [S.dim(f"{un} of {spend['requests']} requests had no price or incomplete token counts and are left out, not counted as free.")] if un else []


def by_project(spend: dict, proj: Projects) -> dict[str, float]:
    out: dict[str, float] = {}
    for sd in spend.get("session_days", []):
        out[proj.name(sd["session"])] = out.get(proj.name(sd["session"]), 0.0) + sd["usd"]
    return out


def day_axis(spend: dict, n_days: int) -> list[int]:
    last = max((d["day"] for d in spend.get("daily", [])), default=0)
    return [last - (n_days - 1 - i) * DAY_MS for i in range(n_days)]


# ---------------------------------------------------------------------------------------------
# 1. today

def cmd_today(spend: dict, o: dict, S: Style, proj: Projects) -> list[str]:
    if not spend["priced_requests"]:
        return ["No priced requests in this window yet."]
    total = spend["usd"]["total"]
    L = [S.b(f"What cost you most {o['label']}: {usd(total)}") + S.dim(f"  ({spend['priced_requests']} requests, observed, gateway rates)")]
    parts = partition(spend)
    L += ["", S.b("Top causes")]
    for i, r in enumerate(parts[:3], 1):
        extra = f"  [{usd(r['extra'])} more than a cache hit would have cost, {r['n']}x]" if r.get("extra") else ""
        L.append(f"  {i}. {usd(r['usd']):>8} {pct(r['usd'], total):>4}  {r['what']}{extra}")
    L += ["", S.b("Where the money went by token type")]
    rows = [[n, usd(v), pct(v, total), bar(v / total)] for n, v in
            (("cache reads", spend["usd"]["cache_read"]), ("cache writes", spend["usd"]["cache_write"]),
             ("output", spend["usd"]["output"]), ("uncached input", spend["usd"]["fresh"]))]
    L += table(rows, "lrrl")
    L += ["", S.b("By model")]
    L += table([[m["model"], usd(m["usd"]["total"]), pct(m["usd"]["total"], total), bar(m["usd"]["total"] / total)]
                for m in merged_models(spend)[:5]], "lrrl")
    projects = sorted(by_project(spend, proj).items(), key=lambda kv: -kv[1])
    L += ["", S.b("By project")]
    L += table([[n, usd(v), pct(v, total), bar(v / total)] for n, v in projects[:5]], "lrrl")
    L += ["", S.b("Costliest sessions")]
    L += table([[s["session"].split(":")[-1][:8], proj.name(s["session"]), short_model(s["model"]), f"{s['turns']} turns",
                 usd(s["usd"]["total"]), f"rewrites {usd(s['rewrite_usd'])}" if s["rewrites"] else ""]
                for s in spend["sessions"][:5]], "llllrl")
    L += [""] + ledger_lines(spend, S) + coverage_line(spend, S)
    return L


# ---------------------------------------------------------------------------------------------
# 3. cold

def cmd_cold(spend: dict, o: dict, S: Style, proj: Projects) -> list[str]:
    rw = spend.get("rewrites", {})
    total = spend["usd"]["total"]
    if not rw:
        return [f"No full-prefix cache rewrites {o['label']}. Nothing went cold."]
    extra = sum(c["usd"] for c in rw.values())
    L = [S.b(f"Cache rewrites {o['label']}: {sum(c['n'] for c in rw.values())} full re-writes, "
             f"{usd(extra)} more than cache hits would have cost") + S.dim(f"  ({pct(extra, total)} of {usd(total)} spend, observed)")]
    L.append(S.dim("A rewrite = cache_write >= 80% of the request's context, read from token counts (not cache_miss_reason)."))
    L += ["", S.b("By cause") + S.dim("   (cause is inferred from the pause length and message count)")]
    rows = [[k, str(c["n"]), ktok(c["tokens"]), usd(c["usd"]), bar(c["usd"] / extra if extra else 0), CAUSE_WORDS[k] if k in CAUSE_WORDS else ""]
            for k, c in sorted(rw.items(), key=lambda kv: -kv[1]["usd"])]
    L += table(rows, "lrrrll")
    gaps = [g for g in spend.get("idle_gaps", []) if g["n"]]
    if gaps:
        gtot = sum(g["usd"] for g in gaps) or 1
        L += ["", S.b("How long you were away before an expiry")]
        L += table([[g["band"], f"{g['n']}x", usd(g["usd"]), bar(g["usd"] / gtot)] for g in gaps], "lrrl")
        reach = sum(g["usd"] for g in gaps if g["band"] in ("5-10m", "10-30m", "30-60m"))
        L.append(S.dim(f"  Estimate: {usd(reach)} followed pauses of 5-60 min, the range a 1-hour cache lifetime would cover; "
                       "a longer lifetime or pings cost money too - see /context-guru:insights-idle for the net."))
    L += ["", S.b("Costliest rewrites")]
    L += table([[dt.datetime.fromtimestamp(e["ts"] / 1000, tz=o["tz"]).strftime("%a %H:%M"), proj.name(e["session"]),
                 short_model(e["model"]), ktok(e["context"]) + " ctx", ("after " + fmt_dur(e["gap_s"])) if e["gap_s"] >= 0 else "first message",
                 e["cause"], usd(e["usd"])] for e in spend.get("top_rewrites", [])[:5]], "llllllr")
    avoid = sum(rw[k]["usd"] for k in AVOIDABLE if k in rw)
    L += ["", f"  Possibly avoidable (everything except session starts): {usd(avoid)}  [observed spend, not a promised saving]"]
    L += coverage_line(spend, S)
    return L


# ---------------------------------------------------------------------------------------------
# 4. compact

def compact_economics(rate: dict, ctx: int) -> dict:
    """Return the three costs of the next turn. All ESTIMATES built on SUMMARY/SURVIVING_TOKENS."""
    w, r, out = rate["cache_write"], rate["cache_read"], rate["output"]
    n = min(ctx, SURVIVING_TOKENS)
    cold = ctx * w                                            # come back after expiry, no compaction
    warm = ctx * r                                            # come back in time, no compaction
    compact = ctx * r + SUMMARY_OUT_TOKENS * out + n * w + n * r  # compact while warm, then one turn
    return {"cold": cold, "warm": warm, "compact": compact, "extra_if_warm": compact - warm,
            "saved_if_cold": cold - compact}


def cmd_compact(spend: dict, o: dict, S: Style, proj: Projects) -> list[str]:
    now = o["now"]
    live = [s for s in spend.get("live", []) if now - s["last_ts"] <= 2 * 3600_000]
    if not live:
        return ["No session has touched the cache in the last 2 hours, so there is nothing warm to protect."]
    L = [S.b("Cache state of your recent sessions") + S.dim("  (context and last touch observed; the rest is estimate)")]
    rows, verdicts = [], []
    for s in live[:6]:
        rate = spend["rates"].get(s["model"])
        age = (now - s["last_ts"]) / 1000
        left = s["ttl_ms"] / 1000 - age
        state = f"warm {fmt_dur(left)} left" if left > 0 else f"COLD {fmt_dur(-left)} ago"
        name = proj.name(s["session"])
        if rate is None:
            rows.append([name, ktok(s["context"]), "1h" if s["ttl_1h"] else "5m", state, "no price for this model"])
            continue
        e = compact_economics(rate, s["context"])
        rows.append([name, ktok(s["context"]) + " tok", "1h tier" if s["ttl_1h"] else "5m tier", state,
                     f"next turn if cold {usd(e['cold'])}, if warm {usd(e['warm'])}"])
        if left <= 0:
            verdicts.append(f"  {name}: cache already expired. Your next message rewrites {ktok(s['context'])} tokens ({usd(e['cold'])}) "
                            "whatever you do - /compact now would pay that same rewrite, so it saves nothing on this turn. "
                            "Compact (or /clear with a handoff note) only to shrink every later turn.")
        elif e["saved_if_cold"] <= 0:
            verdicts.append(f"  {name}: warm, but the context is small enough that compacting does not pay. Leave it.")
        else:
            p = e["extra_if_warm"] / (e["extra_if_warm"] + e["saved_if_cold"])
            verdicts.append(f"  {name}: warm for {fmt_dur(left)}. Compact now costs ~{usd(e['extra_if_warm'])} extra if you come back in time "
                            f"and saves ~{usd(e['saved_if_cold'])} if you stay away past the TTL. "
                            f"Worth it if you think there is more than a {100*p:.0f}% chance you will be away that long.")
    L += table([["project", "context", "cache", "state", "cost of the next turn (estimate)"]] + rows, "lllll")
    L += [""] + verdicts
    L += ["", S.dim(f"Estimate assumptions: a compaction writes ~{ktok(SUMMARY_OUT_TOKENS)} tokens of summary and leaves ~{ktok(SURVIVING_TOKENS)} tokens of context; "
                    "it ignores the saving on later turns of a smaller context, so it understates the case for compacting a long session.")]
    return L


# ---------------------------------------------------------------------------------------------
# 2. unused

def unused_items(tools: dict, last: dict | None) -> list[dict]:
    """Never-called, removable, priced capabilities ranked by dollars carried. Context Guru's own
    components and Claude Code's built-ins are excluded here, once, for every caller."""
    lastmap: dict[str, tuple[int, int]] = {}
    for t in (last or {}).get("tools", []):
        for key in {t["name"], t.get("skill") or ""} - {""}:
            c, ts = lastmap.get(key, (0, 0))
            lastmap[key] = (c + t["calls"], max(ts, t["last_ts"]))
    srv_last: dict[str, int] = {}
    for name, (_, ts) in lastmap.items():
        m = re.match(r"mcp__(.+?)__", name)
        if m:
            srv_last[m.group(1)] = max(srv_last.get(m.group(1), 0), ts)
    items = []
    for s in tools.get("servers", []):
        if s["calls"] or OWN.search(s["server"]) or not s.get("priced"):
            continue
        items.append({"kind": "MCP server", "name": s["server"], "tokens": s["tokens"], "usd": s["unused_usd"],
                      "cmd": f"claude mcp remove {s['server']}", "last": srv_last.get(s["server"], 0)})
    for sk in (tools.get("skills") or {}).get("skills", []):
        if sk["calls"] or OWN.search(sk["name"]) or not sk.get("priced"):
            continue
        items.append({"kind": "skill", "name": sk["name"], "tokens": sk["tokens"], "usd": sk["unused_usd"],
                      "cmd": f'set "skillOverrides": {{"{sk["name"]}": "off"}} in ~/.claude/settings.json',
                      "last": lastmap.get(sk["name"], (0, 0))[1]})
    for t in tools.get("tools", []):
        if t["calls"] or t.get("builtin") or t["kind"] != "tool" or OWN.search(t["name"]) or not t.get("priced"):
            continue
        items.append({"kind": "client tool", "name": t["name"], "tokens": t["tokens"], "usd": t["unused_usd"],
                      "cmd": t["removal"].get("command", ""), "last": lastmap.get(t["name"], (0, 0))[1]})
    return sorted(items, key=lambda i: -i["usd"])


def cmd_unused(tools: dict | None, last: dict | None, o: dict, S: Style, days: float) -> list[str]:
    if not tools or not tools.get("totals"):
        return ["No tool inventory for this window. The proxy records it per session; try again after a few sessions."]
    items = unused_items(tools, last)
    if not items:
        return ["Every tool, MCP server and skill you carry was called at least once in this window."]
    carried = sum(i["usd"] for i in items)
    now, top = o["now"], items[:10]
    L = [S.b(f"Carried but never called in {days:.0f} days: {usd(carried)}") + S.dim("  (observed: declaration tokens re-read each turn, priced at the tier each turn billed)")]
    if days >= 2:
        L.append(f"  Removing the {len(top)} below saves about {usd(sum(i['usd'] for i in top) / days * 30)}/month if your usage stays the same  [estimate]")
    rows = []
    for i in top:
        age = (now - i["last"]) / DAY_MS if i["last"] else None
        note = ""
        if age is not None and age <= RECENT_USE_DAYS:
            note = S.warn(f"used {age:.0f}d ago - check first")
        elif age is not None:
            note = f"last used {age:.0f}d ago"
        rows.append([i["kind"], i["name"], f"{ktok(i['tokens'])}/turn", usd(i["usd"]), note, i["cmd"]])
    L += [""] + table(rows, "llrrll")
    if len(items) > 10:
        rest = items[10:]
        L.append(S.dim(f"  ... and {len(rest)} more, {usd(sum(i['usd'] for i in rest))} together"))
    asd = sum(t["unused_usd"] for t in tools.get("aside", []))
    L += ["", S.dim("Never listed: Context Guru's own components and Claude Code's built-in tools"
                    + (f" (carrying the unused built-ins costs {usd(asd)}; you cannot remove them)." if asd else ".")),
          S.dim("'client tool' = a tool declared by the agent that is neither MCP nor a skill (usually one of Claude Code's own newer tools); "
                "--disallowedTools hides it for that run."),
          S.dim("A server removal takes effect in the next session. `claude mcp remove` takes -s user|project|local; "
                "`claude mcp list` shows where each is defined.")]
    return L


# ---------------------------------------------------------------------------------------------
# 5. trends

def series(spend: dict, n: int, pick) -> list[float]:
    by = {d["day"]: d for d in spend.get("daily", [])}
    return [pick(by[d]) if d in by else 0.0 for d in day_axis(spend, n)]


def delta(vals: list[float]) -> str:
    h = len(vals) // 2
    a, b = sum(vals[:h]), sum(vals[h:])
    return "new" if a == 0 and b > 0 else (f"{100*(b-a)/a:+.0f}%" if a > 0 else "-")


def recommendations(spend: dict, tools: dict | None, days: float) -> list[dict]:
    rw, L, recs = spend.get("rewrites", {}), spend.get("ledger", {}), []
    if "idle_expiry" in rw:
        recs.append({"usd": rw["idle_expiry"]["usd"], "cls": "observed",
                     "what": f"{rw['idle_expiry']['n']} pauses outlived the cache; each re-sent the whole conversation. "
                             "Compact or /clear before a long break (`compact`), see `cold` for the pause lengths."})
    other = sum(rw[k]["usd"] for k in ("prefix_change", "history_rewrite", "new_thread") if k in rw)
    if other:
        recs.append({"usd": other, "cls": "observed",
                     "what": "Mid-session prompt changes forced a rewrite (toggling MCP servers, editing CLAUDE.md, rewinds, subagents). "
                             "Decide tools and MCP servers before the session starts."})
    carried = sum(i["usd"] for i in unused_items(tools, None)) if tools and tools.get("totals") else 0.0
    if carried:
        recs.append({"usd": carried, "cls": "observed carry; saving is an estimate",
                     "what": "MCP servers, skills and tools carried and never called (`unused` lists them with the command that removes each)."})
    ping = L.get("ping_usd", 0.0)
    if ping and ping > L.get("ping_saved_usd", 0.0):
        recs.append({"usd": ping - L.get("ping_saved_usd", 0.0), "cls": "observed cost vs modelled saving",
                     "what": "Keep-alive pings cost more than the rewrites they are modelled to have avoided. Check /context-guru:cache-strategy-picker."})
    if L.get("cg_llm_usd", 0) > L.get("gross_saved_usd", 0):
        recs.append({"usd": L["cg_llm_usd"] - L["gross_saved_usd"], "cls": "observed",
                     "what": "Context Guru's summariser spent more than it saved in this window; consider a lighter preset."})
    return sorted(recs, key=lambda r: -r["usd"])


def cmd_trends(spend: dict, tools: dict | None, o: dict, S: Style, proj: Projects) -> list[str]:
    n = max(2, min(30, int(round(window_days(spend))) or o["days"]))
    daily = series(spend, n, lambda d: d["usd"]["total"])
    total = spend["usd"]["total"]
    L = [S.b(f"Spend, last {n} days: {usd(total)}") + "  " + spark(daily) + S.dim(f"  (peak {usd(max(daily))}/day, observed)"),
         S.dim(f"  rewrite cost {spark(series(spend, n, lambda d: d['rewrite_usd']))}  {usd(sum(c['usd'] for c in spend.get('rewrites', {}).values()))} extra paid over cache hits")]
    mrows = []
    for m in merged_models(spend)[:5]:
        v = series(spend, n, lambda d, ids=m["ids"]: sum(d["by_model"].get(k, 0.0) for k in ids))
        mrows.append([m["model"], spark(v), usd(m["usd"]["total"]), pct(m["usd"]["total"], total), delta(v)])
    L += ["", S.b("By model") + S.dim("   (last column: second half of the window vs first)")] + table(mrows, "llrrr")
    per: dict[str, dict[int, float]] = {}
    for sd in spend.get("session_days", []):
        per.setdefault(proj.name(sd["session"]), {})
        per[proj.name(sd["session"])][sd["day"]] = per[proj.name(sd["session"])].get(sd["day"], 0.0) + sd["usd"]
    axis = day_axis(spend, n)
    prow = []
    for name, dd in sorted(per.items(), key=lambda kv: -sum(kv[1].values()))[:6]:
        v = [dd.get(d, 0.0) for d in axis]
        prow.append([name, spark(v), usd(sum(v)), pct(sum(v), total), delta(v)])
    L += ["", S.b("By project")] + table(prow, "llrrr")
    recs = recommendations(spend, tools, window_days(spend))
    L += ["", S.b("Ranked by dollars")]
    for i, r in enumerate(recs[:5], 1):
        L.append(f"  {i}. {usd(r['usd']):>8}  {r['what']}  {S.dim('[' + r['cls'] + ']')}")
    if not recs:
        L.append("  Nothing worth acting on in this window.")
    L += [""] + ledger_lines(spend, S) + coverage_line(spend, S)
    return L


# ---------------------------------------------------------------------------------------------
# driver

def resolve_window(cmd: str, o: dict) -> tuple[int, int]:
    """(since_ms, now_ms). `today` starts at local midnight; the others look back `days`."""
    now = o["now"]
    off = o["tz_min"] * 60_000
    midnight = (now + off) // DAY_MS * DAY_MS - off
    days = o["days"]
    if cmd == "today":
        return midnight - (days - 1) * DAY_MS, now
    return midnight - (days - 1) * DAY_MS, now


def run(cmd: str, fetch, opts: dict) -> tuple[str, object]:
    """Return (text, document). `fetch(path, params, heavy)` -> parsed JSON or None."""
    S = Style(opts.get("color", False))
    o = dict(opts)
    o.setdefault("now", int(time.time() * 1000))
    o.setdefault("tz_min", 0)
    o["tz"] = dt.timezone(dt.timedelta(minutes=o["tz_min"]))
    o.setdefault("days", {"today": 1, "unused": 7, "cold": 7, "compact": 1, "trends": 14}[cmd])
    o["label"] = "today" if cmd == "today" and o["days"] == 1 else f"in the last {o['days']} days"
    since, now = resolve_window(cmd, o)
    proj = o.get("projects") or Projects()
    doc: dict = {}
    if cmd == "unused":
        tools = fetch("/api/tools", {"since": since, "until": now}, True)
        last = fetch("/api/tools/last-used", None, False)
        doc = {"tools": tools, "last_used": last}
        return "\n".join(cmd_unused(tools, last, o, S, (now - since) / DAY_MS)), doc
    params = {"since": since, "tz": o["tz_min"], "now": now}
    spend = fetch("/api/spend/explain", params, True)
    if spend is None:
        return ("Could not read /api/spend/explain. The proxy is not running with --dashboard, is an older build, "
                "or this account may not read it."), None
    spend["window"].update(since=since, until=now)
    doc = {"spend": spend}
    if cmd == "today":
        lines = cmd_today(spend, o, S, proj)
    elif cmd == "cold":
        lines = cmd_cold(spend, o, S, proj)
    elif cmd == "compact":
        lines = cmd_compact(spend, o, S, proj)
    else:
        tools = fetch("/api/tools", {"since": since, "until": now}, True)
        doc["tools"] = tools
        lines = cmd_trends(spend, tools, o, S, proj)
    return "\n".join(lines), doc


def fixture_fetch(bundle: dict):
    keys = {"/api/spend/explain": "spend", "/api/tools": "tools", "/api/tools/last-used": "last_used"}
    return lambda path, params=None, heavy=False: bundle.get(keys[path])


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("cmd", choices=("today", "unused", "cold", "compact", "trends"))
    ap.add_argument("--days", type=int, help="window length in days (defaults differ per command)")
    ap.add_argument("--json", action="store_true", help="print the raw documents")
    ap.add_argument("--port")
    ap.add_argument("--fixture", help="read a captured bundle of API responses from this file (tests, demos)")
    ap.add_argument("--now", help="pretend it is this ISO time, e.g. 2026-10-10T23:13:00Z")
    ap.add_argument("--tz-offset-min", type=int, help="UTC offset for day boundaries (default: local)")
    ap.add_argument("--projects-dir", help="Claude Code projects dir (default ~/.claude/projects)")
    a = ap.parse_args(argv)
    opts: dict = {"color": sys.stdout.isatty() and "NO_COLOR" not in os.environ}
    if a.days:
        opts["days"] = a.days
    if a.now:
        opts["now"] = int(dt.datetime.fromisoformat(a.now.replace("Z", "+00:00")).timestamp() * 1000)
    opts["tz_min"] = a.tz_offset_min if a.tz_offset_min is not None else time.localtime().tm_gmtoff // 60
    if a.projects_dir:
        opts["projects"] = Projects(a.projects_dir)
    if a.fixture:
        with open(a.fixture, encoding="utf-8") as f:
            fetch = fixture_fetch(json.load(f))
    else:
        sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
        import insights  # the plugin's own fetcher and port resolution

        port = a.port or insights._resolve_port()[0]
        if not port:
            print("No Context Guru proxy is configured for this directory. Run /context-guru:install first.")
            return 0
        f = insights.Fetcher(port)
        fetch = lambda path, params=None, heavy=False: f.get(path, params, heavy=True)  # noqa: E731
    text, doc = run(a.cmd, fetch, opts)
    print(json.dumps(doc, indent=1) if a.json else text)
    return 0


if __name__ == "__main__":
    sys.exit(main())
