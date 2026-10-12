---
name: doctor
description: Check the Context Guru setup (routing, proxy, prices, status line) and say what to fix.
allowed-tools: Bash(python3 ${CLAUDE_PLUGIN_ROOT}/scripts/cg.py:*)
---

Run this, then print its output exactly as it is in one code block. Add nothing before or after it.

```bash
python3 "${CLAUDE_PLUGIN_ROOT}/scripts/cg.py" doctor $ARGUMENTS
```

If the output ends with a `More:` line and the user wants the detail, run the same command with `details`.
Do not turn dollars into tokens or add advice that the output does not contain.
