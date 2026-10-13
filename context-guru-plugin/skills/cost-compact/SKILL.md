---
name: cost-compact
description: Moved to /context-guru:cache (whether a compact is worth it). Kept for one release so existing habits still work.
argument-hint: "[details]"
allowed-tools: Bash(python3 ${CLAUDE_PLUGIN_ROOT}/scripts/cg.py:*)
---

This command moved to `/context-guru:cache`. Tell the user that in one line, then run the new command:

```bash
python3 "${CLAUDE_PLUGIN_ROOT}/scripts/cg.py" cache $ARGUMENTS
```

Print its output exactly as it is in one code block.
