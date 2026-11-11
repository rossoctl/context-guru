---
name: context-guru-update
description: Check for and install a newer context-guru proxy binary. Use when the user asks to update, upgrade, or check the context-guru version.
---

# Update the proxy binary

Codex does not run this check automatically. Tell the user to invoke `$context-guru-update`
periodically if they want to stay current; do not claim there is a background update check.

Run `python3 ../../scripts/codex_plugin.py update --check` from this skill directory first. This is
read-only and reports the installed and latest versions. If the check fails, say the version is
unknown rather than claiming it is current. If it fails because the command sandbox blocks the
release-network request, retry the same read-only check with
`sandbox_permissions="require_escalated"` and explain why.

Do not install the update from inside Codex. If an update is available, tell the user to finish or
exit the current session and run this directly in an ordinary shell:

```sh
~/.local/state/context-guru-codex/context-guru-update
```

The standalone command downloads and checksum-verifies the release, then restarts context-guru's
owned user service. Starting a new Codex session afterward avoids coupling a session to the proxy
restart that updates its own transport.
