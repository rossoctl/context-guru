---
name: context-guru-adapt
description: Moved to $context-guru-save-more (config and cache) and $context-guru-remove-mcp-and-skills (unused MCP servers and skills). Kept for one release.
---

Tell the user in one line that this moved to `$context-guru-save-more` and `$context-guru-remove-mcp-and-skills`.
Then run this from this skill directory and print its output in one code block (read only; apply nothing here):

```bash
python3 ../../scripts/cg.py adapt
```

Append `cache`, `compaction` or `tools` if the user named one.
