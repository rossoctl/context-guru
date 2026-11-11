#!/usr/bin/env python3
"""Generate the central Grafana dashboards. Run it, and commit the JSON it writes.

Why a generator: a Grafana dashboard is a long JSON file in which a wrong query looks exactly like a
right one. Here every panel is one short line with its query, so a review can read them all.

    python3 gen_dashboards.py          # writes dashboards/*.json
    python3 gen_dashboards.py --check  # exit 1 if the committed JSON is out of date
"""
import json, os, sys

DS = {"type": "prometheus", "uid": "central-prom"}
FILTER = 'proxy=~"$proxy", machine=~"$machine"'


GREEN, ORANGE, RED, YELLOW, BLUE, PURPLE = "#73BF69", "#FF9830", "#F2495C", "#FADE2A", "#5794F2", "#B877D9"
USD = "currencyUSD"
# Label filters. They are constants because Python before 3.12 allows no backslash in an f-string.
X5 = ',status_class="5xx"'
DIR_SAVED, DIR_ADDED = ',direction="saved"', ',direction="added"'
RESCUE_SAVED, RESCUE_ADDED = ',effect="rescue",direction="saved"', ',effect="rescue",direction="added"'
TIER_READ = ',tier="cache_read"'
TIER_PROMPT = ',tier=~"cache_read|cache_write_5m|cache_write_1h|input"'


def var(name, label, query, multi=True):
    return {
        "name": name, "label": label, "type": "query", "datasource": DS,
        "query": {"query": query, "refId": "v"}, "refresh": 2, "sort": 1,
        "includeAll": True, "allValue": ".*", "multi": multi,
        "current": {"selected": True, "text": "All", "value": "$__all"},
    }


def inc(metric, by="", win="$__range", extra=""):
    """sum(increase(metric{filter}[win])), optionally by a label."""
    g = f" by ({by})" if by else ""
    return f'sum{g}(increase({metric}{{{FILTER}{extra}}}[{win}]))'


def rate(metric, by="", extra=""):
    g = f" by ({by})" if by else ""
    return f'sum{g}(rate({metric}{{{FILTER}{extra}}}[$__rate_interval]))'


def net(by="", fn="inc"):
    """saved - added, per label value. PromQL drops a label value that is on one side only, and a
    component that only ever ADDS cost (a compaction call, say) is exactly the one this must show.
    So each side is padded with the other side's label values times zero."""
    side = (lambda m: inc(m, by)) if fn == "inc" else (lambda m: rate(m, by))
    s, a = side("llm_savings_saved_usd_total"), side("llm_savings_added_usd_total")
    if not by:
        return f"({s} - {a})"
    return f"((({s}) or ({a}) * 0) - (({a}) or ({s}) * 0))"


def color(c):
    return {"mode": "fixed", "fixedColor": c}


def stat(pid, title, expr, x, y, w=4, h=4, unit="short", desc="", decimals=None, c=None, big=False):
    d = {"id": pid, "type": "stat", "title": title, "description": desc, "datasource": DS,
         "gridPos": {"x": x, "y": y, "w": w, "h": h},
         "targets": [{"expr": expr, "refId": "A", "instant": True, "datasource": DS}],
         "fieldConfig": {"defaults": {"unit": unit, "noValue": "no data"}, "overrides": []},
         "options": {"reduceOptions": {"calcs": ["lastNotNull"]}, "colorMode": "value", "graphMode": "none",
                     "textMode": "value"}}
    if decimals is not None:
        d["fieldConfig"]["defaults"]["decimals"] = decimals
    if c:
        d["fieldConfig"]["defaults"]["color"] = color(c)
    if big:
        d["options"]["text"] = {"valueSize": 56}
    return d


