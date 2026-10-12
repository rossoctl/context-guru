---
name: heatmap
description: A weekday-by-hour heat map of what cache misses cost you, so you can see when your pauses are expensive.
allowed-tools: Bash(python3 ${CLAUDE_PLUGIN_ROOT}/scripts/tv.py:*)
---

Run this, then print its output exactly as it is in one code block. Add nothing before or after it.

```bash
python3 "${CLAUDE_PLUGIN_ROOT}/scripts/tv.py" heatmap
```
Do not turn dollars into tokens or add advice that the output does not contain.
