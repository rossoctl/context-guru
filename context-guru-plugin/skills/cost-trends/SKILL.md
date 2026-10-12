---
name: cost-trends
description: Per-project and per-model spend trends with sparklines, and recommendations ranked by dollars. Use when the user asks how spend is trending, which project or model costs the most over time, or what to do first to save money. Accepts --json for the raw document and --days N for the window.
allowed-tools: Bash(${CLAUDE_PLUGIN_ROOT}/scripts/cgcost.py)
---

# cost-trends

```bash
"${CLAUDE_PLUGIN_ROOT}/scripts/cgcost.py" trends
```

Add `--days N` to change the window, or `--json` and print the output verbatim with no narration.
The script resolves the proxy port itself and only reads (GETs). It applies nothing: every command
it prints is for the user to run, so offer it and run one only when asked.

Print the report as is: it is already ranked, priced at the gateway's rates and labelled
`observed` (billed tokens), `modelled` (a counterfactual the proxy computed) or `estimate`
(arithmetic with printed assumptions). Do not add, round up or merge figures, and do not turn an
`estimate` into a promise. If it says the dashboard route is unavailable, the proxy is not running
with `--dashboard` or is an older build; say that and stop.

Offer the first one or two recommendations only, with the command or skill that acts on each.
