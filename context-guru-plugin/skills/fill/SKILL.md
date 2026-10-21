---
name: fill
description: How full the context window is, what is filling it, and how long until it is full.
allowed-tools: Bash(python3 "${CLAUDE_PLUGIN_ROOT}/scripts/tv.py" fill:*)
---

Run this, then print its output exactly as it is in one code block. Add nothing before or after it.

```bash
python3 "${CLAUDE_PLUGIN_ROOT}/scripts/tv.py" fill
```
Do not turn dollars into tokens or add advice that the output does not contain.
