---
name: cg-waste
description: MCP servers, skills and tools you carry but never use, with the command that removes each.
---

Run this from this skill directory, then print its output exactly as it is in one code block. Add nothing before or after it.

```bash
python3 ../../scripts/cg.py waste
```

Append `details` to the command when the user asks for more. Do not turn dollars into tokens.
If loopback access is blocked, retry the same read-only command with `sandbox_permissions="require_escalated"`.
