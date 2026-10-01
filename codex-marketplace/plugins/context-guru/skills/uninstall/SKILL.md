---
name: context-guru-uninstall
description: Remove the context-guru Codex profile and stop its proxy. Use when asked to disable, remove, or uninstall it.
---

# Remove context-guru from Codex

Run `python3 ../../scripts/codex_plugin.py uninstall --dry-run` from this skill directory and show
what it found. Ask for confirmation, then rerun without `--dry-run`. The script removes only a
profile carrying its ownership marker and only stops its recorded process.
