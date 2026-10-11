---
name: cost-unused
description: Tools, MCP servers and skills this setup carries on every request but never calls, ranked by dollars carried, each with the command that removes it and a caution when it was used recently. Use when the user asks what to remove, which MCP servers or skills are unused, what their tool list costs, or how to shrink the prompt. Accepts --json for the raw document and --days N for the window.
allowed-tools: Bash(${CLAUDE_PLUGIN_ROOT}/scripts/cgcost.py)
---

# cost-unused

```bash
"${CLAUDE_PLUGIN_ROOT}/scripts/cgcost.py" unused
```

Add `--days N` to change the window, or `--json` and print the output verbatim with no narration.
The script resolves the proxy port itself and only reads (GETs). It applies nothing: every command
it prints is for the user to run, so offer it and run one only when asked.

Print the report as is: it is already ranked, priced at the gateway's rates and labelled
`observed` (billed tokens), `modelled` (a counterfactual the proxy computed) or `estimate`
(arithmetic with printed assumptions). Do not add, round up or merge figures, and do not turn an
`estimate` into a promise. If it says the dashboard route is unavailable, the proxy is not running
with `--dashboard` or is an older build; say that and stop.

Never suggest removing Context Guru's own components (the script already leaves them out). For anything marked `used Nd ago - check first`, ask before offering the command. Removal takes effect in the next session.
