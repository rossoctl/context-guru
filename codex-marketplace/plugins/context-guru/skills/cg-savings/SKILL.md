---
name: cg-savings
description: How much Context Guru saved, by lever; realised and estimated kept apart.
---

Run this from this skill directory, then print its output exactly as it is in one code block. Add nothing before or after it.

```bash
python3 ../../scripts/cg.py savings
```

Append `details` to the command when the user asks for more. Do not turn dollars into tokens.
If loopback access is blocked, retry the same read-only command with `sandbox_permissions="require_escalated"`.
