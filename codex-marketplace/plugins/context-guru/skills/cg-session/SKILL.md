---
name: cg-session
description: One-line timeline of the current session: turns, pauses, misses, pings, compactions.
---

Run this from this skill directory, then print its output exactly as it is in one code block. Add nothing before or after it.

```bash
python3 ../../scripts/cg.py session
```

Append `details` to the command when the user asks for more. Do not turn dollars into tokens.
If loopback access is blocked, retry the same read-only command with `sandbox_permissions="require_escalated"`.
