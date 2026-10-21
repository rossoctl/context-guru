---
name: context-guru-cache
description: Moved to the insights skill (`cache`). Kept for one release so existing habits still work.
---

This command moved to `context-guru-insights` (`cache`). Say so in one line, then run the new one from this skill directory and print its output exactly as it is in one code block:

```bash
python3 ../../scripts/cg.py insights cache
```

If loopback access is blocked, retry the same read-only command with `sandbox_permissions="require_escalated"`.
If it still cannot run, say proxy state is unverified rather than guessing.
