---
name: cache
description: Is the prompt cache about to cost money: a countdown to expiry, what a miss would cost, recent misses by cause. Use when asked about the cache, TTL, idle time, misses, or why a turn was expensive.
argument-hint: "[details]"
allowed-tools: Bash(python3 "${CLAUDE_PLUGIN_ROOT}/scripts/cg.py" cache:*)
---

Run this, then print its output exactly as it is in one code block. Add nothing before or after it.

```bash
python3 "${CLAUDE_PLUGIN_ROOT}/scripts/cg.py" cache $ARGUMENTS
```

If the output ends with a `More:` line and the user wants the detail, run the same command with `details`.
Do not turn dollars into tokens or add advice that the output does not contain.
