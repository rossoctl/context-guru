# Status line

The status line sits under the prompt and answers two questions all the time: **how much am I saving, and is my cache about to cost me?** It shows nothing in a project that is not routed through the proxy.

![Warm: green, seconds ticking down](../statusline-live/warm-dark.svg#only-dark)
![Warm: green, seconds ticking down](../statusline-live/warm-light.svg#only-light)

The first line is money, the second is the cache clock, the sub-agents, the context bar and at most one hint:

```text
saved $0.31 session · $4.20 today · ★ $212.40 lifetime
cache ● █████░ 3:57 │ agents 2 · soonest ◐ 0:51 │ ctx ███░░░░░ 41%
```

## What each piece means

| Piece | Meaning |
|---|---|
| `saved $0.31 session` | Saved this session: modelled against the same requests without the proxy, at your gateway's prices, net of the proxy's own spend. |
| `$4.20 today` | The same measure since local midnight. |
| `★ $212.40 lifetime` | The same measure over everything this proxy has recorded. |
| `cache ● █████░ 3:57` | A bar that drains as the prompt cache approaches expiry, and the clock. Time is measured from the end of the last response. |
| `↻` after the clock | The last touch of the cache was a keep-alive ping. |
| `agents 2 · soonest ◐ 0:51` | Running sub-agents: the count and the clock of the one whose cache expires first (a single agent shows its own clock, as `agent ● 4:21`). Each has its own cache. Finished (cold) ones are not shown. |
| `ctx ███░░░░░ 41%` | How full you are, as a share of the model's context limit. |
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

### Sub-agents

One running sub-agent, then several (the count and the one expiring soonest), then five.

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

At 40 columns the line sheds, in order: tools, latency, spend, the update marker, the long hint form, then the agents shrink to a count, then the `saved` label and the context bar. The cache clock is shortened last.

![40 columns](../statusline-live/narrow-40-dark.svg#only-dark)
![40 columns](../statusline-live/narrow-40-light.svg#only-light)

## Layouts

| Layout | Shows |
|---|---|
| `minimal` | one line: saved, cache, hint |
| `balanced` (default) | update, saved, cache, agents, context, hint |
| `detailed` | balanced plus `spent $4.10 · $2.73/h · up 1h30m` (the rate appears after 5 minutes), a slow-upstream note (only when upstream averages 3 s or more) and the least-used tool or skill |

![Detailed layout](../statusline-live/detailed-dark.svg#only-dark)
![Detailed layout](../statusline-live/detailed-light.svg#only-light)

Switch without editing settings, and keep the choice across renders:

```sh
python3 "<plugin dir>/scripts/statusline.py" --set-layout=detailed   # or minimal, balanced, auto
```

`auto` forgets the choice. A remembered layout wins over the flag in the installed command. To change the installed command instead, re-run `/context-guru:statusline` with `--layout=minimal|balanced|detailed`. Hide or show single elements with `--hide=a,b` and `--show=a,b` (`update save spend cache agents ctx hint latency tools ka`), or the environment variables `CG_STATUSLINE_LAYOUT`, `CG_STATUSLINE_HIDE`, `CG_STATUSLINE_SHOW` and `CG_STATUSLINE_COLUMNS`.

## <span id="refresh"></span>Refresh and the one-second clock

Claude Code redraws the status line on every message and, if you ask, on a timer. The installer writes `"refreshInterval": 1` next to the command, so the clock ticks every second (the minimum Claude Code accepts). A value you set yourself is kept when the status line is re-installed, so an install made earlier with a longer interval keeps it. To get the one-second clock on an existing install, set it by hand in the settings file that holds the status line:

```json
"statusLine": { "type": "command", "command": "…", "refreshInterval": 1 }
```

and start a new session. Older Claude Code versions ignore the key and redraw only on events. The clock is arithmetic on a timestamp the proxy supplies, so a redraw needs no new network read: each render reads a disk-cached copy of the proxy's answer (2 s for cache state, 60 s for the day total, 5 minutes for the lifetime total). The cost is one short process per second per open session, about 70 ms of CPU each. If that matters, set a larger `refreshInterval` (for example 5); the clock then moves every 5 seconds.

## Colour and plain terminals

Colours are the basic ANSI set plus dim, so they follow your terminal theme on light and dark backgrounds. `NO_COLOR=1` or `TERM=dumb` removes the colour; the clock, bar and symbols still carry the state. There is no separate ASCII mode: the bars and symbols are Unicode. `--layout=minimal` keeps to one short line.

## Turn it on, off, or change it

`/context-guru:statusline` installs it (the install also does this by default), changes the layout, or turns it off with `settings.py off`, which restores whatever status line you had before. It never sends a keep-alive ping; it only reads.
