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

**Be decisive. This should feel like one command, not an interview.** The user asked for an install —
run the steps, then report what happened in a handful of lines. Do not narrate each step before
taking it, do not explain what a proxy is, and do not ask permission for the default path:
project-local routing is already the safe choice, which is why it is the default.

There are exactly three things worth stopping for, and in all three continuing would damage
something:

1. a checksum that could not be verified — never install anyway;
2. `--global`, which puts every session on the machine behind the proxy — confirm once, naming that;
3. a base URL already set to somebody else's endpoint — see step 3, where the usual answer is to
   chain behind it rather than to ask.

Everything else: act, then say what you did.

## What to say before you start

Three lines, not an essay. The user asked for an install; deliver one, and let them ask for detail.

- Routes this project's Claude Code requests through a **local** proxy (`127.0.0.1`), which forwards
  to Anthropic. **No API key added** — a Pro/Max login keeps working unchanged.
- **The real risk: if the proxy is down, requests HANG** rather than failing — no output, no error.
  A `UserPromptSubmit` hook detects that and restarts it. This is why the default scope is one
  project, not the machine.
- `/context-guru:uninstall` reverses it, restoring any base URL it replaced.

Fuller detail — preset behaviour, subscription vs metered billing, scope trade-offs — is in
`docs/how-to/install-plugin.md`. Point at it; do not recite it.

## Do not investigate the user's machine

Bounded on purpose, because an unbounded version of this step got denied as
`[Credential Exploration]` by Claude Code's own auto-mode classifier on the first real install:

- **Never enumerate, print or test credential variables** — not `ANTHROPIC_API_KEY`,
  `ANTHROPIC_AUTH_TOKEN`, `CLAUDE_CODE_OAUTH_TOKEN`, `AWS_*`, nor any `env | grep` over them. This
  plugin does not read credentials and must not appear to. A proxy plugin sweeping for API keys is
  indistinguishable from the thing everyone is right to fear.
- **Do not profile other processes** — no `ps aux`, `ss`, `lsof` or port scanning to identify what
  else is running. If something else holds the port, the scripts report it; that is enough.
- The only environment fact you need is whether `$ANTHROPIC_BASE_URL` already names our port, and
  `echo "${ANTHROPIC_BASE_URL:-unset}"` answers it.

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
  `go install` fallback first and reports `fallback=go_install_attempted` (printed before the
  attempt, so read it with `result=`, not as proof the build worked); if that is in the output and the
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

**Also check the environment, not only the file.** On a hosted or containerised agent the base URL
is often set in the process environment rather than in any settings file, so `show` reports
`exists=false` while the session is already routed elsewhere:

```bash
echo "ANTHROPIC_BASE_URL=${ANTHROPIC_BASE_URL:-unset}"
```

If that names a port other than ours, **chain rather than replace, and just do it** — run our proxy
in front of theirs so their gateway still handles auth and upstream routing. On a hosted agent this
is the normal shape rather than an anomaly to escalate; one line saying what you are chaining behind
is the right amount of ceremony:

```bash
ANTHROPIC_UPSTREAM="<their base URL>" "${CLAUDE_PLUGIN_ROOT}/scripts/start-proxy.sh"
```

`ANTHROPIC_UPSTREAM` is the proxy's own environment fallback for `--anthropic-upstream`, and the
caller's `Authorization` / `x-api-key` passes straight through to it — so their gateway still
authenticates. For it to survive into later sessions the variable has to be in the settings `env`
block beside our key, since that is what the SessionStart hook inherits:

```json
{"env": {"ANTHROPIC_BASE_URL": "http://127.0.0.1:8787/anthropic",
         "ANTHROPIC_UPSTREAM": "http://127.0.0.1:<their port>"}}
```

`settings.py` writes only our one key, so add that second key by hand and say that you did.

Say what you are doing and why in one line, then continue. Replacing a platform-provided gateway
outright will usually break that agent's authentication, so do not offer it as the default.

### 4. START THE PROXY FIRST, before writing any settings

**This order is not a preference, and getting it wrong breaks the session doing the install.**

Claude Code picks up a settings `env` change **while the session is running** — it does not wait for
a restart. Observed, in a fresh session driving this very skill: the key was written, the proxy had
not been started yet, and the session's next API call went to `127.0.0.1:8787` and died with
`API Error: Connection refused`. It never reached the step that starts the proxy. That left the
project routed with nothing listening — the hang state this whole design exists to avoid, produced
by the installer itself.

So: proxy up and answering `/healthz` first, settings second. Then the instant routing takes effect,
something is already there.

```bash
"${CLAUDE_PLUGIN_ROOT}/scripts/start-proxy.sh"
```

This hook self-gates on `$ANTHROPIC_BASE_URL` naming our port, which is not yet true — so pass the
routed URL for this one manual invocation:

