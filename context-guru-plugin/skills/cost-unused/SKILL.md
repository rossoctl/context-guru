---
name: cost-unused
description: Moved to /context-guru:waste (unused tools and servers). Kept for one release so existing habits still work.
argument-hint: "[details]"
allowed-tools: Bash(python3 "${CLAUDE_PLUGIN_ROOT}/scripts/cg.py" waste:*)
---

This command moved to `/context-guru:waste`. Tell the user that in one line, then run the new command:

```bash
python3 "${CLAUDE_PLUGIN_ROOT}/scripts/cg.py" waste $ARGUMENTS
```

Print its output exactly as it is in one code block.
