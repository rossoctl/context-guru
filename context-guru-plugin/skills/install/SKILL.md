---
name: install
description: Install a local context-guru proxy and route this project's Claude Code sessions through it, so long sessions stop paying to re-create the prompt cache. Use when the user asks to install, set up, enable, try or start context-guru, or to route Claude Code through it. Accepts --global to route every project on the machine instead of just this one.
---

# Install context-guru for Claude Code

Install the proxy binary, then add **one** key — `env.ANTHROPIC_BASE_URL` — to a settings file
so this project's sessions go through it.

Your job here is the part a shell script does badly: choosing the right file, merging into
settings the user already depends on, noticing a base URL that is already set, and verifying
the result. The deterministic steps are scripts in `${CLAUDE_PLUGIN_ROOT}/scripts/`. Run them;
do not reimplement them inline.

## What the user is agreeing to

Say this plainly before touching anything, because it is the part that matters and it is short:

- Every Claude Code API request in the chosen scope will go through a **local** proxy on
  `127.0.0.1`. Nothing is sent anywhere else, and the proxy forwards to Anthropic itself.
- **No API key is needed.** Setting `ANTHROPIC_BASE_URL` without a credential variable keeps
  their claude.ai login working — a Pro/Max subscription continues to apply, with their usage
  limits and billing unchanged. (On a subscription the saving lands in usage limits rather than
  dollars, so `/context-guru:status` cost figures are list-price estimates, not their bill.)
- The default preset is `cache`: the prompt-cache split and nothing else. No content dropped,
  no markers, no extra tool in their requests, no model calls.
- **If the proxy is down, requests in the routed scope do not fail cleanly — they HANG.** With
  routing configured and nothing listening, a prompt produces no output and no error on either
  stream, indefinitely. That is the state after a crash, a reboot, or an idle-exit, and it is the
  whole risk of routing at all; it is why the default scope is this project rather than the machine.
  The plugin installs a `UserPromptSubmit` hook that detects it, tries to restart the proxy, and
  otherwise prints what to do — a hook rather than a skill because invoking a skill needs a model
  call, which is the thing that is broken.
- `/context-guru:uninstall` removes the key and stops the proxy.

## Steps

### 1. Install the binary

```bash
"${CLAUDE_PLUGIN_ROOT}/scripts/install.sh"
```

It prints `key=value` lines. Read them rather than guessing:

- `result=present` — already installed, nothing downloaded. Fine; continue.
- `result=installed` — downloaded and verified. If `on_path=false`, tell the user to add the
  directory to their `PATH` and that the session hook cannot find the proxy until they do.
- `result=error reason=no_release_found` — no published release yet for this repo. Say so, and
  offer the source build (`make build-static`, needs Go 1.26 but no C toolchain). Do not pretend
  it worked.
- `result=error reason=download_failed` — the release tag exists but carries no asset for this
  platform, which is what a repo looks like before its first build is attached. The script tries a
  `go install` fallback first and reports `fallback=go_install`; if that is in the output and the
  result is still an error, there was no toolchain either.
- `reason=checksum_mismatch`, `checksum_unavailable`, `checksum_absent` — **stop, and do not
  install.** All three mean the download could not be verified against the release's
  `checksums.txt`. There is no signature anywhere yet, so this is the only integrity check in the
  path, and the binary in question is about to handle all of the user's LLM traffic and hold their
  API key. `CONTEXT_GURU_INSECURE=1` overrides it and you must not set it on the user's behalf.
- `checksum=SKIPPED_BY_CONTEXT_GURU_INSECURE` in the output — the user set that themselves. Say
  plainly that an unverified binary was installed.
- **Already installed?** The script reports `result=present` with the `version=`, and does not
  replace it. To upgrade, re-run with `CONTEXT_GURU_UPGRADE=1`; to pin a version, set
  `CONTEXT_GURU_VERSION=vX.Y.Z`. If `version=unknown`, the installed binary predates `--version`
  and an upgrade is worth offering.

### 2. Choose the scope

Default to **this project only**. Ask before doing anything wider, and give them the real
trade-off in one line each:

| Scope | File | If the proxy is down |
|---|---|---|
| **This project (default)** | `.claude/settings.local.json` | only this project breaks; the file is gitignored |
| This project, whole team | `.claude/settings.json` | breaks for everyone who clones the repo |
| Every project (`--global`) | `~/.claude/settings.json` | **every Claude Code session on the machine breaks** |

