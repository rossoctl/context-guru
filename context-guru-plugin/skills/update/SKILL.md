---
name: update
description: Check whether a newer context-guru-proxy BINARY has been released and, if the user wants it, install it. Use when the user asks to update, upgrade, or check the version of context-guru or its proxy, or after a session-start note said a newer proxy is available. The plugin itself updates through the marketplace; this updates the proxy binary, which is released separately and does not move when the plugin does.
---

# Update the context-guru proxy binary

<!-- Deliberately NO `allowed-tools:` here, for the same reason install/SKILL.md has none:
     replacing the binary that carries every one of the user's model requests is at least as
     consequential as the traffic-interception command that skill gates, and this file must not
     grant itself the permission the classifier exists to ask about. -->

!`"${CLAUDE_PLUGIN_ROOT}/scripts/settings.py" update-check show`

**The block above is a pure file read** — installed-vs-latest, and whatever the user already
answered. It fetches nothing from the network: `answer=`/`skipped=`/`latest=` are only ever as
fresh as the last SessionStart check, so treat `latest=` as "as of then," not "right now."

This skill does not touch routing, does not start a proxy, and does not ask for the
traffic-interception consent `/context-guru:install` asks for — it only replaces the binary on
`PATH` and, when you do that, restarts the proxy onto it. If the binary is missing entirely (not
merely old), say so and point at `/context-guru:install` instead; this skill has nothing to install
onto.

## 1. If the record is stale or absent, don't guess

`result=skipped`, or an empty `latest=`, means "unknown," not "current." There is no cheap
check-only command today — `install.sh` only resolves the latest tag on the way to actually
installing. If the user wants a real answer right now rather than waiting for the next
SessionStart, the honest move is to proceed straight to step 3: `CONTEXT_GURU_UPGRADE=1 install.sh`
either upgrades or reports `result=present` (already current) — either way you learn the truth, and
a no-op run costs one redirect fetch.

## 2. Report honestly

- Up to date: say so plainly. Do not claim it if the last check `result=` was `skipped` — that
  means "unknown," not "current."
- A newer release exists: name both versions and that the download is on the order of 30 MB.
- `answer=auto`: say a matching upgrade is expected to happen automatically in the background and
  take effect at the proxy's next start — this skill can still run it now on request.

## 3. If the user wants to update now

Run, verbatim:

```
CONTEXT_GURU_UPGRADE=1 "${CLAUDE_PLUGIN_ROOT}/scripts/install.sh"
```

This is the same checksum-verified download path `/context-guru:install` uses for a first install —
nothing here reimplements it. Report `result=installed`/`checksum=`/any `reason=` exactly as that
skill does; a failure there is a hard stop, not something to retry silently.

On success, restart the proxy so the new binary is live now rather than at the next session:

```
"${CLAUDE_PLUGIN_ROOT}/scripts/start-proxy.sh"
```

`start-proxy.sh` already compares the running proxy's fingerprint (which now includes the binary
version) against what should be running, and restarts it itself when they differ — you do not need
to stop anything by hand.

## 4. Record the answer, so the next session-start notice behaves

- Updated now, or the user says update automatically from now on:
  `"${CLAUDE_PLUGIN_ROOT}/scripts/settings.py" update-check answer --answer always`
- Not this release: `"${CLAUDE_PLUGIN_ROOT}/scripts/settings.py" update-check answer --answer skip --version '<the latest tag>'`
  — mutes only that tag; a later release still asks.
- Never ask again, on this machine: `"${CLAUDE_PLUGIN_ROOT}/scripts/settings.py" update-check answer --answer never`

A silent or absent choice from the user is a **no** — record it as `skip`, never as `always`.
