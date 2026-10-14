---
name: remove-mcp-and-skills
description: Find the MCP servers, MCP tools and skills you carry but never use (never built-in tools), show what carrying them costs and what earlier removals have measurably saved, and filter them after one yes. Use when asked what to remove, which MCP servers or skills are unused, or to undo a removal.
argument-hint: "[details] [--days N]  |  apply tools <option>@<basis>  |  revert tools"
allowed-tools: Bash(python3 "${CLAUDE_PLUGIN_ROOT}/scripts/cg.py" remove-mcp-and-skills:*)
---

Only the read is pre-approved. A change is the separate command `cg.py apply` or `cg.py revert`, so Claude Code
also asks the user for permission to run it: that second prompt is expected, do not work around it.

**If the arguments start with `apply` or `revert`** (the user typed the change themselves, which is their yes),
run this and print its output in one code block:

```bash
python3 "${CLAUDE_PLUGIN_ROOT}/scripts/cg.py" $ARGUMENTS
```

**Otherwise,** run this with a Bash timeout of 420000 ms and print its output exactly as it is in one code block:

```bash
python3 "${CLAUDE_PLUGIN_ROOT}/scripts/cg.py" remove-mcp-and-skills $ARGUMENTS
```

If the output has no `Question:` line, stop there. Add nothing.

Otherwise, after the output is printed, ask exactly the `Question:` line (without the word "Question:") with
AskUserQuestion. Options: "Yes, remove them" (description: the item names from the `Never used` list that the
recommendation covers) and "No, keep them".

Only on yes run `python3 "${CLAUDE_PLUGIN_ROOT}/scripts/cg.py"` followed by every word after
`/context-guru:remove-mcp-and-skills` on the `Apply it?` line, unchanged, and print its output. On anything else
say "Nothing changed." and stop. If the apply output starts with `Not applied:`, print it and stop.
To undo, run the `Undo:` line the same way. Do not turn dollars into tokens or add advice the output does not contain.
