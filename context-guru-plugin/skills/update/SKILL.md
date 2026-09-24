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

**The block above is a pure file read, and it is history, not news.** `answer=`/`skipped=`/
`latest=`/`installed=` are only ever as fresh as the last SessionStart check — which could be from
minutes ago, or from a session that ran long before this one. **The user invoking this skill is
asking a direct question, and a cached answer is not a substitute for checking.** Always continue
to step 1 and get a live answer; never report the block above as if it were current.

This skill does not touch routing, does not start a proxy, and does not ask for the
traffic-interception consent `/context-guru:install` asks for — it only replaces the binary on
`PATH` and, when you do that, restarts the proxy onto it. If the binary is missing entirely (not
merely old), say so and point at `/context-guru:install` instead; this skill has nothing to install
onto.

## 1. Always get a live answer first

```
"${CLAUDE_PLUGIN_ROOT}/scripts/install.sh" --check-latest
```

Read-only: no download, no install, no proxy restart — it only resolves the latest release tag and
reports `installed_version=`/`latest_version=`/`update_available=true|false|unknown`. This is what
makes "check for updates but don't install one" an actual choice you can honour, rather than the
`CONTEXT_GURU_UPGRADE=1` path below, which installs whatever it finds.

`update_available=unknown` means the check itself failed (offline, rate-limited) — say that
plainly. It is not the same claim as "up to date," and reporting it that way is the single most
misleading thing this skill can do.

## 2. Report honestly

- `update_available=false`: up to date, name the version.
- `update_available=true`: name both versions and that the download is on the order of 30 MB.
- `update_available=unknown`: say the check failed and why (`reason=`), and that you don't know
  either way — never fall back to the cached `latest=` from the block above as if it answered this.
- `answer=auto` (from the cached block): mention a matching upgrade is expected to happen
  automatically in the background and take effect at the proxy's next start — this skill can still
  run it now on request.

## 3. Only install on an explicit "yes"

If — and only if — the user says to update now, run, verbatim:

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

**If the user asked you to check but explicitly said not to install, stop after step 2.** Checking
is not a decision, and running this step without one turns "tell me what's out there" into an
upgrade the user did not ask for.

## 4. Record the answer — only when the user actually gave one

Never run any of these unless the user said one of these three things out loud, in this
conversation. Merely running step 1, or the user asking "did you check for updates," is **not** an
answer — it is a question, and recording a `skip` for a question the user never answered is exactly
the confusion this rule exists to prevent.

- The user said update now, or update automatically from now on:
  `"${CLAUDE_PLUGIN_ROOT}/scripts/settings.py" update-check answer --answer always`
- The user said not this release (or said nothing when directly asked whether to update):
  `"${CLAUDE_PLUGIN_ROOT}/scripts/settings.py" update-check answer --answer skip --version '<the latest tag>'`
  — mutes only that tag; a later release still asks.
- The user said never ask again, on this machine:
  `"${CLAUDE_PLUGIN_ROOT}/scripts/settings.py" update-check answer --answer never`

A silent or absent answer to a question you actually asked is a **no** — record it as `skip`. A
question you never asked has no answer to record at all.
