---
name: context-guru-uninstall
description: Stop sending IBM Bob's traffic through context-guru - remove the startup-file block, the hooks and the proxy service. Use when asked to uninstall, remove, disable or turn off context-guru for Bob.
---

# Remove context-guru from Bob

1. Run this command. It writes nothing:

   ```sh
   python3 "$HOME/.bob/plugins/context-guru/scripts/bob_plugin.py" uninstall --dry-run
   ```

2. Show the user what it will remove. Ask one yes/no question: remove it now? Also ask if they
   want the dashboard data deleted too. The default is to keep it.
3. After a clear yes, run one of these:

   ```sh
   python3 "$HOME/.bob/plugins/context-guru/scripts/bob_plugin.py" uninstall
   python3 "$HOME/.bob/plugins/context-guru/scripts/bob_plugin.py" uninstall --purge
   ```

   `--purge` also deletes the state folder (dashboard data, logs, the reset script).

4. Report each line: `hooks=`, `rc_block=`, `service=`. A `left_alone` value means the file has a
   change that is not ours. Show the detail line, and tell the user to remove that part by hand.
5. Always end with: **quit and reopen Bob, then log in once**. Bob keeps its login per backend
   address.

The current Bob window keeps the old address until Bob restarts. After the service stops, Bob
cannot reach its backend from this window. So tell the user to quit Bob right after this command.

If Bob cannot answer at all, the user runs this in a terminal instead. It needs no Python:

```sh
~/.local/state/context-guru-bob/context-guru-reset --yes
```
