---
name: update
description: Check for a newer context-guru-proxy binary and install it if you want. The plugin itself updates through the marketplace.
disable-model-invocation: true
---

# Update the context-guru proxy binary

<!-- Deliberately NO `allowed-tools:` here, for the same reason install/SKILL.md has none:
     replacing the binary that carries every one of the user's model requests is at least as
     consequential as the traffic-interception command that skill gates, and this file must not
     grant itself the permission the classifier exists to ask about. -->

!`"${CLAUDE_PLUGIN_ROOT}/scripts/settings.py" update-check show`

**The block above is a pure file read, and it is history, not news.** `answer=`/`skipped=`/
`notified=`/`latest=`/`installed=` are only ever as fresh as the last SessionStart check — which
could be from minutes ago, or from a session that ran long before this one. **The user invoking
this skill is asking a direct question, and a cached answer is not a substitute for checking.**
Always continue to step 1 and get a live answer; never report the block above as if it were
current.

`skipped=` and `notified=` are NOT the same claim. `skipped=<tag>` means the user explicitly
declined that tag. `notified=<tag>` only means a SessionStart hook once printed a note about it —
the note may never have reached the user at all (its output competes with whatever they actually
asked that session, and a hook cannot force a question). If `notified=` names a tag but `skipped=`
does not, say the release was surfaced but never actually answered — never describe it as declined.

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

## 3. Record the answer first — only when the user actually gave one

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

Do this **before** step 4, and regardless of what step 4 does next. "Install this release" and
"remember what to do about future releases" are two different things the user can answer
independently — a failed download in step 4 is not a reason to have skipped this one.

**Report the answer from the store, not from what you intended to write.** Each of the three
commands above prints `result=recorded answer=…`; read that line back (or re-run
`update-check show`) and tell the user what is actually recorded. Never assert that a preference
was saved without having seen the store confirm it — that gap is exactly how a broken write ends
up reported as a success.

## 4. Only install on an explicit "yes"

If — and only if — the user says to update now, run, verbatim:

```
CONTEXT_GURU_UPGRADE=1 "${CLAUDE_PLUGIN_ROOT}/scripts/install.sh"
```

This is the same checksum-verified download path `/context-guru:install` uses for a first install —
nothing here reimplements it. Report `result=installed`/`checksum=`/any `reason=` exactly as that
skill does; a failure there is a hard stop on the install — not on step 3, which has already run and
whose recorded answer stands regardless.

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
