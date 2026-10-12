# Status line: live cache clock and sub-agents

The status line shows how long the prompt cache stays warm, as a clock that ticks every second, and does the same for each running sub-agent (each has its own cache). All renders below are real output from a test proxy with synthetic data. The `.cast` files in this folder replay them in `asciinema play`; `live-countdown.cast` is a real 50 second run, one render per second.

| Clock | Rule |
|---|---|
| green `●` | more than 60 s left (a fifth of the lifetime on the 1 h tier) |
| amber `◐` | 60 s to 15 s left; a hint prices what a pause would cost |
| red `◐ 0:09!` | last 15 s |
| red `○ cold 1:20` | expired; counts up. Time is measured from the end of the last response |

Switch layouts without editing settings: `statusline.py --set-layout=minimal|balanced|detailed|auto`.

### Warm: green, seconds ticking down

![Warm: green, seconds ticking down](warm-dark.svg#only-dark)
![Warm: green, seconds ticking down](warm-light.svg#only-light)

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

### Two sub-agents, each with its own clock

![Two sub-agents, each with its own clock](agents-2-dark.svg#only-dark)
![Two sub-agents, each with its own clock](agents-2-light.svg#only-light)

### Many sub-agents collapse to a count and the soonest expiry

![Many sub-agents collapse to a count and the soonest expiry](agents-5-dark.svg#only-dark)
![Many sub-agents collapse to a count and the soonest expiry](agents-5-light.svg#only-light)

### Narrow terminal (40 columns)

![Narrow terminal (40 columns)](narrow-40-dark.svg#only-dark)
![Narrow terminal (40 columns)](narrow-40-light.svg#only-light)

### Model switched while the cache is warm

![Model switched while the cache is warm](model-switch-dark.svg#only-dark)
![Model switched while the cache is warm](model-switch-light.svg#only-light)

### Tool set changed (from Claude Code's own miss report)

![Tool set changed (from Claude Code's own miss report)](tool-set-changed-dark.svg#only-dark)
![Tool set changed (from Claude Code's own miss report)](tool-set-changed-light.svg#only-light)

### Detailed layout

![Detailed layout](detailed-dark.svg#only-dark)
![Detailed layout](detailed-light.svg#only-light)

### Proxy offline

![Proxy offline](proxy-offline-dark.svg#only-dark)
![Proxy offline](proxy-offline-light.svg#only-light)
