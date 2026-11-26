---
name: context-guru-doctor
description: Check every part of context-guru for IBM Bob and say how to fix what is wrong - proxy, service, startup-file block, hooks, binary, and whether this Bob release still has what context-guru needs. Use when Bob or context-guru seems broken, after a Bob update, or when asked for a health check.
---

# Check context-guru for Bob

Run this command. It only reads context-guru's files and asks the proxy and launchd for status:

```sh
python3 "$HOME/.bob/plugins/context-guru/scripts/bob_plugin.py" doctor
```

It prints `rc_probe=skipped`. To also check that Bob's login shell sees the address, ask the user
first, then add `--probe`. The probe starts the user's login shell, which runs their shell startup
files. Those can do anything they are written to do (for example update a shell framework or use
the network), so say this when you ask.

Each check prints `name=ok`, `name=warn` or `name=fail`. A `fix=` line follows each one that is not
ok. Report:

1. The last line, `result=`.
2. Each `fail`, then each `warn`, with its `fix=` in plain words.
3. `bob_version=`. If `bob_bundle=fail`, say plainly: this Bob release changed something
   context-guru needs. The user can run the reset script in the fix line, so Bob talks to IBM
   directly, and tell the context-guru team the Bob version.

Do not run a fix yourself. Ask the user first, and run each fix only after a clear yes.
