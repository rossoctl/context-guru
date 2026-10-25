# Status line

The status line sits under the prompt and answers two questions all the time: **how much am I saving, and is my cache about to cost me?** It shows nothing in a project that is not routed through the proxy.

The default layout has two lines:

```text
saved $18.60 of $61.40 session · saved $4.20 of $20.50 subagents
cache ● █████░ 4:00 ↻ │ ctx ███░░░░░ 41%
```

Line 1 is money. Line 2 is the main agent's cache clock and the context bar. The status line owns the money, the clock and the context bar. The [Context Guru mod](#the-mod) owns the next-message price, the sub-agent caches, the weekly tip and the Ping button.

Only savings recorded with each request count: the compaction saving (against the same request uncompacted), the cache split, and the keep-alive credit, which a request gets only when a ping that read the cache kept it warm. Two credits that the dashboard prices when it is read are left out: the estimate for requests recorded before the cache split was measured, and the tool (declaration) filter's saving. So the line can read lower than the dashboard, which adds both. A figure the proxy did not report is left out, never shown as `$0.00`. The figures are not Claude Code's own `cost` (its estimate at list prices), which `detailed` shows as `spent`.

Every number is about this session. There is no today or lifetime total on the line; the dashboard and `/context-guru:insights savings` cover longer windows.

## On a Claude subscription

A subscription does not bill per token. The limit is the 5-hour window. When Claude Code sends `rate_limits` in its status-line input, the first line shows the window, not money:

```text
5h 34% · saved 1.2M rd / 85.0k wr
cache ● █████░ 3:57 │ ctx ███░░░░░ 41%
```

| Piece | Meaning | Source |
|---|---|---|
| `5h 34%` | How much of the 5-hour limit you used. `--layout=detailed` adds `7d 12%`. | Claude Code's `rate_limits.five_hour.used_percentage` |
| `rd` | Tokens compaction kept out of the prompt on **later turns** that re-send the transcript. They would have been cache reads. | `/api/stats` `replay_tokens` (`saved_gross − saved_unique`) |
| `wr` | Tokens compaction removed, **counted once**. Sent as new content, they would have been cache writes. | `/api/stats` `saved_unique` |

- The numbers are plain token counts. **They are not weighted.** Anthropic does not publish how the limit weights reads, writes and fresh input, so the status line does not guess.
- The window starts at `five_hour.resets_at` minus 5 hours. The counts cover all traffic through this proxy since then, not only this session.
- The counts are the proxy's own token estimates, and cover compaction only. Cachesplit, keep-alive and declaration-filter savings exist only as dollar figures, so they have no token count here.
- A kind with nothing saved is left out. With nothing saved, or no reset time, the line shows `5h 34%` only.
- No dollar figure shows on a subscription. Each money element of the API-key line:
  - `saved $x of $y session` and `subagents`: replaced by the window counts.
  - `↻ $z` manual-ping saving: goes with them.
  - `next $a warm · $b cold` (opt-in with `--show=next`): hidden. It is a price, and a subscription is not billed per request.
  - Per-agent `saved $x of $y` (opt-in with `--show=agents`): dropped.
  - `keep-alive 3 pings` (detailed): keeps the count, drops the cost.
  - `spent` (detailed): hidden.
- An ended window (`resets_at` in the past, before Claude Code sends a new block) shows nothing, and no money. At narrow widths the counts go first. `5h 34%` stays longest.
- `--layout=detailed` writes `compaction saved …`, so the line names what the counts cover.
- With an API key, Bedrock or Vertex there is no `rate_limits`. The money view above stays.

## What each piece means

