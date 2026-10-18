---
name: context-guru-insights
description: Savings, cache and tools in one place; `savings`, `cache` or `tools` (what you carry and never use) for one.
---

Run this from this skill directory, then print its output exactly as it is in one code block. Add nothing before or after it.

```bash
python3 ../../scripts/cg.py insights
```

Append `savings`, `cache` or `tools` when the user asks about one, and `details` when they ask for more.
Do not turn dollars into tokens.
If loopback access is blocked, retry the same read-only command with `sandbox_permissions="require_escalated"`.
If it still cannot run, say proxy state is unverified rather than guessing.
