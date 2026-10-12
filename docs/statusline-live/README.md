# Status line: live cache clock and sub-agents

The status line shows how long the prompt cache stays warm, as a clock that ticks every second, and shows the sub-agent that will lose its cache first (each sub-agent has its own cache). All renders below are real output from a test proxy with synthetic data, at 80 columns unless stated. The `.cast` files replay in `asciinema play`; `live-countdown.cast` is a real 50 second run, one render per second.

| Clock | Rule |
|---|---|
| green `●` | more than 60 s left (a fifth of the lifetime on the 1 h tier) |
| amber `◐` | 60 s to 15 s left; the hint prices what a pause would cost |
| red `◐ 0:09!` | last 15 s |
| red `○ cold 1:15` | expired; counts up. Time is measured from the end of the last response |

The bar drains over the cache's own lifetime (5 minutes, or 1 hour on the 1 h tier). Switch layouts without editing settings: `statusline.py --set-layout=minimal|balanced|detailed|auto`.

### Warm: green, seconds ticking down

![Warm: green, seconds ticking down](warm-dark.svg#only-dark)
![Warm: green, seconds ticking down](warm-light.svg#only-light)

### Warm on the 1 h cache tier (the bar rescales to the tier's lifetime)

![Warm on the 1 h cache tier (the bar rescales to the tier's lifetime)](warm-1h-tier-dark.svg#only-dark)
![Warm on the 1 h cache tier (the bar rescales to the tier's lifetime)](warm-1h-tier-light.svg#only-light)

### Expiring: amber, with what a pause would cost

![Expiring: amber, with what a pause would cost](expiring-dark.svg#only-dark)
![Expiring: amber, with what a pause would cost](expiring-light.svg#only-light)

### Critical: red, last 15 seconds, marked with `!`

![Critical: red, last 15 seconds, marked with `!`](critical-dark.svg#only-dark)
![Critical: red, last 15 seconds, marked with `!`](critical-light.svg#only-light)

### Cold: counts up how long it has been cold

![Cold: counts up how long it has been cold](cold-dark.svg#only-dark)
![Cold: counts up how long it has been cold](cold-light.svg#only-light)

### Keep-alive ping in place: `↻`

![Keep-alive ping in place: `↻`](ping-planned-dark.svg#only-dark)
![Keep-alive ping in place: `↻`](ping-planned-light.svg#only-light)

### One running sub-agent

![One running sub-agent](agents-1-dark.svg#only-dark)
![One running sub-agent](agents-1-light.svg#only-light)

### Several sub-agents: the count and the one expiring soonest

![Several sub-agents: the count and the one expiring soonest](agents-2-dark.svg#only-dark)
![Several sub-agents: the count and the one expiring soonest](agents-2-light.svg#only-light)

### Five sub-agents

![Five sub-agents](agents-5-dark.svg#only-dark)
![Five sub-agents](agents-5-light.svg#only-light)

### Worst case at 80 columns: cold, five sub-agents, a hint

![Worst case at 80 columns: cold, five sub-agents, a hint](worst-case-80-dark.svg#only-dark)
![Worst case at 80 columns: cold, five sub-agents, a hint](worst-case-80-light.svg#only-light)

### Narrow terminal (40 columns)

![Narrow terminal (40 columns)](narrow-40-dark.svg#only-dark)
![Narrow terminal (40 columns)](narrow-40-light.svg#only-light)

### Model changed while the cache is warm

![Model changed while the cache is warm](model-switch-dark.svg#only-dark)
![Model changed while the cache is warm](model-switch-light.svg#only-light)

### Tool set changed (from Claude Code's own miss report)

![Tool set changed (from Claude Code's own miss report)](tool-set-changed-dark.svg#only-dark)
![Tool set changed (from Claude Code's own miss report)](tool-set-changed-light.svg#only-light)

### Detailed layout (120 columns)

![Detailed layout (120 columns)](detailed-dark.svg#only-dark)
![Detailed layout (120 columns)](detailed-light.svg#only-light)

### Proxy offline

![Proxy offline](proxy-offline-dark.svg#only-dark)
![Proxy offline](proxy-offline-light.svg#only-light)
