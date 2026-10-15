---
name: adapt
description: Moved to /context-guru:save-more (config and cache) and /context-guru:remove-mcp-and-skills (unused MCP servers and skills). Kept for one release so existing habits still work.
argument-hint: "[cache|compaction|tools]"
allowed-tools: Bash(python3 "${CLAUDE_PLUGIN_ROOT}/scripts/cg.py" adapt:*)
---

This command moved: `/context-guru:adapt cache` and `compaction` are now `/context-guru:save-more cache` and
`/context-guru:save-more config`; `/context-guru:adapt tools` is now `/context-guru:remove-mcp-and-skills`.
Tell the user that in one line, then run the new command's read and print its output in one code block:

```bash
python3 "${CLAUDE_PLUGIN_ROOT}/scripts/cg.py" adapt $ARGUMENTS
```

Do not apply anything from here; if the user wants to, tell them the new command to run.