def series(pid, title, targets, x, y, w=12, h=8, unit="short", stack=False, desc="", kind="timeseries",
           colors=None, bars=False, vertical=False, instant=False, time_from=None, interval=None):
    d = {"id": pid, "type": kind, "title": title, "description": desc, "datasource": DS,
         "gridPos": {"x": x, "y": y, "w": w, "h": h},
         "targets": [{"expr": e, "legendFormat": l, "refId": chr(65 + i), "datasource": DS}
                     for i, (e, l) in enumerate(targets)],
         "fieldConfig": {"defaults": {"unit": unit, "custom": {"stacking": {"mode": "normal" if stack else "none"}}},
                         "overrides": []},
         "options": {"legend": {"displayMode": "table", "placement": "bottom"}}}
    if instant:
        for t in d["targets"]:
            t["instant"] = True
    if interval:
        for t in d["targets"]:
            t["interval"] = interval
    if time_from:
        d["timeFrom"] = time_from
    if bars:
        d["fieldConfig"]["defaults"]["custom"].update({"drawStyle": "bars", "fillOpacity": 80, "lineWidth": 0})
    if kind == "bargauge":
        d["options"] = {"orientation": "vertical" if vertical else "horizontal", "displayMode": "basic",
                        "showUnfilled": True, "reduceOptions": {"calcs": ["lastNotNull"]}}
        # a bar below zero is a loss: red. Above zero: green. Colors by value, not by series.
        d["fieldConfig"]["defaults"]["color"] = {"mode": "thresholds"}
        d["fieldConfig"]["defaults"]["thresholds"] = {"mode": "absolute", "steps": [
            {"color": RED, "value": None}, {"color": GREEN, "value": 0}]}
    if kind == "barchart":
        d["options"] = {"legend": {"displayMode": "hidden"}, "xTickLabelRotation": 0}
        d["fieldConfig"]["defaults"]["color"] = color(GREEN)
    for name, c in (colors or {}).items():
        d["fieldConfig"]["overrides"].append({"matcher": {"id": "byName", "options": name},
                                              "properties": [{"id": "color", "value": color(c)}]})
    return d


def table(pid, title, targets, x, y, w, h, join, transforms, desc="", unit_by_col=None):
    """Instant queries that share a label are joined into one row each."""
    d = {"id": pid, "type": "table", "title": title, "description": desc, "datasource": DS,
         "gridPos": {"x": x, "y": y, "w": w, "h": h},
         "targets": [{"expr": e, "refId": chr(65 + i), "datasource": DS, "instant": True, "format": "table"}
                     for i, e in enumerate(targets)],
         "transformations": [{"id": "joinByField", "options": {"byField": join, "mode": "outer"}}] + transforms,
         "fieldConfig": {"defaults": {}, "overrides": [
             {"matcher": {"id": "byName", "options": col},
              "properties": [{"id": "unit", "value": u}, {"id": "decimals", "value": 2}]}
             for col, u in (unit_by_col or {}).items()]},
         "options": {"showHeader": True}}
    return d


def text(pid, title, md, x, y, w, h):
    return {"id": pid, "type": "text", "title": title, "gridPos": {"x": x, "y": y, "w": w, "h": h},
            "options": {"mode": "markdown", "content": md}}


def dash(uid, title, desc, panels, tags):
    return {"uid": uid, "title": title, "description": desc, "tags": ["central"] + tags,
            "schemaVersion": 39, "version": 1, "editable": False, "refresh": "1m",
            "time": {"from": "now-7d", "to": "now"}, "timezone": "browser",
            "templating": {"list": [
                var("proxy", "Proxy", "label_values(llm_proxy_info, proxy)"),
                var("machine", "Machine", 'label_values(llm_proxy_info{proxy=~"$proxy"}, machine)'),
            ]},
            "panels": panels}


