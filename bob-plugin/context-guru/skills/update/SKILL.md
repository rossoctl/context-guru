---
name: context-guru-update
description: Check for and install a newer context-guru proxy binary for IBM Bob. Use when asked to update, upgrade or check the version of context-guru.
---

# Update the proxy binary

Bob does not check for updates by itself. Tell the user to run this skill now and then.

1. Run this command. It changes nothing, and it makes one network call to GitHub to find the latest
   release:

   ```sh
   python3 "$HOME/.bob/plugins/context-guru/scripts/bob_plugin.py" update --check
   ```

   Report `installed_version=` and `latest_version=`, and `note=` when there is one. If the check
   fails, say the version is unknown. Do not say it is current. If the installed version is newer
   than the latest release, do not offer a downgrade unless the user asks; then add
   `--allow-downgrade`.

2. If `update_available=true`, ask one yes/no question: install it now? After a clear yes, run:

   ```sh
   python3 "$HOME/.bob/plugins/context-guru/scripts/bob_plugin.py" update
   ```

3. Report the result:
   - `result=updated`: the new proxy runs.
   - `result=update_failed` with `rolled_back=true`: the new proxy did not start, so the old one
     runs again. Nothing is broken.
   - `result=update_failed` with `rolled_back=false`: show `fix=`.
   - `result=refused` with `reason=binary_not_managed`: the proxy came from somewhere else (for
     example brew). Update it that way.

The update restarts the proxy. A Bob request in flight at that moment can fail; the user can send it
again. The download is checked against the release checksums before it is used.
