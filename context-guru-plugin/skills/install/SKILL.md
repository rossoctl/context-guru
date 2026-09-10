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
3. a base URL already set to somebody else's endpoint — see step 4, where the usual answer is to
   chain behind it rather than to ask.

Everything else: act, then say what you did.

## What to say before you start

Three lines, not an essay. The user asked for an install; deliver one, and let them ask for detail.

- Routes this project's Claude Code requests through a **local** proxy (`127.0.0.1`), which forwards
  to Anthropic. **No API key added** — a Pro/Max login keeps working unchanged.
- **The real risk: if the proxy is down, requests HANG** rather than failing — no output, no error.
  A `UserPromptSubmit` hook detects that and restarts it. This is why the default scope is one
  project, not the machine.
- `/context-guru:uninstall` reverses it, restoring any base URL it replaced. If the routing
  itself is what breaks, a skill cannot run — so the install also drops a plain-sh escape hatch
  outside the plugin, and step 6 tells them where it is.

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
- `result=installed` — downloaded and verified. If `on_path=false`, note the `path=` value: you will
  pass it as `--bin` in step 6 so the session hook can find the proxy without a `PATH` change (see
  there for why telling them to edit their profile is not sufficient on its own).
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

### 2. Find out which port and preset are actually configured

**You cannot read `$CLAUDE_PLUGIN_OPTION_PORT` here, and a shell default silently gives you the wrong
answer.** Claude Code puts those variables into HOOK environments only, never into a Bash tool call —
so `${CLAUDE_PLUGIN_OPTION_PORT:-8787}` in a command always expands to 8787, whatever the user
configured. That was a real defect, not a nicety: the routing key named 8787 while every later hook read
the configured port and self-gated on it, so the hooks saw an unrouted project, did nothing, and the one
running proxy had no auto-restart behind it. Once it idle-exited, nothing brought it back, silently.

The values are on disk, so read them:

```bash
"${CLAUDE_PLUGIN_ROOT}/scripts/settings.py" config
```

- `option_port=…` / `option_preset=…` / `option_idle_exit=…` / `option_upstream=…` — use these
- `source=(none)` — nothing configured; the `plugin.json` defaults apply (port 8787, preset `cache`)
- **an option with no line of its own is unconfigured**, whatever `source=` says. Only keys the user
  actually set are printed, so a partial config — the port set and the preset never touched, say —
  reports a real `source=` and simply omits `option_preset=`. Take the `plugin.json` default for each
  missing option individually; do not read one present option as meaning the rest are set, and do not
  invent a value because `source=` was not `(none)`.

Carry the port through **every** later step explicitly: `--port` when starting the proxy, and the same
number in the URL you write. If it turns out to be anything other than 8787, say so in your summary —
a non-default port is the kind of thing a user forgets they set.

### 3. Choose the scope

Default to **this project only**. Ask before doing anything wider, and give them the real
trade-off in one line each:

| Scope | File | If the proxy is down |
|---|---|---|
| **This project (default)** | `.claude/settings.local.json` | only this project breaks; the file is gitignored |
| This project, whole team | `.claude/settings.json` | breaks for everyone who clones the repo |
| Every project (`--global`) | `~/.claude/settings.json` | **every Claude Code session on the machine breaks** |

Project scope is the default because of that third row, and the asymmetry is worth being concrete
about: a project-scope mistake costs the user one project, while the same mistake machine-wide takes
out every session they could use to fix it — including every project that has nothing to do with
context-guru. Writing the project's own `.claude/settings.local.json` is also why overriding a
machine-wide base URL is safe here: `env` blocks merge per key, most specific first, so the project
file wins for this project and changes nothing anywhere else.

**The script enforces this; it is not left to you.** `settings.py add` refuses the machine-wide file
outright and exits 2 with `reason=user_scope_needs_flag` unless it is passed `--user-scope`. That
refusal used to live only in this paragraph, and a default that exists only in a prompt is not a
guardrail — it can be skipped or read differently, and what it guards against is a machine-wide
lockout. So if the user passed `--global`: confirm once, naming the blast radius, and only then add
`--user-scope` to the step 6 command. Never add it to satisfy an error you did not expect — an
unexpected `user_scope_needs_flag` means you are about to write the wrong file.

Removal is not gated, on purpose: `/context-guru:uninstall` has to be able to clean every scope.

### 4. Look before you write

```bash
"${CLAUDE_PLUGIN_ROOT}/scripts/settings.py" show --file <target>
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

The caller's `Authorization` / `x-api-key` passes straight through to that upstream, which is what
lets their gateway keep authenticating. Two places have to know about it, for different reasons:

* **now**, twice as an explicit `--upstream` argument: once when starting the proxy in step 5 and once
  when writing the settings in step 6. Not via the plugin option and not via an env prefix —
  `CLAUDE_PLUGIN_OPTION_*` does not reach a Bash tool call, and an env prefix cannot be covered by a
  permission rule;
* **later**, so every future session's hook chains too — which means the variable has to be in the
  settings `env` block beside our key, because that block is what the hook inherits:

```json
{"env": {"ANTHROPIC_BASE_URL": "http://127.0.0.1:8787/anthropic",
         "ANTHROPIC_UPSTREAM": "http://127.0.0.1:<their port>"}}
