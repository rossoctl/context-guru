---
name: insights
description: Savings, cache and tools in one place. With no subcommand, all three one after another; `savings` (how much Context Guru saved), `cache` (is the cache about to cost money, recent misses by cause) or `tools` (MCP servers, skills and tools you carry and never use) for one. Use when asked what it saved, about the cache, idle time or misses, or what is wasting money.
argument-hint: "[savings|cache|tools] [details]"
allowed-tools: Bash(python3 "${CLAUDE_PLUGIN_ROOT}/scripts/cg.py" insights:*)
---

Run this, then print its output exactly as it is in one code block. Add nothing before or after it.

```bash
python3 "${CLAUDE_PLUGIN_ROOT}/scripts/cg.py" insights $ARGUMENTS
```

If the output ends with a `More:` line and the user wants the detail, run the same command with the subcommand
they asked about and `details`. Do not turn dollars into tokens or add advice that the output does not contain.
