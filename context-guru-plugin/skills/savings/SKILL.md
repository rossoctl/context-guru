---
name: savings
description: How much context-guru saved: by lever, with a trend; saved dollars and estimated opportunity kept separate. Use when asked how much was saved, what the proxy is worth, or for a cost report.
argument-hint: "[details]"
allowed-tools: Bash(python3 ${CLAUDE_PLUGIN_ROOT}/scripts/cg.py:*)
---

Run this, then print its output exactly as it is in one code block. Add nothing before or after it.

```bash
python3 "${CLAUDE_PLUGIN_ROOT}/scripts/cg.py" savings $ARGUMENTS
```

If the output ends with a `More:` line and the user wants the detail, run the same command with `details`.
Do not turn dollars into tokens or add advice that the output does not contain.
