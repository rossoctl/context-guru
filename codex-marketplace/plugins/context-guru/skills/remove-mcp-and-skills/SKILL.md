---
name: context-guru-remove-mcp-and-skills
description: Find the MCP servers, MCP tools and skills you carry but never use (never built-in tools), show their cost and what earlier removals saved, and filter them after one yes.
---

Run this from this skill directory and print its output exactly as it is in one code block:

```bash
python3 ../../scripts/cg.py remove-mcp-and-skills
```

Append `details` when the user asks for more. In the output, read `/context-guru:remove-mcp-and-skills` as this skill.

If the output has no `Question:` line, stop. Otherwise, after printing it, ask exactly that question (yes/no) and
wait. Only on yes, run `python3 ../../scripts/cg.py` followed by every word after
`/context-guru:remove-mcp-and-skills` on the `Apply it?` line, unchanged, with
`sandbox_permissions="require_escalated"` (it changes the proxy's configuration), and print its output. On
anything else say "Nothing changed." and stop. If the user typed `apply tools <token>` or `revert tools`
themselves, that is their yes: run `python3 ../../scripts/cg.py apply tools <token>` (or `revert tools`) the
same way. To undo, run the `Undo:` line the same way.

If loopback access is blocked, retry the read-only command with `sandbox_permissions="require_escalated"`.
If it still cannot run, say proxy state is unverified rather than guessing.