If the user passed `--global`, use the third and confirm once that they mean it, naming the
blast radius. `env` blocks merge per key across scopes, so a user-scope install is not clobbered
by a project that ships its own `env` block.

### 3. Look before you write

```bash
python3 "${CLAUDE_PLUGIN_ROOT}/scripts/settings.py" show --file <target>
```

If `base_url` is already set to something that is not our port, **stop and ask.** It may be
their company gateway, a benchmark endpoint, or another proxy — replacing it silently would
break their setup while looking like success. Offer: keep theirs (abandon the install), or
replace it (and tell them the old value, so they can put it back).

### 4. Write the one key

The port comes from the plugin's configuration (`CLAUDE_PLUGIN_OPTION_PORT`, default `8787`).
The URL must end in `/anthropic` — that is the path the proxy serves the Anthropic dialect on.

```bash
python3 "${CLAUDE_PLUGIN_ROOT}/scripts/settings.py" add \
  --file <target> --url "http://127.0.0.1:${CLAUDE_PLUGIN_OPTION_PORT:-8787}/anthropic"
```

- `result=added` — report the `backup=` path to the user. That is their undo.
- `result=conflict` — you skipped step 3, or the file changed. Go back and ask; only pass
  `--force` once the user has said to replace that specific value. When they do, the replaced
  value is recorded and `/context-guru:uninstall` puts it back — say so, because "we will take
  over your gateway" is much easier to agree to when it is reversible.
- `result=repointed` — the file already held a context-guru URL on a different port (the user
  changed the configured port). Moved, with the previous value reported. Not a conflict.
- `result=error reason=unparseable_json` — their settings file is already broken. Do not
  rewrite it. Tell them where and let them fix it.

### 5. Start it and prove it works

```bash
"${CLAUDE_PLUGIN_ROOT}/scripts/start-proxy.sh"
```

The script is idempotent and gated: it starts the proxy only if nothing answers `/healthz`, and
does nothing at all unless `ANTHROPIC_BASE_URL` names our port. In this session that variable is
**not yet set** — the settings change applies to the *next* session — so verify by hand:

```bash
curl -fsS "http://127.0.0.1:${CLAUDE_PLUGIN_OPTION_PORT:-8787}/healthz"
```

If nothing answers, start it in the foreground of a background shell and read the log rather
than declaring victory:

```bash
context-guru-proxy \
  --listen "127.0.0.1:${CLAUDE_PLUGIN_OPTION_PORT:-8787}" \
  --preset "${CLAUDE_PLUGIN_OPTION_PRESET:-cache}" \
  --idle-exit="${CLAUDE_PLUGIN_OPTION_IDLE_EXIT:-24h}"
```

Pass the port as `--listen`, not through `LISTEN_ADDR`: the port has to be visible in the process
command line, or nothing — including `/context-guru:uninstall` — can identify this proxy among
others.

A `--idle-exit` below the store's floor (~5h34m at the default TTL) is **refused at startup** on
purpose — exiting clears in-memory cache state. If they want a shorter one, that is a
`store.ttl_seconds` conversation, not a flag to force.

### 6. Tell them what happens next

- The setting takes effect in a **new session** — this one is already running with the old
  environment. Say so explicitly; otherwise the natural next question is "why is `/status`
  showing nothing?"
- From then on the plugin's `SessionStart` hook starts the proxy automatically if it is not
  running, in the projects that are routed and nowhere else.
- The proxy exits by itself after `--idle-exit` of no use, so nothing is left running.
- Dashboard: `http://127.0.0.1:<port>/dashboard/` — the four billed token tiers are where the
  cache effect is visible.
- `/context-guru:status` for the numbers, `/context-guru:uninstall` to undo.

## Do not

- Do not add any other key. Not `ANTHROPIC_API_KEY`, not `ANTHROPIC_AUTH_TOKEN` — a credential
  variable is what would take them OFF their subscription billing.
- Do not edit a settings file without the backup step, and do not hand-edit JSON: use the
  script, which replaces the file atomically.
- Do not put the base URL in `.mcp.json`, an env file, or a shell rc. One key, one file.
- Do not claim it is working because the install steps returned 0. `/healthz` answering is the
  claim; anything else is a guess.