def fleet():
    f = FILTER
    P = []
    # Money first (spec 6.0, rule 1). The tiles cover the selected time range (default: 7 days).
    P.append(stat(1, "NET SAVED", net(), 0, 0, w=7, h=5, unit=USD, decimals=2, c=GREEN, big=True,
                  desc="Saved minus added over the selected range. This is the number that says whether the proxy pays. Cache effects are a breakdown and are not added."))
    P.append(stat(13, "Saving rate", f'{net()} / clamp_min({inc("llm_inference_cost_usd_total")} + {net()}, 1e-9)', 7, 0, w=4, h=5,
                  unit="percentunit", decimals=1, c=GREEN, big=True,
                  desc="Net saved as a share of what the traffic would have cost without the proxy. The baseline is spend plus net saved, which is what you would have paid. The exact baseline counter (llm_savings_baseline_usd_total) is not built yet."))
    P.append(stat(2, "Gross saved", inc("llm_savings_saved_usd_total"), 11, 0, w=4, h=5, unit=USD, decimals=2,
                  desc="Cost the proxy removed, before what it cost to do so."))
    P.append(stat(3, "Added cost", inc("llm_savings_added_usd_total"), 15, 0, w=4, h=5, unit=USD, decimals=2, c=ORANGE,
                  desc="What the proxy's own work cost: its model calls and the cache writes it caused."))
    P.append(stat(4, "Spend", inc("llm_inference_cost_usd_total"), 19, 0, w=5, h=5, unit=USD, decimals=2,
                  desc="What the providers billed over the selected range. Each proxy prices with its own rate table."))
    P.append(series(5, "Net saved per day", [(net(fn="inc").replace("$__range", "1d"), "net")], 0, 5, 24, 7, unit=USD,
                    kind="barchart", time_from="30d", interval="1d",
                    desc="One bar per day, for 30 days whatever the dashboard range. A bar below zero is a day the proxy cost more than it saved."))
    P.append(series(6, "Saved vs added (USD per hour)", [
        (f'{rate("llm_savings_saved_usd_total")} * 3600', "saved"),
        (f'{rate("llm_savings_added_usd_total")} * 3600', "added")], 0, 12, 12, 8, unit=USD,
        colors={"saved": GREEN, "added": ORANGE}, desc="Both sides of the net, as a rate."))
    P.append(series(7, "Net saved by machine", [(f"topk(8, {net('machine')})", "{{machine}}")], 12, 12, 12, 8,
                    unit=USD, kind="bargauge", instant=True,
                    desc="The 8 best machines over the selected range. The full list is on the Machine comparison dashboard."))
    P.append(stat(8, "Machines reporting", f'count(count by (machine) (llm_proxy_info{{{f}}}))', 0, 20, w=5, h=4,
                  desc="Machines that pushed in the last few minutes. llm_proxy_info is always 1, so an idle machine still counts."))
    P.append(stat(9, "Silent machines",
                  f'count(max by (proxy, machine) (max_over_time(llm_proxy_info{{{f}}}[1d])) unless max by (proxy, machine) (llm_proxy_info{{{f}}})) or vector(0)',
                  5, 20, w=5, h=4, c=YELLOW,
                  desc="Machines that pushed in the last day and have pushed nothing for a few minutes. The alert MachineSilent fires after 15."))
    P.append(stat(10, "5xx error rate",
                  f'{inc("llm_inference_requests_total", extra=X5)} / clamp_min({inc("llm_inference_requests_total")}, 1)',
                  10, 20, w=5, h=4, unit="percentunit", decimals=2, c=GREEN,
                  desc="Server errors only. A 4xx is usually the client's own request."))
    P.append(stat(11, "Requests per minute", f'{rate("llm_inference_requests_total")} * 60', 15, 20, w=5, h=4,
                  decimals=0, desc="Requests that reached all proxies."))
    P.append(stat(12, "Alerts firing", 'count(ALERTS{alertstate="firing"}) or vector(0)', 20, 20, w=4, h=4, c=GREEN,
                  desc="Alerts that Prometheus has in the firing state. The list is in Alertmanager."))
    return dash("central-fleet", "Fleet overview",
                "Do we save money, and is the fleet healthy? Tokens are the same for every proxy. Money is each proxy's own rate table.",
                P, ["fleet"])


