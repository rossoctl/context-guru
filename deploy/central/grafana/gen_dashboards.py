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


def var(name, label, query, multi=True):
    return {
        "name": name, "label": label, "type": "query", "datasource": DS,
        "query": {"query": query, "refId": "v"}, "refresh": 2, "sort": 1,
        "includeAll": True, "allValue": ".*", "multi": multi,
        "current": {"selected": True, "text": "All", "value": "$__all"},
    }


def stat(pid, title, expr, x, y, w=4, h=4, unit="short", desc="", decimals=None):
    d = {"id": pid, "type": "stat", "title": title, "description": desc, "datasource": DS,
         "gridPos": {"x": x, "y": y, "w": w, "h": h},
         "targets": [{"expr": expr, "refId": "A", "instant": True, "datasource": DS}],
         "fieldConfig": {"defaults": {"unit": unit, "noValue": "no data"}, "overrides": []},
         "options": {"reduceOptions": {"calcs": ["lastNotNull"]}, "colorMode": "value"}}
    if decimals is not None:
        d["fieldConfig"]["defaults"]["decimals"] = decimals
    return d


def series(pid, title, targets, x, y, w=12, h=8, unit="short", stack=False, desc="", kind="timeseries"):
    d = {"id": pid, "type": kind, "title": title, "description": desc, "datasource": DS,
         "gridPos": {"x": x, "y": y, "w": w, "h": h},
         "targets": [{"expr": e, "legendFormat": l, "refId": chr(65 + i), "datasource": DS}
                     for i, (e, l) in enumerate(targets)],
         "fieldConfig": {"defaults": {"unit": unit, "custom": {"stacking": {"mode": "normal" if stack else "none"}}},
                         "overrides": []},
         "options": {"legend": {"displayMode": "table", "placement": "bottom"}}}
    return d


def dash(uid, title, desc, panels, tags):
    return {"uid": uid, "title": title, "description": desc, "tags": ["central"] + tags,
            "schemaVersion": 39, "version": 1, "editable": False, "refresh": "1m",
            "time": {"from": "now-24h", "to": "now"}, "timezone": "browser",
            "templating": {"list": [
                var("proxy", "Proxy", "label_values(llm_proxy_info, proxy)"),
                var("machine", "Machine", 'label_values(llm_proxy_info{proxy=~"$proxy"}, machine)'),
            ]},
            "panels": panels}


