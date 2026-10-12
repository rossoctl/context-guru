---
name: cg-home
description: Context Guru home: what you saved, today against yesterday, top three things to do next.
---

Run this from this skill directory, then print its output exactly as it is in one code block. Add nothing before or after it.

```bash
python3 ../../scripts/cg.py home
```

Append `details` to the command when the user asks for more. Do not turn dollars into tokens.
If loopback access is blocked, retry the same read-only command with `sandbox_permissions="require_escalated"`.
