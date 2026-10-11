---
name: cost-cold
description: Where the prompt cache went cold and what it cost: full-prefix rewrites classified from token counts, by cause, with how long the user was away before each expiry. Use when the user asks why the cache missed, what idle time costs, what a rewrite is, or why a session got expensive after a break. Accepts --json for the raw document and --days N for the window.
allowed-tools: Bash(${CLAUDE_PLUGIN_ROOT}/scripts/cgcost.py)
---

# cost-cold

```bash
"${CLAUDE_PLUGIN_ROOT}/scripts/cgcost.py" cold
```

Add `--days N` to change the window, or `--json` and print the output verbatim with no narration.
The script resolves the proxy port itself and only reads (GETs). It applies nothing: every command
it prints is for the user to run, so offer it and run one only when asked.

Print the report as is: it is already ranked, priced at the gateway's rates and labelled
`observed` (billed tokens), `modelled` (a counterfactual the proxy computed) or `estimate`
(arithmetic with printed assumptions). Do not add, round up or merge figures, and do not turn an
`estimate` into a promise. If it says the dashboard route is unavailable, the proxy is not running
with `--dashboard` or is an older build; say that and stop.

State that the cause label is inferred from the pause length and message count, while the dollars are observed. Do not recommend keep-alive pings from this report; point to `insights-idle` for the net of pings.