| Piece | Meaning |
|---|---|
| `saved $18.60 of $61.40 session` | The main agent's requests. `$61.40` is what they would have cost without context-guru, at your gateway's prices. `$18.60` is the saving against that: the same `$61.40` minus what was actually paid, including context-guru's own model calls and keep-alive pings. So the saving is always part of the total, never more. |
| `saved $4.20 of $20.50 subagents` | The same, summed over every other request of the session: sub-agents, teammates, and Claude Code's side requests (the auto-mode classifier, titles), which cost money even though they are not listed as agents. Shown once the session has a request outside the main agent. A narrow terminal drops it first. |
| `cache ● █████░ 4:00` | A bar that drains as the prompt cache approaches expiry, and the clock (`m:ss`, or `h:mm:ss` from one hour). Time is measured from the end of the last response. |
| `4:00 ↻` | The proxy is holding this cache warm with keep-alive pings. The clock is then the time until the cache goes cold if you send nothing: the last planned ping plus the cache lifetime. The bar still drains over the current cache entry, so each ping refills it. `↻` alone, with the normal clock, means the last touch was a ping and none is planned. |
| `cache ○ cold 3:12` | The cache expired 3 minutes 12 seconds ago. Drawn dim, never red or amber. The next message pays the rewrite. `cache ○ cold` alone when the time since is unknown. |
| `ctx ███░░░░░ 41%` | How full you are, as a share of the model's context limit. Adds `⚠ rewrite near` from 85%. |
| `▲ update v0.3.2` | A newer proxy release is waiting; shown first so it cannot scroll off. |
| `⏳` | The figures are the last ones read. The proxy is slow, or the copy is old. The money, the context bar and the clock stay on screen; the clock is arithmetic on the copy's own timestamps, so it is right at any age. |

