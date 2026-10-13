---
name: insights
description: Moved to /context-guru:status (the cost report is now the home screen). Kept for one release so existing habits still work.
argument-hint: "[details]"
allowed-tools: Bash(python3 ${CLAUDE_PLUGIN_ROOT}/scripts/cg.py home:*)
---

This command moved to `/context-guru:status`. Tell the user that in one line, then run the new command:

```bash
python3 "${CLAUDE_PLUGIN_ROOT}/scripts/cg.py" home $ARGUMENTS
```

Print its output exactly as it is in one code block.
