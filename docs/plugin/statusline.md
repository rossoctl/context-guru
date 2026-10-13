# Status line

The status line sits under the prompt and answers two questions all the time: **how much am I saving, and is my cache about to cost me?** It shows nothing in a project that is not routed through the proxy.

![Warm: green, seconds ticking down](../statusline-live/warm-dark.svg#only-dark)
![Warm: green, seconds ticking down](../statusline-live/warm-light.svg#only-light)

The first line is money, the second is the cache clock, the sub-agents, the context bar, the session's keep-alive pings and at most one hint:

```text
saved $0.31 of $4.10
cache ● █████░ 13:40 ↻ │ agents reviewer ● 4:21 · Explore ◐ 0:51 · 1 cold │ ctx ███░░░░░ 41% │ ka 3 pings $0.04
```

Every number is about this session. There is no today or lifetime total on the line; the dashboard and `/context-guru:savings` cover longer windows.

## What each piece means

| Piece | Meaning |
|---|---|
| `saved $0.31` | Saved this session: modelled against the same requests without the proxy, at your gateway's prices, net of the proxy's own spend. |
| `of $4.10` | What this session cost through the proxy, on the same prices: the agent's requests, the proxy's own model calls and its keep-alive pings. Left out until the proxy has priced a request. This is not Claude Code's own `cost` (its estimate at list prices), which `detailed` shows as `spent`. |
| `cache ● █████░ 3:57` | A bar that drains as the prompt cache approaches expiry, and the clock. Time is measured from the end of the last response. |
| `13:40 ↻` | The proxy is holding this cache warm with keep-alive pings. The clock is then the time until the cache goes cold if you send nothing: the last planned ping plus the cache lifetime. The bar still drains over the current cache entry, so each ping refills it. `↻` alone, with the normal clock, means the last touch was a ping and none is planned. |
| `agents reviewer ● 4:21 · Explore ◐ 0:51 · 1 cold` | Each sub-agent's cache, under the name Claude Code's agents list shows (`←` / `↓` in Claude Code), soonest to lapse first, up to three, then `+N more`. Each clock is that agent's time until cold, with `↻` when pings hold it. Cold ones are counted. See [Sub-agents](#sub-agents-and-the-selected-agent). |
| `ctx ███░░░░░ 41%` | How full you are, as a share of the model's context limit. |
| `ka 3 pings $0.04` | Keep-alive pings the proxy sent in this session, and what they cost. Shown after the first ping. |
| `▸ hint` | At most one thing worth acting on, dollars first. |
| `▲ update v0.3.2` | A newer proxy release is waiting; shown first so it cannot scroll off. |

### The cache clock

| State | Looks like | Rule |
|---|---|---|
| Warm | green `●` | more than 60 s left (a fifth of the lifetime on the 1 h tier) |
| Expiring | amber `◐` | 60 s to 15 s left; the hint prices what a pause would cost |
| Critical | red `◐ 0:09!` | the last 15 s |
| Cold | red `○ cold 1:15` | expired; counts up how long it has been cold |

The bar drains over the cache's own lifetime: 5 minutes, or 1 hour on the 1 h tier.

## Every state

### Warm: green, seconds ticking down

![Warm](../statusline-live/warm-dark.svg#only-dark)
![Warm](../statusline-live/warm-light.svg#only-light)

### Warm on the 1 h cache tier

The bar rescales to the tier's lifetime.

![Warm on the 1 h tier](../statusline-live/warm-1h-tier-dark.svg#only-dark)
![Warm on the 1 h tier](../statusline-live/warm-1h-tier-light.svg#only-light)

### Expiring: amber, with what a pause would cost

![Expiring](../statusline-live/expiring-dark.svg#only-dark)
![Expiring](../statusline-live/expiring-light.svg#only-light)

### Critical: red, last 15 seconds, marked `!`

![Critical](../statusline-live/critical-dark.svg#only-dark)
![Critical](../statusline-live/critical-light.svg#only-light)

### Cold: counts up how long it has been cold

![Cold](../statusline-live/cold-dark.svg#only-dark)
![Cold](../statusline-live/cold-light.svg#only-light)

### Keep-alive ping in place: `↻`

![Ping planned](../statusline-live/ping-planned-dark.svg#only-dark)
![Ping planned](../statusline-live/ping-planned-light.svg#only-light)

### <span id="sub-agents-and-the-selected-agent"></span>Sub-agents

Claude Code does not tell the status line which agent is selected in its agents list: the status line command receives the same input whichever agent you are viewing (checked against Claude Code 2.1.296). So the line always shows the main agent's cache on the left, and every sub-agent's cache by name, the same name the agents list shows. Select `reviewer` in the list and read the `reviewer` clock.

The names come from the session's own files next to its transcript (`<session>/subagents/agent-*.meta.json`). The proxy tells the agents apart by the `x-claude-code-agent-id` header Claude Code sends on every sub-agent and agent-team teammate request. A request the proxy grouped by its prompt instead (Claude Code's side requests, such as the auto-mode classifier) is not an agent and is not listed. A sub-agent with no name on disk keeps the unlabelled form, `agents 2 · soonest ◐ 0:51`.

Named agents, with the main cache and `reviewer` held warm by keep-alive pings and one finished agent counted as cold:

![Named sub-agents](../statusline-live/agents-named-dark.svg#only-dark)
![Named sub-agents](../statusline-live/agents-named-light.svg#only-light)

The unlabelled form: one running sub-agent, then several, then five.

![One sub-agent](../statusline-live/agents-1-dark.svg#only-dark)
![One sub-agent](../statusline-live/agents-1-light.svg#only-light)
![Several sub-agents](../statusline-live/agents-2-dark.svg#only-dark)
![Several sub-agents](../statusline-live/agents-2-light.svg#only-light)
![Five sub-agents](../statusline-live/agents-5-dark.svg#only-dark)
![Five sub-agents](../statusline-live/agents-5-light.svg#only-light)

### Model changed while the cache is warm

Switching model rewrites the cache, so the hint says so before you send.

![Model switch](../statusline-live/model-switch-dark.svg#only-dark)
![Model switch](../statusline-live/model-switch-light.svg#only-light)

### Tool set changed

From Claude Code's own miss report, when it is under two minutes old.

![Tool set changed](../statusline-live/tool-set-changed-dark.svg#only-dark)
![Tool set changed](../statusline-live/tool-set-changed-light.svg#only-light)

### Proxy offline

![Proxy offline](../statusline-live/proxy-offline-dark.svg#only-dark)
![Proxy offline](../statusline-live/proxy-offline-light.svg#only-light)

Refused is `cg ✕ proxy offline`. A slow proxy shows `cg: stats loading…`, then the last numbers with `⏳`. Hung for ten minutes shows `cg? not responding`.

### Narrow terminals and the worst case

At 80 columns, with the cache cold, five sub-agents and a hint:

![Worst case at 80 columns](../statusline-live/worst-case-80-dark.svg#only-dark)
![Worst case at 80 columns](../statusline-live/worst-case-80-light.svg#only-light)

At 40 columns the line sheds, in order: tools, latency, keep-alive pings, spend, the update marker, the long hint form, then the agents shrink to a count, then the `saved` total and label and the context bar. The cache clock is shortened last.

![40 columns](../statusline-live/narrow-40-dark.svg#only-dark)
![40 columns](../statusline-live/narrow-40-light.svg#only-light)

## Layouts

| Layout | Shows |
|---|---|
| `minimal` | one line: saved, cache, hint |
| `balanced` (default) | update, saved, cache, agents, context, keep-alive pings, hint |
| `detailed` | balanced plus `spent $4.10 · $2.73/h · up 1h30m` (the rate appears after 5 minutes), a slow-upstream note (only when upstream averages 3 s or more) and the least-used tool or skill |

![Detailed layout](../statusline-live/detailed-dark.svg#only-dark)
![Detailed layout](../statusline-live/detailed-light.svg#only-light)

Switch without editing settings, and keep the choice across renders:

```sh
python3 "<plugin dir>/scripts/statusline.py" --set-layout=detailed   # or minimal, balanced, auto
```

`auto` forgets the choice. A remembered layout wins over the flag in the installed command. To change the installed command instead, re-run `/context-guru:statusline` with `--layout=minimal|balanced|detailed`. Hide or show single elements with `--hide=a,b` and `--show=a,b` (`update save spend cache agents ctx hint latency tools ka`), or the environment variables `CG_STATUSLINE_LAYOUT`, `CG_STATUSLINE_HIDE`, `CG_STATUSLINE_SHOW` and `CG_STATUSLINE_COLUMNS`.

## Modes

Five more layouts each answer one question on the second line, next to the cache clock. Choose one with `--layout=<name>` in the installed command, or remember it with `statusline.py --set-layout=<name>` (`--set-layout=auto` goes back to the default).

| Layout | The question | What it shows |
|---|---|---|
| `spark` | How much have I saved lately? | `saved $4.20 today · last 14 days ▂▄▅…`: today's saving and a 14-day sparkline by your local calendar day; the rightmost cell is today |
| `strip` | Are my cache hits holding? | The last 20 requests as `■` hit, `▨` partial, `✖` miss (oldest on the left), and the dollars misses cost today (`$1.40 lost today`); from 100 columns a key is added |
| `heat` | When do I lose cache? | `misses/hour 0h … 24h  $1.40 lost today`: `░▒▓█` mean more misses in that hour, blank none, `·` hours not yet reached; the dollars are the whole day |
| `fill` | How full is my context, and with what? | A stacked bar: `▒` always loaded, `█` conversation, `░` free; the percent and, when it is filling, the time to full. At 100 columns or more, a legend with the two shares and what a miss now would cost |
| `watch` | Which sub-agent caches are about to lapse? | Line 1 the saved figure and the main clock; line 2 `agent caches (2) ████ 4:30  █░░░ 0:50`: one mini-bar per running sub-agent in id order, up to 4, then `+N more` |

All five show the cache clock but have no hint slot.

### spark

![spark](../statusline-live/mode-spark-dark.svg#only-dark)
![spark](../statusline-live/mode-spark-light.svg#only-light)

### strip

![strip](../statusline-live/mode-strip-dark.svg#only-dark)
![strip](../statusline-live/mode-strip-light.svg#only-light)

At 120 columns:

![strip at 120 columns](../statusline-live/mode-strip-120-dark.svg#only-dark)
![strip at 120 columns](../statusline-live/mode-strip-120-light.svg#only-light)

### heat

![heat](../statusline-live/mode-heat-dark.svg#only-dark)
![heat](../statusline-live/mode-heat-light.svg#only-light)

### fill

![fill](../statusline-live/mode-fill-dark.svg#only-dark)
![fill](../statusline-live/mode-fill-light.svg#only-light)

With room for the legend (120 columns):

![fill at 120 columns](../statusline-live/mode-fill-120-dark.svg#only-dark)
![fill at 120 columns](../statusline-live/mode-fill-120-light.svg#only-light)

### watch

![watch](../statusline-live/mode-watch-dark.svg#only-dark)
![watch](../statusline-live/mode-watch-light.svg#only-light)

These layouts draw with the same renderer as the [terminal views](terminal-views.md). `NO_COLOR` removes the colour, and `CG_ASCII=1` swaps the glyphs for plain characters (`o` hit, `~` partial, `X` miss, `#`, `=` and `-` for bars). That ASCII switch exists for these layouts only; the default layouts have none. Their data is cached (series and spend for 2 minutes, requests for 4 seconds) and a render never waits for it.

## <span id="refresh"></span>Refresh and the one-second clock

Claude Code redraws the status line on every message and, if you ask, on a timer. The installer writes `"refreshInterval": 1` next to the command, so the clock ticks every second (the minimum Claude Code accepts). A value you set yourself is kept when the status line is re-installed, so an install made earlier with a longer interval keeps it. To get the one-second clock on an existing install, set it by hand in the settings file that holds the status line:

```json
"statusLine": { "type": "command", "command": "…", "refreshInterval": 1 }
```

and start a new session. Older Claude Code versions ignore the key and redraw only on events. The clock is arithmetic on a timestamp the proxy supplies, so a redraw needs no new network read. A render takes about 55 ms warm and 90 ms on a cold cache. The cost is one short process per second per open session. If that matters, set a larger `refreshInterval` (for example 5); the clock then moves every 5 seconds.

## Render time

Every layout returns in about 0.4 s at worst, Python start-up included (the slowest run in the table took 0.43 s). The render has a 0.36 s budget. Only the session's totals (`/api/stats?lean=1&session=…`) are waited for, and the cache state is read in parallel under the same deadline. Everything the mode layouts add (the day total, series, spend, requests) comes from a disk cache that a detached background process refreshes, one per 30 seconds. A timeout shows the last numbers with `⏳`, never "proxy offline".

Measured on this host, which is loaded and shared with production: 50 runs per cell, wall milliseconds, median / 95th percentile / maximum. "Slow" is a server that sleeps 5 seconds on every request.

| layout | healthy, warm cache | healthy, cold cache | slow proxy | proxy down |
|---|---|---|---|---|
| minimal | 53 / 59 / 64 | 89 / 97 / 97 | 389 / 398 / 399 | 92 / 98 / 106 |
| balanced | 51 / 60 / 66 | 89 / 104 / 133 | 390 / 397 / 401 | 94 / 105 / 110 |
| detailed | 57 / 66 / 70 | 93 / 103 / 110 | 389 / 395 / 398 | 91 / 101 / 104 |
| spark | 55 / 64 / 64 | 90 / 102 / 108 | 389 / 393 / 396 | 92 / 104 / 127 |
| strip | 54 / 58 / 61 | 89 / 97 / 103 | 391 / 399 / 406 | 89 / 111 / 125 |
| heat | 55 / 60 / 67 | 90 / 99 / 101 | 391 / 410 / 425 | 92 / 111 / 116 |
| fill | 54 / 57 / 61 | 85 / 94 / 94 | 390 / 408 / 427 | 81 / 92 / 120 |
| watch | 55 / 59 / 63 | 91 / 101 / 104 | 391 / 412 / 416 | 91 / 97 / 99 |

The same harness on the status line before this work:

| layout | healthy, warm cache | healthy, cold cache | slow proxy | proxy down |
|---|---|---|---|---|
| minimal | 42 / 59 / 63 | 80 / 89 / 93 | 682 / 691 / 692 | 76 / 90 / 91 |
| balanced | 44 / 53 / 54 | 80 / 91 / 99 | 679 / 729 / 751 | 75 / 82 / 86 |
| detailed | 45 / 52 / 60 | 79 / 89 / 98 | 678 / 703 / 713 | 83 / 126 / 146 |

The slow-proxy case went from about 680 ms to about 390 ms. Healthy renders are about 10 ms slower than before, from the extra elements (agents, spend).

Per-agent names, the keep-alive plan and the session-only total (which removed the lifetime read) cost nothing measurable. Interleaved runs of the previous release ("before") and this one ("after") against the same server, with three named sub-agents on disk, 50 runs each, median / 95th percentile / maximum in milliseconds:

| layout | scenario | before | after |
|---|---|---|---|
| minimal | warm | 56 / 71 / 75 | 57 / 75 / 86 |
| minimal | cold | 94 / 112 / 140 | 88 / 101 / 118 |
| minimal | slow | 396 / 433 / 530 | 395 / 415 / 444 |
| minimal | down | 93 / 141 / 171 | 84 / 103 / 144 |
| balanced | warm | 56 / 62 / 65 | 55 / 69 / 75 |
| balanced | cold | 95 / 151 / 438 | 95 / 119 / 160 |
| balanced | slow | 390 / 407 / 420 | 391 / 401 / 402 |
| balanced | down | 90 / 105 / 176 | 85 / 110 / 154 |
| detailed | warm | 58 / 71 / 87 | 62 / 72 / 76 |
| detailed | cold | 87 / 99 / 107 | 90 / 102 / 116 |
| detailed | slow | 390 / 398 / 406 | 389 / 398 / 460 |
| detailed | down | 89 / 97 / 104 | 83 / 93 / 101 |
| watch | warm | 56 / 64 / 72 | 56 / 64 / 67 |
| watch | cold | 87 / 96 / 104 | 85 / 95 / 102 |
| watch | slow | 393 / 410 / 485 | 392 / 401 / 414 |
| watch | down | 88 / 109 / 166 | 83 / 92 / 177 |

Reading the names is one directory listing and a few small files, about 0.1 ms.

## Colour and plain terminals

Colours are the basic ANSI set plus dim, so they follow your terminal theme on light and dark backgrounds. `NO_COLOR=1` or `TERM=dumb` removes the colour; the clock, bar and symbols still carry the state. The default layouts have no ASCII mode: their bars and symbols are Unicode (the [modes](#modes) can switch to ASCII with `CG_ASCII=1`). `--layout=minimal` keeps to one short line.

## Turn it on, off, or change it

`/context-guru:statusline` installs it (the install also does this by default), changes the layout, or turns it off with `settings.py off`, which restores whatever status line you had before. It never sends a keep-alive ping; it only reads.
