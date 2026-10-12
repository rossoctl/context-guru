---
name: cost-compact
description: Whether to compact now: each recent session's context size, cache state and time left on the cache, the cost of the next turn if it comes back warm or cold, and the break-even chance of being away. Use when the user asks whether to compact, whether a rewrite is coming, or whether the cache is still warm. Accepts --json for the raw document and --days N for the window.
allowed-tools: Bash(${CLAUDE_PLUGIN_ROOT}/scripts/cgcost.py)
---

# cost-compact

```bash
"${CLAUDE_PLUGIN_ROOT}/scripts/cgcost.py" compact
```

Add `--days N` to change the window, or `--json` and print the output verbatim with no narration.
The script resolves the proxy port itself and only reads (GETs). It applies nothing: every command
it prints is for the user to run, so offer it and run one only when asked.

Print the report as is: it is already ranked, priced at the gateway's rates and labelled
`observed` (billed tokens), `modelled` (a counterfactual the proxy computed) or `estimate`
(arithmetic with printed assumptions). Do not add, round up or merge figures, and do not turn an
`estimate` into a promise. If it says the dashboard route is unavailable, the proxy is not running
with `--dashboard` or is an older build; say that and stop.

Quote the break-even sentence as is. If the cache is already cold, say that compacting does not avoid the rewrite on the next turn.
