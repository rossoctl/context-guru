---
name: ping
description: Send one keep-alive ping now so this session's prompt cache stays warm, and say what it saved.
---

Run this, then print its output exactly as it is in one code block. Add nothing before or after it.

```bash
python3 "${CLAUDE_PLUGIN_ROOT}/scripts/cg.py" ping
```

This sends one real (small) request, so Claude Code asks before running it. It pings only this session, and only while its cache is still warm (a cold cache has nothing to keep warm).
Do not turn dollars into tokens or add advice that the output does not contain.