```

`settings.py` writes only our one key, so add that second key by hand and say that you did.

Say what you are doing and why in one line, then continue. Replacing a platform-provided gateway
outright will usually break that agent's authentication, so do not offer it as the default.

### 5. START THE PROXY FIRST, before writing any settings

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

The script self-gates on `$ANTHROPIC_BASE_URL` naming our port, which is not yet true at this point,
so ask it to start anyway:

```bash
"${CLAUDE_PLUGIN_ROOT}/scripts/start-proxy.sh" --unrouted
```

Pass the other facts you already have as arguments too — the port from step 2, the gateway from step 4, and the
binary path from step 1 if it reported `on_path=false`:

```bash
"${CLAUDE_PLUGIN_ROOT}/scripts/start-proxy.sh" --unrouted \
  --upstream "<their base URL>" --bin "<the path= from step 1>"
```

**`--bin` is not optional when `on_path=false`.** Without it the script resolves the binary by name,
finds nothing, and reports "the proxy binary is not on PATH" — and the last agent to hit that
improvised, symlinking the binary into a directory under the plugin cache that happened to be on
`PATH`. It worked, and it would have broken silently at the next plugin update or pod restart, for a
reason nobody would connect to a symlink made days earlier. You already have the path; pass it.

**Everything goes in as arguments, and nothing as an environment prefix.** Two reasons, both learned
the hard way on a hosted agent:

* Bash permission rules match by command PREFIX, so `FOO=1 /path/to/start-proxy.sh` cannot be covered
  by any rule naming this script. An env-prefixed command is one the user cannot approve.
* **`CLAUDE_PLUGIN_OPTION_*` is NOT in the environment of a Bash tool call**, even when the option is
  configured. Verified: the option was set to the pod's gateway in user settings and the variable was
  absent from the shell. Plugin options reach HOOK environments; they do not reach a script this skill
  runs. So never assume the script can read the configured upstream — pass it.

The flag is `--unrouted`, not `--force`. It was `--force` for one round and got denied for its name —
`[Safety Bypass Flag]`, which is a fair reading. Nothing is being bypassed: the script's gate asks "is
this project routed to us?" and during an install the honest answer is "not yet, that is the next
step". Do not reintroduce `--force`, even though it still works.

**Expect this to be denied under auto mode, and do not try to get around it.** Observed twice on a
hosted agent:

```
Denied by auto mode classifier ∙ [Traffic Redirection] Starting a third-party plugin's local proxy
that intercepts and forwards the agent's own Anthropic API traffic ... reroutes model traffic through
unvetted code without the user having named or reviewed that specific interception.
```

That is the correct outcome, not a bug: installing a plugin by name is not the same as consenting to
have your model traffic intercepted, and an agent should not make that call for its user. When it
happens, stop, report exactly what is and is not done, and hand the user their three options:

1. approve the prompt (interactive sessions get one);
2. run this one command themselves with the `!` prefix — print it ready to paste, with no env
   prefixes so it matches what a rule would allow;
3. add a permission rule, e.g. `Bash(<the scripts dir>/**)`, if they would rather not be asked again.

Do not offer a fourth way, do not reword the command to look less like what it is, and never write the
routing key while the proxy is not up.

**Do not satisfy the gate by prefixing `ANTHROPIC_BASE_URL=…` instead.** That is what this step used
to say, and it was denied outright on a hosted agent:

```
Denied by auto mode classifier ∙ [Traffic Redirection] ... repoints ANTHROPIC_BASE_URL to a local
proxy intercepting all Claude API traffic before forwarding to an unverified upstream
```

A fair call — the command text really did redirect traffic — and it blocked the install at its last
step. It also could not be granted, because Bash permission rules match by command *prefix*, so no
rule naming this script can cover an env-prefixed invocation.

The arguments above are the whole interface. **Do not set `ANTHROPIC_UPSTREAM=`, `CONTEXT_GURU_BIN=` or
`CONTEXT_GURU_FORCE=` in front of this command** — this paragraph used to recommend exactly that, two
paragraphs after prohibiting it, and the recommendation was the denied shape. And do not rely on the
plugin's **Upstream base URL** option being visible here: `CLAUDE_PLUGIN_OPTION_*` reaches hook
environments, and this step is a Bash tool call, so in *this* step the configured option is precisely
what the script cannot read. Setting it is still worth telling the user about — it is what later
sessions' hooks use — but it is not a substitute for `--upstream` now.

Confirm the proxy is actually up before continuing — `proxy up on 127.0.0.1:<port>` in the output, or:

```bash
curl -fsS "http://127.0.0.1:<port>/healthz"
```

If it did not come up, **stop and do not write the settings key.** An unrouted project with no proxy
is a working project; a routed one with no proxy is a broken one.

### 6. Write the one key

The port is the one you discovered in step 2 — not a shell expansion, which would silently be 8787.
The URL must end in `/anthropic` — that is the path the proxy serves the Anthropic dialect on.

```bash
"${CLAUDE_PLUGIN_ROOT}/scripts/settings.py" add \
  --file <target> --url "http://127.0.0.1:<port>/anthropic"
```

If step 4 found a gateway to chain behind, add `--upstream <their base URL>` to that same command.
Do **not** hand-edit the file to add it: one atomic write, one backup, and uninstall removes only an
upstream it recorded writing. Skipping it leaves chaining working *only* until the running proxy
idles out — the next session's hook would start one aimed at `api.anthropic.com`.

**If step 1 reported `on_path=false`, add `--bin <the absolute path it reported>` as well.** Telling
the user to fix their `PATH` is not enough on its own: the `SessionStart` hook resolves the proxy BY
NAME, so until the shell profile is edited the install looks successful, works for this session, and
the auto-restart safety net silently never fires — with a hang as the failure mode it was there to
catch. On a hosted agent it is worse, because the writable directories reset on restart, so a profile
edit does not survive. `--bin` writes the absolute path into the same `env` block the hook inherits,
which fixes it without touching the user's shell at all. Still mention the `PATH` gap, since they will
want `context-guru-proxy` on the command line too.

- `result=added` — report the `backup=` path to the user. That is their undo.
- `reset_hatch=<path>` — **print this path verbatim, on its own line, in your summary.** It is the
  escape hatch, and this is the only moment the user is certain to be able to read it: the failure
  it exists for is "every request through the proxy fails", and in that state no skill can run,
  including `/context-guru:uninstall`. It happened — a colleague's install left him at 401 on every
  call with the documented undo path unavailable for exactly the reason he needed it. Tell him the
  command, not the concept:

  ```
  If Claude ever stops being able to talk after this, run: <the reset_hatch path>
  ```

  It restores every settings file this plugin edited, from a copy taken before the first edit, and
  needs no Claude, no network, no proxy and no plugin. `reset_hatch=unavailable` means the state
  directory could not be written — say so plainly, because then their only undo is the `backup=`
  path above.
- `reset_original=unavailable` — the routing is recorded but the file's original CONTENT is not.
  Rare (an unreadable settings file). Worth one line, since the hatch can then unroute but not
  restore.
- `result=conflict` — you skipped step 4, or the file changed. Go back and ask; only pass
  `--force` once the user has said to replace that specific value. When they do, the replaced
  value is recorded and `/context-guru:uninstall` puts it back — say so, because "we will take
  over your gateway" is much easier to agree to when it is reversible.
- `result=completed` — already routed to this proxy, and one of the other keys was missing or stale;
  `added_keys=` names what was filled in. This is the outcome of re-running the install as a repair
  after an earlier attempt stopped partway, which is the common case on a hosted agent. Report it as a
  success, not as "nothing to do".
- `result=unchanged` — routed, and every key already correct. Genuinely nothing to do.
- `result=repointed` — the file already held a context-guru URL on a different port (the user
  changed the configured port). Moved, with the previous value reported, and the chaining keys carried
  across. Not a conflict.
- `result=error reason=unparseable_json` — their settings file is already broken. Do not
  rewrite it. Tell them where and let them fix it.

### 7. Prove it, and only then say it worked

The proxy went up in step 5, so this is a re-check after the routing change rather than a first
start — confirm rather than assume:

```bash
curl -fsS "http://127.0.0.1:<port>/healthz"
```

`start-proxy.sh` is idempotent and gated: it starts the proxy only if nothing answers `/healthz`, and
does nothing at all unless `ANTHROPIC_BASE_URL` names our port — so re-running it is free.

If nothing answers, **say so and offer to remove the routing key**, because a routed project with no
proxy is worse than an unrouted one. To diagnose, start it in the foreground of a background shell
and read the log rather than declaring victory:

```bash
context-guru-proxy \
  --listen "127.0.0.1:<port>" \
  --preset "<preset>" \
  --idle-exit="<idle-exit>" \
  --dashboard \
  --dashboard-db "${XDG_STATE_HOME:-$HOME/.local/state}/context-guru/dashboard-<port>.db"
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

### 8. Tell them what happens next

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
- The escape hatch from step 6, once more, as the last line of your summary. A user who has to
  find it will be looking at this transcript with a session that cannot answer questions.

## Do not

- Do not add any other key. Not `ANTHROPIC_API_KEY`, not `ANTHROPIC_AUTH_TOKEN` — a credential
  variable is what would take them OFF their subscription billing.
- Do not edit a settings file without the backup step, and do not hand-edit JSON: use the
  script, which replaces the file atomically.
- Do not put the base URL in `.mcp.json`, an env file, or a shell rc. One key, one file.
- Do not claim it is working because the install steps returned 0. `/healthz` answering is the
  claim; anything else is a guess.