def savings():
    f = FILTER
    P = []
    P.append(series(1, "Net by component (USD, selected range)", [(net("component"), "{{component}}")],
                    0, 0, 9, 9, unit=USD, kind="bargauge", instant=True,
                    desc="Saved minus added for each component. A red bar is a component that costs more than it saves."))
    P.append(series(2, "Net by kind (USD per hour)", [(f'{net("kind", fn="rate")} * 3600', "{{kind}}")],
                    9, 0, 15, 9, unit=USD, stack=True,
                    desc="rewrite, spend and setting. A spend is a component's own model call and is always a cost."))
    P.append(series(3, "Saved tokens by tier (selected range)", [(inc("llm_savings_saved_tokens_total", "tier"), "{{tier}}")],
                    0, 9, 7, 8, kind="bargauge", vertical=True, instant=True,
                    desc="Tokens the provider did not bill. The tier decides the price of each token."))
    P.append(series(4, "Cache effects (USD per day)", [
        (f'{inc("llm_savings_cache_effect_usd_total", "effect", "1d", DIR_SAVED)}', "{{effect}} saved"),
        (f'-{inc("llm_savings_cache_effect_usd_total", "effect", "1d", DIR_ADDED)}', "{{effect}} added")],
        7, 9, 9, 8, unit=USD, stack=True, bars=True, interval="1d",
        desc="A breakdown of how the cache changed the bill, saved above zero and added below. It is never added to saved or added."))
    P.append(series(5, "Break cost by cause", [
        (inc("llm_savings_cache_effect_usd_total", "cause", extra=',effect="break",direction="added"'), "{{cause}}")],
        16, 9, 8, 8, unit=USD, kind="bargauge", instant=True, desc="What cache breaks cost, by why they happened."))
    P.append(stat(6, "Estimated share",
                  f'{inc("llm_savings_estimated_total")} / clamp_min({inc("llm_savings_requests_total")}, 1)',
                  0, 17, w=4, h=8, unit="percentunit", decimals=1, c=YELLOW,
                  desc="Share of requests whose figure is an estimate. Above 20 percent, read the headline as a range."))
    P.append(series(7, "Unpriced requests by reason (per minute)", [(f'{rate("llm_savings_unpriced_total", "reason")} * 60', "{{reason}}")],
                    4, 17, 9, 8, stack=True, desc="Operations the accountant did not price, and why."))
    P.append(table(8, "Components with a negative net", [
        inc("llm_savings_saved_usd_total", "component"), inc("llm_savings_added_usd_total", "component")],
        13, 17, 11, 8, "component", [
            {"id": "calculateField", "options": {"mode": "binary", "alias": "net",
                                                 "binary": {"left": "Value #A", "operator": "-", "right": "Value #B"},
                                                 "replaceFields": False}},
            {"id": "filterByValue", "options": {"type": "include", "match": "all", "filters": [
                {"fieldName": "net", "config": {"id": "lower", "options": {"value": 0}}}]}},
            {"id": "organize", "options": {"excludeByName": {"Time": True},
                                           "renameByName": {"Value #A": "saved", "Value #B": "added"}}}],
        desc="Read this table first: a component here costs more than it saves. A component with no saved series at all shows no row; it appears in Net by component.",
        unit_by_col={"saved": USD, "added": USD, "net": USD}))
    return dash("central-savings", "Savings", "Where does the saving come from, and where does it leak?", P, ["savings"])


def cache():
    f = FILTER
    rescue = lambda e: rate("llm_savings_cache_effect_usd_total", extra=e)
    P = []
    P.append(series(1, "Cache read ratio", [
        (f'{rate("llm_inference_tokens_total", "model", TIER_READ)} / clamp_min({rate("llm_inference_tokens_total", "model", TIER_PROMPT)}, 1e-9)', "{{model}}")],
        0, 0, 12, 8, unit="percentunit", desc="cache_read / (cache_read + cache_write + input). Output tokens are not part of the prompt."))
    P.append(series(2, "Misses by cause (per hour)", [(f'{rate("llm_cache_misses_total", "cause")} * 3600', "{{cause}}")],
                    12, 0, 12, 8, stack=True, bars=True, desc="Cache misses per hour, by why."))
    P.append(series(3, "Miss cost by cause (USD, selected range)", [(inc("llm_cache_miss_cost_usd_total", "cause"), "{{cause}}")],
                    0, 8, 8, 8, unit=USD, kind="bargauge", instant=True,
                    desc="The write premium on the re-written prefix, priced with each model's own rate. A model with no rate counts the miss and books no money."))
    P.append(series(4, "Ping outcomes (per hour)", [(f'{rate("llm_keepalive_pings_total", "outcome")} * 3600', "{{outcome}}")],
                    8, 8, 10, 8, stack=True, bars=True,
                    colors={"ok": GREEN, "skipped": BLUE, "failed": RED, "timeout": ORANGE},
                    desc="Pings per hour. Only ok and failed are visible: a timed-out or skipped ping leaves no row."))
    P.append(stat(5, "Pings that paid",
                  f'({inc("llm_savings_cache_effect_usd_total", extra=RESCUE_SAVED)} - {inc("llm_savings_cache_effect_usd_total", extra=RESCUE_ADDED)}) / clamp_min({inc("llm_keepalive_cost_usd_total")}, 1e-9)',
                  18, 8, w=6, h=8, unit="suffix: x", decimals=1, c=GREEN, big=True,
                  desc="Rescue value divided by ping cost over the selected range. Above 1, the pings paid."))
    P.append(series(6, "Keep-alive: cost, rescue and net (USD per hour)", [
        (f'{rate("llm_keepalive_cost_usd_total")} * 3600', "ping cost"),
        (f'({rescue(RESCUE_SAVED)} - {rescue(RESCUE_ADDED)}) * 3600', "rescue (net of reads)"),
        (f'(({rescue(RESCUE_SAVED)} - {rescue(RESCUE_ADDED)}) - {rate("llm_keepalive_cost_usd_total")}) * 3600', "net")],
        0, 16, 16, 8, unit=USD, colors={"ping cost": ORANGE, "rescue (net of reads)": GREEN, "net": BLUE},
        desc="Do the pings pay? The rescue is a write that became a read, so its value is the write saved minus the read paid."))
    P.append(text(7, "Shared-warm tokens", "**No data yet.**\n\nThe counter `llm_keepalive_shared_warm_tokens_total` needs #853.", 16, 16, 8, 8))
    return dash("central-cache", "Cache and keep-alive", "Does the cache pay, and do pings pay?", P, ["cache"])


