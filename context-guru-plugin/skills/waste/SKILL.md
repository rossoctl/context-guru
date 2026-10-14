---
name: waste
description: MCP servers, skills and tools you carry but never use, what they cost, and the command that removes each. Use when asked what is wasting money or tokens, or what to remove.
argument-hint: "[details]"
allowed-tools: Bash(python3 ${CLAUDE_PLUGIN_ROOT}/scripts/cg.py:*)
---

Run this, then print its output exactly as it is in one code block. Add nothing before or after it.

```bash
python3 "${CLAUDE_PLUGIN_ROOT}/scripts/cg.py" waste $ARGUMENTS
```

If the output ends with a `More:` line and the user wants the detail, run the same command with `details`.
Do not turn dollars into tokens or add advice that the output does not contain.
