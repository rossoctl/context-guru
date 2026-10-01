---
name: context-guru-update
description: Check for and install a newer context-guru proxy binary. Use when the user asks to update, upgrade, or check the context-guru version.
---

# Update the proxy binary

Run `python3 ../../scripts/codex_plugin.py update --check` from this skill directory first. This is
read-only and reports the installed and latest versions. If the check fails, say the version is
unknown rather than claiming it is current.

Only after the user explicitly asks to install the update, run
`python3 ../../scripts/codex_plugin.py update --install`. This downloads and checksum-verifies the
release using the shared installer, then safely restarts only the recorded context-guru process.
Report the installer's result verbatim when it refuses or fails.