def fleet():
    f = FILTER
    inc = lambda m: f'sum(increase({m}{{{f}}}[$__range]))'
    P = []
    # Row of headline numbers. "Today" is the selected time range, so the same panels answer "this week".
    P.append(stat(1, "Machines reporting", f'count(count by (machine) (llm_proxy_info{{{f}}}))', 0, 0,
                  desc="Machines that pushed in the last few minutes. llm_proxy_info is always 1, so an idle machine still counts."))
    P.append(stat(2, "Spend", inc("llm_inference_cost_usd_total"), 4, 0, unit="currencyUSD", decimals=2,
                  desc="What the providers billed over the selected time range."))
    P.append(stat(3, "Gross saved", inc("llm_savings_saved_usd_total"), 8, 0, unit="currencyUSD", decimals=2,
                  desc="Cost the proxy removed, before what it cost to do so."))
    P.append(stat(4, "Added cost", inc("llm_savings_added_usd_total"), 12, 0, unit="currencyUSD", decimals=2,
                  desc="What the proxy's own work cost: its model calls and the cache writes it caused."))
    P.append(stat(5, "Net saved",
                  f'sum(increase(llm_savings_saved_usd_total{{{f}}}[$__range])) - sum(increase(llm_savings_added_usd_total{{{f}}}[$__range]))',
                  16, 0, unit="currencyUSD", decimals=2,
                  desc="Saved minus added. This is the number that says whether the proxy pays. Cache effects are a breakdown and are not added."))
    P.append(stat(6, "5xx error rate",
                  f'sum(increase(llm_inference_requests_total{{{f},status_class="5xx"}}[$__range])) / clamp_min(sum(increase(llm_inference_requests_total{{{f}}}[$__range])), 1)',
                  20, 0, unit="percentunit", decimals=2,
                  desc="Server errors only. A 4xx is usually the client's own request."))
    P.append(series(7, "Requests per minute", [
        (f'sum by (proxy) (rate(llm_inference_requests_total{{{f}}}[$__rate_interval])) * 60', "{{proxy}}")], 0, 4, 12, 8,
        desc="Requests that reached each proxy."))
    P.append(series(8, "Saved vs added (USD per hour)", [
        (f'sum(rate(llm_savings_saved_usd_total{{{f}}}[$__rate_interval])) * 3600', "saved"),
        (f'sum(rate(llm_savings_added_usd_total{{{f}}}[$__rate_interval])) * 3600', "added")], 12, 4, 12, 8, unit="currencyUSD",
        desc="Both sides of the net, as a rate."))
    P.append(series(9, "Net saved per day", [
        (f'sum(increase(llm_savings_saved_usd_total{{{f}}}[1d])) - sum(increase(llm_savings_added_usd_total{{{f}}}[1d]))', "net")],
        0, 12, 24, 8, unit="currencyUSD", kind="barchart",
        desc="One bar per day. A bar below zero is a day the proxy cost more than it saved."))
    return dash("central-fleet", "Fleet overview",
                "Is the fleet healthy, and do we save money? Tokens are the same for every proxy. Money is each proxy's own rate table.",
                P, ["fleet"])


def savings():
    f = FILTER
    P = []
    P.append(series(1, "Net by component (USD, selected range)", [
        (f'sum by (component) (increase(llm_savings_saved_usd_total{{{f}}}[$__range])) - sum by (component) (increase(llm_savings_added_usd_total{{{f}}}[$__range]))', "{{component}}")],
        0, 0, 12, 9, unit="currencyUSD", kind="bargauge",
        desc="Saved minus added for each component. A negative bar is a component that costs more than it saves."))
    P.append(series(2, "Components with negative net", [
        (f'(sum by (component) (increase(llm_savings_saved_usd_total{{{f}}}[$__range])) - sum by (component) (increase(llm_savings_added_usd_total{{{f}}}[$__range]))) < 0', "{{component}}")],
        12, 0, 12, 9, unit="currencyUSD", kind="bargauge",
        desc="Only the components below zero. Candidates to turn off (#670)."))
    P.append(series(3, "Net by kind", [
        (f'sum by (kind) (increase(llm_savings_saved_usd_total{{{f}}}[$__rate_interval])) - sum by (kind) (increase(llm_savings_added_usd_total{{{f}}}[$__rate_interval]))', "{{kind}}")],
        0, 9, 12, 8, unit="currencyUSD", stack=True,
        desc="rewrite, spend and setting. A spend is a component's own model call and is always a cost."))
    P.append(series(4, "Saved tokens by tier", [
        (f'sum by (tier) (rate(llm_savings_saved_tokens_total{{{f}}}[$__rate_interval])) * 60', "{{tier}}")],
        12, 9, 12, 8, stack=True, desc="Tokens per minute. The tier decides the price of each token."))
    P.append(series(5, "Cache effects (USD per hour)", [
        (f'sum by (effect, direction) (rate(llm_savings_cache_effect_usd_total{{{f}}}[$__rate_interval])) * 3600', "{{effect}} {{direction}}")],
        0, 17, 12, 8, unit="currencyUSD", stack=True,
        desc="A breakdown of how the cache changed the bill. It is never added to saved or added."))
    P.append(series(6, "Break cost by cause", [
        (f'sum by (cause) (increase(llm_savings_cache_effect_usd_total{{{f},effect="break",direction="added"}}[$__range]))', "{{cause}}")],
        12, 17, 12, 8, unit="currencyUSD", kind="bargauge", desc="What cache breaks cost, by why they happened."))
    P.append(stat(7, "Estimated share",
                  f'sum(increase(llm_savings_estimated_total{{{f}}}[$__range])) / clamp_min(sum(increase(llm_savings_requests_total{{{f}}}[$__range])), 1)',
                  0, 25, 6, 4, unit="percentunit", decimals=1,
                  desc="Share of requests whose figure is an estimate. Above 20 percent, read the headline as a range."))
    P.append(series(8, "Unpriced requests by reason", [
        (f'sum by (reason) (rate(llm_savings_unpriced_total{{{f}}}[$__rate_interval])) * 60', "{{reason}}")],
        6, 25, 18, 8, stack=True, desc="Operations the accountant did not price, and why. Per minute."))
    return dash("central-savings", "Savings",
                "Where does the saving come from, and where does it leak?", P, ["savings"])


