---
name: cost-cold
description: Moved to /context-guru:cache (where the cache went cold). Kept for one release so existing habits still work.
argument-hint: "[details]"
allowed-tools: Bash(python3 ${CLAUDE_PLUGIN_ROOT}/scripts/cg.py cache:*)
---

This command moved to `/context-guru:cache`. Tell the user that in one line, then run the new command:

```bash
python3 "${CLAUDE_PLUGIN_ROOT}/scripts/cg.py" cache $ARGUMENTS
```

Print its output exactly as it is in one code block.