The line has no `next` price, no keep-alive count, no sub-agent clocks and no tip. They moved into the [mod](#the-mod). `--show=next`, `--show=agents`, `--show=ka` and `--show=tip` bring them back.

### The cache clock

| State | Looks like | Rule |
|---|---|---|
| Warm | green `●` | more than 60 s left (a fifth of the lifetime on the 1 h tier) |
| Expiring | amber `◐` | 60 s to 15 s left |
| Critical | red `◐ 0:09!` | the last 15 s |
| Cold | dim `cache ○ cold 3:12` | expired; counts up how long ago. Never red or amber |
| Pinged | cyan `● 13:40 ↻` | kept warm by keep-alive or manual pings |

The bar drains over the cache's own lifetime: 5 minutes, or 1 hour on the 1 h tier.

## Every state

Each render below is the real output of `statusline.py` (colour removed). The recordings that showed the older lines are gone.

Warm, green, seconds ticking down:

```text
saved $18.60 of $61.40 session · saved $4.20 of $20.50 subagents
cache ● █████░ 4:00 │ ctx ███░░░░░ 41%
```

Warm on the 1 h cache tier. The bar rescales to the tier's lifetime, and the clock shows `h:mm:ss` from one hour:

```text
saved $18.60 of $61.40 session
cache ● ██████ 59:00 │ ctx ███░░░░░ 41%
```

Expiring, amber, the last minute (the last fifth of the lifetime on the 1 h tier):

```text
saved $18.60 of $61.40 session
cache ◐ █░░░░░ 0:42 │ ctx ███░░░░░ 41%
```

Critical, red, the last 15 seconds, marked `!`:

```text
saved $18.60 of $61.40 session
cache ◐ ░░░░░░ 0:10! │ ctx ███░░░░░ 41%
```

Cold, dim, counting up how long ago it expired. The segment stays on the line, so you see the moment a pause started to cost money:

```text
saved $18.60 of $61.40 session
cache ○ cold 3:12 │ ctx ███░░░░░ 41%
```

Held warm by keep-alive pings, cyan, with `↻`. The clock is the time until it goes cold if you send nothing:

```text
saved $18.60 of $61.40 session
cache ● █████░ 13:40 ↻ │ ctx ███░░░░░ 41%
```

Proxy offline is `cg ✕ proxy offline`. A slow proxy shows `cg: stats loading…` only when there was never any answer. Otherwise it shows the last figures with `⏳`. A proxy that does not answer for ten minutes shows `cg? not responding`, unless a copy of the cache state exists: then the figures stay, with `⏳`.

### Narrow terminals

A line too wide for the terminal sheds, in this order: spend and latency (detailed), keep-alive, the update marker, the `subagents` phrase, the `session` label, the context bar. The cache clock is shortened last.

```text
saved $18.60 of $61.40 session · saved $4.20 of $20.50 subagents     (80 columns)
cache ● █████░ 4:00 │ ctx ███░░░░░ 41%

saved $18.60 of $61.40 session                                       (62 columns)
cache ● █████░ 4:00 │ ctx ███░░░░░ 41%

saved $18.60 of $61.40                                               (28 columns)
cache ● 4:00 │ ctx 41%
```

## Layouts

| Layout | Shows |
|---|---|
| `minimal` | one line: saved, cache (`saved $18.60 of $61.40 │ cache ● █████░ 4:00`; cold is `cache ○ cold`) |
| `balanced` (default) | update, saved, cache, context |
| `detailed` | balanced plus `spent $4.10 · $2.73/h · up 1h30m` (the rate appears after 5 minutes), the keep-alive count in plain words (`keep-alive 3 pings · $0.04`), a slow-upstream note (only when upstream averages 3 s or more) and the least-used tool or skill |

Switch without editing settings, and keep the choice across renders:

```sh
python3 "<plugin dir>/scripts/statusline.py" --set-layout=detailed   # or minimal, balanced, auto
```

`auto` forgets the choice. A remembered layout wins over the flag in the installed command. To change the installed command instead, re-run `/context-guru:statusline` with `--layout=minimal|balanced|detailed`. Hide or show single elements with `--hide=a,b` and `--show=a,b` (`update save spend cache next agents ctx ka latency tools tip`), or the environment variables `CG_STATUSLINE_LAYOUT`, `CG_STATUSLINE_HIDE`, `CG_STATUSLINE_SHOW` and `CG_STATUSLINE_COLUMNS`. `next`, `agents`, `ka` and `tip` are off by default: the mod shows them.

The layouts `spark`, `strip`, `heat`, `fill` and `watch` are retired. They moved into the mod's pane (below). A flag that names one falls back to `balanced`. A remembered choice that names one is ignored, and the installed layout applies. Neither gives an error.

### Opt-in elements

These are off in every layout. Turn one on with `--show=<name>` in the installed command.

| Element | Shows | Where it lives by default |
|---|---|---|
| `next` | `next $0.03 warm · $0.30 cold` (`next $0.30` once cold; `next $0.30 if cold` on a proxy that predates the warm figure) | the mod's band |
| `agents` | `agents reviewer active ● 4:58 saved $1.20 of $6.00 · Explore ◐ 0:51 · 1 cold` | the mod's band and pane |
| `ka` | `keep-alive 3 pings · $0.04`, after the first ping (also in `detailed`) | none: plain words, no `ka` |
| `tip` | the weekly tip, as its own last line | the mod's band |

The status line cannot follow the agent you select in Claude Code's agents list. Claude Code does not pass the selected agent to the command (checked against Claude Code 2.1.296). With `--show=agents` the line lists every agent by the name the list shows. The proxy tells agents apart by the `x-claude-code-agent-id` header. A request it grouped by prompt instead (a side request, such as the auto-mode classifier) is not an agent and is not listed.

The weekly tip names each of `/context-guru:save-more` and `/context-guru:remove-mcp-and-skills` (of the ones your plugin has) that has not run in the last 7 days. The commands record their runs in `last-run.json` in the plugin's state directory. The status line and the mod only read that file, so the tip costs no network call. A command that has never run counts from the first time the status line saw the install, so a new install sees no tip in its first week. Turn it off with `python3 "<plugin dir>/scripts/statusline.py" --set-tips=off`.

## <span id="the-mod"></span>The mod: band and pane

Claude Code can load the plugin as a mod. It draws a calm band above the prompt and a pane. The mod never reads the network itself. It runs `scripts/cg.py`, which uses the same on-disk cache as the status line. So the two never ask the proxy twice.

One home per fact:

| Fact | Where |
|---|---|
| saved $x of $y (session, subagents) | status line, line 1 |
| the main cache clock, `↻` | status line |
| context fill | status line |
| the price of the next message, warm and cold | band |
| Ping | band, pane and `/ping` |
| sub-agent caches | band (row 2) and pane |
| the weekly tip | band (row 3, only when stale) |
| spark, strip, heat, fill | pane |

### The band

At most three rows. Usually one.

```text
◆ next message $0.06 · $0.78 if cold [ Ping ]  hide
└ agents  ● Explore 5m  ◐ code-reviewer <1m  ○ planner
tip: /context-guru:save-more hasn't run in 7+ days — finds a cheaper config
```

- **Row 1.** `◆`, then the price of the next message: warm in green, cold dim. Then **Ping** (key `p`) and a small `hide`. The dashboard link is in the pane header: the terminal draws a link's address next to its label, which would wrap the row.
- **Cold cache.** The row changes to `◆ ○ cache cold · next message $0.78`. The price is amber. There is no Ping: a ping cannot help a cache that is gone.
- **Ping off.** When the server says it cannot ping, the button is replaced by a dim reason, such as `ping off: turns on next session` or `ping off: not on this route` (a gateway, or extended thinking). If the server does not say (an older server), the button stays.
- **Row 2, sub-agents.** On by default. One chip per named agent that made a request in the last 30 minutes, most recent first, at most four, then `+N`. `●` green is warm, `◐` amber is the last minute, cyan with `↻` is kept warm by pings, dim `○` is cold. The `a` key, or the pane button, turns the row off and on.
- **Row 3, the weekly tip.** Only when `/context-guru:save-more` or `/context-guru:remove-mcp-and-skills` has not run for 7 days. It uses the same rule as the status line tip (`last-run.json`, and a new install waits a week). It does not rotate.
- **Hide.** `hide` removes the band for the session. `/context-guru:context-guru`, or the pane's **Show the band again** button, brings it back.

### The pane

`/context-guru:context-guru` opens it. Sections that have no data are left out.

```text
◆ Context Guru
open dashboard ↗
saved $22.80 of $81.90 this session

NEXT MESSAGE
if the cache is warm $0.06
if it is cold      $0.78  (90k tokens in the cached prefix)
● cache warm, about 4m left
[ Ping now ]

AGENTS
● Explore               5m       saved $1.20
◐ code-reviewer         <1m
○ planner               cold
Hide agents in the band  a

SAVED LATELY
$4.20 today · last 14 days ▁▂▂▃▄▄▅▅▆▇▇██▅

CACHE HITS
■■■■✖■■▨▨■■■■■■■■■■■
last 20 requests · ■ hit ▨ partial ✖ miss
$1.40 lost today

MISSES BY HOUR
0h   ░▒  █░ ··············· 24h
darker means more misses in that hour
$1.40 lost today

CONTEXT
▒▒▒▒▒▒▒███████░░░░░░░░░░░░░░░░░░░░ 41%
▒ always loaded 20% · █ conversation 21% · ░ free · full in 58m · a miss now costs $1.20

SAVE MORE
tip /context-guru:save-more hasn't run in 7+ days — finds a cheaper config
/context-guru:insights see where the money goes
/context-guru:save-more find a cheaper configuration
/context-guru:remove-mcp-and-skills drop MCP servers and skills you never use
```

The data for the four mode sections comes from `scripts/cg.py panedata`, with the same math the old status line modes used. The pane is the one place that repeats the money, as one summary line.

## <span id="refresh"></span>Refresh and the one-second clock

Claude Code redraws the status line on every message and, if you ask, on a timer. The installer writes `"refreshInterval": 1` next to the command, so the clock ticks every second (the minimum Claude Code accepts). A value you set yourself is kept when the status line is re-installed, so an install made earlier with a longer interval keeps it. To get the one-second clock on an existing install, set it by hand in the settings file that holds the status line:

```json
"statusLine": { "type": "command", "command": "…", "refreshInterval": 1 }
```

and start a new session. Older Claude Code versions ignore the key and redraw only on events. The clock is arithmetic on a timestamp the proxy supplies, so a redraw needs no new network read. A render takes about 55 ms warm and 90 ms on a cold cache. The cost is one short process per second per open session. If that matters, set a larger `refreshInterval` (for example 5); the clock then moves every 5 seconds.

## Render time

Every layout returns in about 0.4 s at worst, Python start-up included (the slowest run in the table took 0.43 s). The render has a 0.36 s budget. Only the session's totals (`/api/stats?lean=1&session=…`) are waited for, and the cache state is read in parallel under the same deadline. A timeout shows the last numbers with `⏳`, never "proxy offline".

Measured on this host, which is loaded and shared with production: 50 runs per cell, wall milliseconds, median / 95th percentile / maximum. "Slow" is a server that sleeps 5 seconds on every request.

| layout | healthy, warm cache | healthy, cold cache | slow proxy | proxy down |
|---|---|---|---|---|
| minimal | 53 / 59 / 64 | 89 / 97 / 97 | 389 / 398 / 399 | 92 / 98 / 106 |
| balanced | 51 / 60 / 66 | 89 / 104 / 133 | 390 / 397 / 401 | 94 / 105 / 110 |
| detailed | 57 / 66 / 70 | 93 / 103 / 110 | 389 / 395 / 398 | 91 / 101 / 104 |

The same harness on the status line before this work:

| layout | healthy, warm cache | healthy, cold cache | slow proxy | proxy down |
|---|---|---|---|---|
| minimal | 42 / 59 / 63 | 80 / 89 / 93 | 682 / 691 / 692 | 76 / 90 / 91 |
| balanced | 44 / 53 / 54 | 80 / 91 / 99 | 679 / 729 / 751 | 75 / 82 / 86 |
| detailed | 45 / 52 / 60 | 79 / 89 / 98 | 678 / 703 / 713 | 83 / 126 / 146 |

The slow-proxy case went from about 680 ms to about 390 ms. Healthy renders are about 10 ms slower than before, from the extra elements (agents, spend).

Per-agent names, the keep-alive plan and the session and subagents money (which removed the lifetime read) cost nothing measurable. Interleaved runs of the previous release ("before") and this one ("after") against the same server, with three named sub-agents on disk, 50 runs each, median / 95th percentile / maximum in milliseconds:

| layout | scenario | before | after |
|---|---|---|---|
| minimal | warm | 50 / 56 / 61 | 50 / 56 / 62 |
| minimal | cold | 79 / 86 / 95 | 80 / 89 / 100 |
| minimal | slow | 386 / 391 / 394 | 388 / 392 / 397 |
| minimal | down | 87 / 96 / 106 | 82 / 94 / 159 |
| balanced | warm | 52 / 56 / 59 | 52 / 56 / 58 |
| balanced | cold | 84 / 102 / 238 | 83 / 100 / 174 |
| balanced | slow | 387 / 393 / 407 | 387 / 396 / 417 |
| balanced | down | 84 / 91 / 94 | 79 / 85 / 94 |
| detailed | warm | 50 / 57 / 62 | 53 / 59 / 60 |
| detailed | cold | 83 / 88 / 90 | 86 / 93 / 95 |
| detailed | slow | 387 / 396 / 425 | 388 / 396 / 413 |
| detailed | down | 88 / 96 / 106 | 84 / 95 / 102 |

Reading the names opens only the files of the agents the proxy lists (at most 24, 4 KB each). With 5,000 agent files in the session the render takes under 0.1 s.

## Colour and plain terminals

Colours are the basic ANSI set plus dim, so they follow your terminal theme on light and dark backgrounds. `NO_COLOR=1` or `TERM=dumb` removes the colour; the clock, bar and symbols still carry the state. The status line has no ASCII mode: its bars and symbols are Unicode. `--layout=minimal` keeps to one short line.

## Turn it on, off, or change it

`/context-guru:statusline` installs it (the install also does this by default), changes the layout, or turns it off with `settings.py off`, which restores whatever status line you had before. It never sends a keep-alive ping; it only reads.
