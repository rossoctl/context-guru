---
name: timeline
description: A timeline of this session: turns, pauses, cache misses with what each cost, pings and compactions, plus the three most expensive moments.
allowed-tools: Bash(python3 ${CLAUDE_PLUGIN_ROOT}/scripts/tv.py:*)
---

Run this, then print its output exactly as it is in one code block. Add nothing before or after it.

```bash
python3 "${CLAUDE_PLUGIN_ROOT}/scripts/tv.py" timeline
```
Do not turn dollars into tokens or add advice that the output does not contain.