def machines():
    f = FILTER
    per1k = lambda by: f'1000 * {net(by)} / clamp_min({inc("llm_inference_requests_total", by)}, 1)'
    P = []
    P.append(series(1, "Net saved by machine (USD, selected range)", [(f"sort_desc({net('machine')})", "{{machine}}")],
                    0, 0, 10, 12, unit=USD, kind="bargauge", instant=True, desc="Every machine, best first."))
    P.append(table(2, "By preset", [
        f'count by (preset) (count by (preset, machine) (llm_proxy_info{{{f}}}))',
        net("preset"), f'{net("preset")} / clamp_min({inc("llm_inference_cost_usd_total", "preset")} + {net("preset")}, 1e-9)', per1k("preset"),
        f'{inc("llm_savings_estimated_total", "preset")} / clamp_min({inc("llm_savings_requests_total", "preset")}, 1)'],
        10, 0, 14, 12, "preset", [
            {"id": "organize", "options": {"excludeByName": {"Time": True}, "renameByName": {
                "Value #A": "machines", "Value #B": "net saved", "Value #C": "saving rate", "Value #D": "net per 1,000 requests",
                "Value #E": "estimated"}}}],
        desc="Saving rate is net saved divided by spend plus net saved (the exact baseline counter is not built). Net per 1,000 requests compares a busy machine with a quiet one.",
        unit_by_col={"net saved": USD, "saving rate": "percentunit", "net per 1,000 requests": USD, "estimated": "percentunit"}))
    P.append(series(3, "Net saved by proxy (USD, selected range)", [(net("proxy"), "{{proxy}}")], 0, 12, 10, 9,
                    unit=USD, kind="bargauge", vertical=True, instant=True,
                    desc="Each proxy prices with its own rate table, so compare these by tokens, not by money, when the tables differ."))
    P.append(series(4, "Net saved per 1,000 requests, by machine", [(f"sort_desc({per1k('machine')})", "{{machine}}")],
                    10, 12, 14, 9, unit=USD, kind="bargauge", vertical=True, instant=True,
                    desc="A fair comparison between a busy machine and a quiet one."))
    return dash("central-machines", "Machine comparison", "Which machines or configs save the most?", P, ["machines"])


DASHBOARDS = {"fleet.json": fleet, "savings.json": savings, "cache.json": cache, "machines.json": machines}


def main():
    out = os.path.join(os.path.dirname(os.path.abspath(__file__)), "dashboards")
    os.makedirs(out, exist_ok=True)
    stale = False
    for name, fn in DASHBOARDS.items():
        text = json.dumps(fn(), indent=2, sort_keys=False) + "\n"
        path = os.path.join(out, name)
        if "--check" in sys.argv:
            if not os.path.exists(path) or open(path).read() != text:
                print(f"out of date: {path}"); stale = True
        else:
            open(path, "w").write(text); print("wrote", path)
    sys.exit(1 if stale else 0)


if __name__ == "__main__":
    main()
