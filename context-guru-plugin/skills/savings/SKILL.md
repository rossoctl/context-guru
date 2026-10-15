---
name: savings
description: Moved to /context-guru:insights savings (how much Context Guru saved). Kept for one release so existing habits still work.
argument-hint: "[details]"
allowed-tools: Bash(python3 "${CLAUDE_PLUGIN_ROOT}/scripts/cg.py" insights:*)
---

This command moved to `/context-guru:insights savings`. Tell the user that in one line, then run the new command:

```bash
python3 "${CLAUDE_PLUGIN_ROOT}/scripts/cg.py" insights savings $ARGUMENTS
```

Print its output exactly as it is in one code block.
