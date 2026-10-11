---
name: cost-today
description: What cost the most today and why: spend by token type, model, project and session, with the top three causes in plain words and Context Guru's honest net (gross saving minus its own overhead). Use when the user asks what cost them the most, where today's money went, why a session was expensive, or what their spend is. Accepts --json for the raw document and --days N for the window.
allowed-tools: Bash(${CLAUDE_PLUGIN_ROOT}/scripts/cgcost.py)
---

# cost-today

```bash
"${CLAUDE_PLUGIN_ROOT}/scripts/cgcost.py" today
```

Add `--days N` to change the window, or `--json` and print the output verbatim with no narration.
The script resolves the proxy port itself and only reads (GETs). It applies nothing: every command
it prints is for the user to run, so offer it and run one only when asked.

Print the report as is: it is already ranked, priced at the gateway's rates and labelled
`observed` (billed tokens), `modelled` (a counterfactual the proxy computed) or `estimate`
(arithmetic with printed assumptions). Do not add, round up or merge figures, and do not turn an
`estimate` into a promise. If it says the dashboard route is unavailable, the proxy is not running
with `--dashboard` or is an older build; say that and stop.

After the report, add at most two sentences: the single biggest cause and the one action that addresses it (the matching command is `cost-cold`, `cost-compact` or `cost-unused`). Cache reads are the normal cost of a long conversation; do not describe them as waste.
