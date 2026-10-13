---
name: context-guru-cache
description: Is the prompt cache about to cost money: countdown, cost of a miss, recent misses by cause.
---

Run this from this skill directory, then print its output exactly as it is in one code block. Add nothing before or after it.

```bash
python3 ../../scripts/cg.py cache
```

Append `details` to the command when the user asks for more. Do not turn dollars into tokens.
If loopback access is blocked, retry the same read-only command with `sandbox_permissions="require_escalated"`.
If it still cannot run, say proxy state is unverified rather than guessing.
