---
name: context-guru-uninstall
description: Restore Codex's previous default provider and stop the context-guru proxy. Use when asked to disable, remove, or uninstall it.
---

# Remove context-guru from Codex

Run `python3 ../../scripts/codex_plugin.py uninstall --dry-run` from this skill directory and show
what it found. Do not run the mutating uninstall command from inside Codex. A routed Codex session
cannot change transport after it starts, so stopping its proxy permanently strands that session's
remaining turns.

Tell the user to exit Codex, then run this directly in an ordinary shell:

```sh
~/.local/state/context-guru-codex/context-guru-reset --yes
```

It removes only the marked provider block, restores the previous default provider unless the user
changed it since installation, and stops only its owned user service (with guarded
recorded-process cleanup for older installations). The next ordinary Codex session uses the
restored provider.
