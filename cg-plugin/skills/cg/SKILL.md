---
name: cg
description: Context Guru home: what you saved, today against yesterday, and the top three things to do next. Use when the user asks how Context Guru is doing or what to do.
argument-hint: "[details]"
allowed-tools: Bash(python3 ${CLAUDE_PLUGIN_ROOT}/scripts/cg.py:*)
---

Run this, then print its output exactly as it is in one code block. Add nothing before or after it.

```bash
python3 "${CLAUDE_PLUGIN_ROOT}/scripts/cg.py" home $ARGUMENTS
```

If the output ends with a `More:` line and the user wants the detail, run the same command with `details`.
Do not turn dollars into tokens or add advice that the output does not contain.