```bash
ANTHROPIC_BASE_URL="http://127.0.0.1:${CLAUDE_PLUGIN_OPTION_PORT:-8787}/anthropic" \
  "${CLAUDE_PLUGIN_ROOT}/scripts/start-proxy.sh"
```

Add `ANTHROPIC_UPSTREAM=<their gateway>` here too if step 3 found one to chain behind. Confirm it is
actually up before continuing — `proxy up on 127.0.0.1:<port>` in the output, or:

```bash
curl -fsS "http://127.0.0.1:${CLAUDE_PLUGIN_OPTION_PORT:-8787}/healthz"
```

If it did not come up, **stop and do not write the settings key.** An unrouted project with no proxy
is a working project; a routed one with no proxy is a broken one.

### 5. Write the one key

The port comes from the plugin's configuration (`CLAUDE_PLUGIN_OPTION_PORT`, default `8787`).
The URL must end in `/anthropic` — that is the path the proxy serves the Anthropic dialect on.

```bash
python3 "${CLAUDE_PLUGIN_ROOT}/scripts/settings.py" add \
  --file <target> --url "http://127.0.0.1:${CLAUDE_PLUGIN_OPTION_PORT:-8787}/anthropic"
```

If step 3 found a gateway to chain behind, add `--upstream <their base URL>` to that same command.
Do **not** hand-edit the file to add it: one atomic write, one backup, and uninstall removes only an
upstream it recorded writing. Skipping it leaves chaining working *only* until the running proxy
idles out — the next session's hook would start one aimed at `api.anthropic.com`.

- `result=added` — report the `backup=` path to the user. That is their undo.
- `result=conflict` — you skipped step 3, or the file changed. Go back and ask; only pass
  `--force` once the user has said to replace that specific value. When they do, the replaced
  value is recorded and `/context-guru:uninstall` puts it back — say so, because "we will take
  over your gateway" is much easier to agree to when it is reversible.
- `result=repointed` — the file already held a context-guru URL on a different port (the user
  changed the configured port). Moved, with the previous value reported. Not a conflict.
- `result=error reason=unparseable_json` — their settings file is already broken. Do not
  rewrite it. Tell them where and let them fix it.

### 6. Prove it, and only then say it worked

The proxy went up in step 4, so this is a re-check after the routing change rather than a first
start — confirm rather than assume:

```bash
curl -fsS "http://127.0.0.1:${CLAUDE_PLUGIN_OPTION_PORT:-8787}/healthz"
```

`start-proxy.sh` is idempotent and gated: it starts the proxy only if nothing answers `/healthz`, and
does nothing at all unless `ANTHROPIC_BASE_URL` names our port — so re-running it is free.

If nothing answers, **say so and offer to remove the routing key**, because a routed project with no
proxy is worse than an unrouted one. To diagnose, start it in the foreground of a background shell
and read the log rather than declaring victory:

```bash
context-guru-proxy \
  --listen "127.0.0.1:${CLAUDE_PLUGIN_OPTION_PORT:-8787}" \
  --preset "${CLAUDE_PLUGIN_OPTION_PRESET:-cache}" \
  --idle-exit="${CLAUDE_PLUGIN_OPTION_IDLE_EXIT:-24h}" \
  --dashboard \
  --dashboard-db "${XDG_STATE_HOME:-$HOME/.local/state}/context-guru/dashboard-${CLAUDE_PLUGIN_OPTION_PORT:-8787}.db"
```

**Both dashboard flags are required, and leaving them off is not cosmetic.** `--dashboard`
defaults to false, so a proxy started without it serves a 404 at `/dashboard/` — the URL step 6
below tells the user to open. And because `start-proxy.sh` is idempotent on `/healthz`, the
session hook will never replace this hand-started proxy: with `--idle-exit 24h` the user's
dashboard stays broken for a day with nothing to connect it to. `--dashboard-db` must be set
because its default is `./context-guru-dashboard.db`, i.e. a database dropped in the user's
repository.

Pass the port as `--listen`, not through `LISTEN_ADDR`: the port has to be visible in the process
command line, or nothing — including `/context-guru:uninstall` — can identify this proxy among
others.

A `--idle-exit` below the store's floor (~5h34m at the default TTL) is **refused at startup** on
purpose — exiting clears in-memory cache state. If they want a shorter one, that is a
`store.ttl_seconds` conversation, not a flag to force.

### 6. Tell them what happens next

- **Do not tell them it only takes effect next session.** That was this skill's claim and it is
  wrong: Claude Code picks the `env` change up live, which is exactly why step 4 starts the proxy
  first. What IS true is that this session began before the proxy existed, so `/context-guru:status`
  may have nothing to show yet — say that instead, and that a new session is the clean way to see it.
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