def cache():
    f = FILTER
    P = []
    P.append(series(1, "Cache read ratio", [
        (f'sum by (model) (rate(llm_inference_tokens_total{{{f},tier="cache_read"}}[$__rate_interval])) / clamp_min(sum by (model) (rate(llm_inference_tokens_total{{{f},tier=~"cache_read|cache_write_5m|cache_write_1h|input"}}[$__rate_interval])), 1e-9)', "{{model}}")],
        0, 0, 12, 8, unit="percentunit", desc="cache_read / (cache_read + cache_write + input). Output tokens are not part of the prompt."))
    P.append(series(2, "Misses by cause", [
        (f'sum by (cause) (rate(llm_cache_misses_total{{{f}}}[$__rate_interval])) * 60', "{{cause}}")],
        12, 0, 12, 8, stack=True, desc="Cache misses per minute, by why."))
    P.append(series(3, "Miss cost by cause (USD, selected range)", [
        (f'sum by (cause) (increase(llm_cache_miss_cost_usd_total{{{f}}}[$__range]))', "{{cause}}")],
        0, 8, 12, 8, unit="currencyUSD", kind="bargauge",
        desc="The write premium on the re-written prefix, priced with each model's own rate. A model with no rate counts the miss and books no money."))
    P.append(series(4, "Ping outcomes", [
        (f'sum by (outcome) (rate(llm_keepalive_pings_total{{{f}}}[$__rate_interval])) * 60', "{{outcome}}")],
        12, 8, 12, 8, stack=True, desc="Pings per minute. Only ok and failed are visible: a timed-out or skipped ping leaves no row."))
    P.append(series(5, "Keep-alive: cost, rescue and net (USD per hour)", [
        (f'sum(rate(llm_keepalive_cost_usd_total{{{f}}}[$__rate_interval])) * 3600', "ping cost"),
        (f'(sum(rate(llm_savings_cache_effect_usd_total{{{f},effect="rescue",direction="saved"}}[$__rate_interval])) - sum(rate(llm_savings_cache_effect_usd_total{{{f},effect="rescue",direction="added"}}[$__rate_interval]))) * 3600', "rescue (net of reads)"),
        (f'((sum(rate(llm_savings_cache_effect_usd_total{{{f},effect="rescue",direction="saved"}}[$__rate_interval])) - sum(rate(llm_savings_cache_effect_usd_total{{{f},effect="rescue",direction="added"}}[$__rate_interval]))) - sum(rate(llm_keepalive_cost_usd_total{{{f}}}[$__rate_interval]))) * 3600', "net")],
        0, 16, 24, 8, unit="currencyUSD",
        desc="Do the pings pay? The rescue is a write that became a read, so its value is the write saved minus the read paid."))
    return dash("central-cache", "Cache and keep-alive", "Does the cache pay, and do pings pay?", P, ["cache"])


DASHBOARDS = {"fleet.json": fleet, "savings.json": savings, "cache.json": cache}


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
