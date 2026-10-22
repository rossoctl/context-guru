---
name: watch
description: A live terminal view of every cache's countdown, what you saved today, and the last 20 requests. One frame here; the live version runs in a second terminal.
allowed-tools: Bash(python3 "${CLAUDE_PLUGIN_ROOT}/scripts/tv.py" watch:*)
---

Run this, then print its output exactly as it is in one code block. Add nothing before or after it.

```bash
python3 "${CLAUDE_PLUGIN_ROOT}/scripts/tv.py" watch --once --hint
```

If the output ends with a `Live:` line, keep it: the live view needs a real terminal, so the user runs that command in a second terminal.
Do not turn dollars into tokens or add advice that the output does not contain.
